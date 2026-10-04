package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/protocol/responses"
	"github.com/hm2899/grokcli-2api/internal/proxy"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

// textCoalesceMax holds text/reasoning micro-deltas before one Write+Flush.
// First client-visible payload always flushes immediately (TTFT); subsequent
// tiny deltas batch until this size, a tool group, or stream end.
const textCoalesceMax = 512

// runOpenAIResponsesStream is the shared body for streamOpenAIResponses and
// streamOpenAIResponsesContinue. envelopeAlreadyOpen is true when the caller
// already wrote response.created/in_progress (Continue / early-open path).
func runOpenAIResponsesStream(w http.ResponseWriter, r *http.Request, body io.Reader, streamer *responses.LiveStreamer, keepalive time.Duration, maxTools int, envelopeAlreadyOpen bool, toolsRequested bool) (map[string]any, int, error) {
	if streamer == nil {
		return nil, 0, errors.New("responses streamer required")
	}
	if !toolsRequested {
		toolsRequested = streamer.HasPendingTools() || streamer.HasClientPayload()
	}
	keepalive = effectiveResponsesKeepalive(keepalive, toolsRequested)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, 0, errors.New("streaming is not supported by this response writer")
	}
	if maxTools < 0 {
		maxTools = 0
	}

	sw := newSSEWriter(w, flusher, r.Context())
	if !envelopeAlreadyOpen {
		// Early envelope for Codex / Claude Code perceived TTFT.
		if err := sw.WriteStrings(streamer.Start(), true); err != nil {
			return nil, 0, err
		}
	}

	toolGap := outboundToolGapFrom(r.Context())
	toolsEmitted := 0
	kaFrame := responsesKeepaliveFrame()

	// pendingText accumulates non-tool frames across tiny upstream deltas so we
	// do not Flush once per token. Flushed on tools / size / keepalive / end.
	// Stored as a single byte buffer (not []string) to avoid per-tick joins.
	//
	// IMPORTANT: only clear the buffer after full delivery (LastOK). Soft client
	// blips used to discard the whole payload while lastOK=false, which dropped
	// mid-turn text/reasoning and surfaced as intermittent incomplete assistant
	// output. Soft short-writes advance by LastWritten (drop delivered prefix,
	// keep the tail) so retries neither truncate nor duplicate text.
	pendingText := make([]byte, 0, textCoalesceMax*2)
	flushPendingText := func(force bool) error {
		if len(pendingText) == 0 {
			return nil
		}
		attempts := 1
		if force {
			attempts = 3
		}
		var lastErr error
		for i := 0; i < attempts && len(pendingText) > 0; i++ {
			if i > 0 {
				time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			}
			payload := pendingText
			streamCoalesceFlush.Add(1)
			lastErr = sw.WriteBytes(payload, force || sw.SoftGone())
			if lastErr != nil {
				return lastErr
			}
			if sw.LastOK() {
				pendingText = pendingText[:0]
				streamer.AckContentDelivered()
				return nil
			}
			// Soft fail: drop only the bytes that landed; keep the unsent tail.
			if n := sw.LastWritten(); n > 0 {
				if n >= len(pendingText) {
					pendingText = pendingText[:0]
				} else {
					pendingText = append(pendingText[:0], pendingText[n:]...)
				}
				if len(pendingText) == 0 {
					streamer.AckContentDelivered()
					return nil
				}
			}
		}
		// Soft-fail with residual: leave pendingText for a later force drain
		// (tool boundary / idle / Complete) so text is not permanently lost.
		return nil
	}

	// emitFrames groups function_call start…done into one Write+Flush (atomic tool
	// delivery). Soft write failures leave lastOK=false → Requeue + Complete recovery.
	emitFrames := func(frames []string, force bool) error {
		if len(frames) == 0 {
			return nil
		}
		// If any frame is a tool or terminal, flush coalesced text first so
		// envelope order stays: text/reasoning → function_call → completed.
		needImmediate := force
		if !needImmediate {
			for _, frame := range frames {
				if frameNeedsResponsesImmediate(frame) {
					needImmediate = true
					break
				}
			}
		}
		if needImmediate {
			if err := flushPendingText(true); err != nil {
				return err
			}
		} else {
			// Pure text/reasoning: coalesce micro-deltas into one buffer.
			for _, frame := range frames {
				pendingText = append(pendingText, frame...)
			}
			if len(pendingText) >= textCoalesceMax || force {
				return flushPendingText(force)
			}
			return nil
		}

		groups := make([][]string, 0, 4)
		cur := make([]string, 0, 4)
		flushCur := func() {
			if len(cur) == 0 {
				return
			}
			groups = append(groups, cur)
			cur = make([]string, 0, 4)
		}
		for _, frame := range frames {
			isToolStart := frameIsResponsesToolStart(frame)
			isTerminalFrame := frameIsResponsesTerminal(frame)
			if isToolStart {
				flushCur()
			} else if isTerminalFrame && len(cur) > 0 {
				joinedCur := strings.Join(cur, "")
				if strings.Contains(joinedCur, "function_call") || strings.Contains(joinedCur, "response.output") ||
					strings.Contains(joinedCur, "response.created") {
					flushCur()
				}
			}
			cur = append(cur, frame)
		}
		flushCur()

		flushGroup := func(g []string, forceWrite bool) error {
			if len(g) == 0 {
				return nil
			}
			isToolGroup := false
			for _, f := range g {
				if strings.Contains(f, "function_call") && strings.Contains(f, "response.output_item.added") {
					isToolGroup = true
					break
				}
			}
			if isToolGroup && toolGap > 0 && toolsEmitted > 0 {
				if waitToolGap(r.Context(), toolGap) {
					sw.MarkSoftGone()
				}
			}
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				if attempt > 0 {
					// Only back off when a soft fail left the tool unacked.
					time.Sleep(time.Duration(attempt) * 2 * time.Millisecond)
				}
				lastErr = sw.WriteStrings(g, forceWrite || sw.SoftGone())
				if lastErr != nil {
					return lastErr
				}
				if sw.LastOK() {
					joined := strings.Join(g, "")
					if strings.Contains(joined, "function_call") {
						streamer.AckToolsInPayload(joined)
						streamer.NoteToolDelivered()
						if isToolGroup {
							toolsEmitted++
						}
					} else if strings.Contains(joined, "response.output_text") ||
						strings.Contains(joined, "response.reasoning") {
						// Text/reasoning survived Write+Flush — count as delivered.
						streamer.AckContentDelivered()
					}
					if strings.Contains(joined, "response.completed") || strings.Contains(joined, "[DONE]") {
						streamer.AckTerminal()
						streamer.SyncDeliveredFromBuffers()
					}
					return nil
				}
			}
			return lastErr
		}

		var firstHard error
		for _, g := range groups {
			if err := flushGroup(g, force); err != nil {
				if firstHard == nil {
					firstHard = err
				}
			}
		}
		if streamer.HasUnackedTools() {
			streamer.RequeueUnackedTools()
		}
		return firstHard
	}

	var usage map[string]any
	firstTokenMS := 0
	started := time.Now()
	wroteThisTick := false

	err := grok.ReadSSEWithIdle(body, keepalive, func(event grok.Event) error {
		if event.Done {
			return nil
		}
		wroteThisTick = false
		delta, err := proxy.ParseChatDelta(event.Data)
		if err != nil {
			return nil
		}
		if raw, ok := delta.Usage.(map[string]any); ok {
			// Merge partial usage frames (xAI may send incremental then final).
			usage = mergeUsageMaps(usage, raw)
		}
		// Merge reasoning+text+tools from one upstream tick into fewer flushes.
		// Tools always go through emitFrames (atomic groups); text may coalesce.
		if frames := streamer.Reasoning(delta.Reasoning); len(frames) > 0 {
			// First reasoning payload flushes for TTFT; later micro-deltas coalesce.
			if err := emitFrames(frames, firstTokenMS == 0); err != nil {
				return err
			}
			wroteThisTick = true
		}
		if frames := streamer.Text(delta.Content); len(frames) > 0 {
			// First client-visible payload flushes immediately for TTFT; later
			// micro-deltas coalesce until textCoalesceMax / tool / idle.
			forceFirst := firstTokenMS == 0
			if err := emitFrames(frames, forceFirst); err != nil {
				return err
			}
			wroteThisTick = true
		}
		if frames := streamer.ToolDeltas(responsesToolDeltas(delta)); len(frames) > 0 {
			if err := emitFrames(frames, true); err != nil {
				return err
			}
			wroteThisTick = true
		}
		// TTFT only after a successful Write of real client payload (not frames that
		// soft-failed and were requeued). HasClientPayload is true as soon as frames
		// are *produced* — using it alone caused intermittent admin rows with
		// ttft>0 + empty 502 when the write never landed.
		if firstTokenMS == 0 && streamer.PayloadDelivered() {
			firstTokenMS = int(time.Since(started).Milliseconds())
			if firstTokenMS <= 0 {
				firstTokenMS = 1
			}
		}
		// Incomplete tool args: throttle keepalives (not every micro-chunk).
		// Skip if we already wrote real frames this tick — socket is warm.
		if streamer.HasPendingTools() && !wroteThisTick {
			return sw.Keepalive(kaFrame, DefaultKeepaliveInterval, true)
		}
		return nil
	}, func() error {
		// Flush coalesced text on idle so clients are not stuck waiting for size.
		_ = flushPendingText(true)
		select {
		case <-r.Context().Done():
			sw.MarkSoftGone()
			return sw.Keepalive(kaFrame, DefaultKeepaliveInterval, true)
		default:
		}
		return sw.Keepalive(kaFrame, DefaultKeepaliveInterval, false)
	})

	// Drain any coalesced text before terminal Complete. Retry while soft-fail
	// left bytes in the buffer so mid-turn text is not truncated.
	for drain := 0; drain < 3 && len(pendingText) > 0; drain++ {
		_ = flushPendingText(true)
		if sw.LastOK() && len(pendingText) == 0 {
			break
		}
	}
	// Coalesced first payload may only land on drain — capture TTFT then.
	if firstTokenMS == 0 && streamer.PayloadDelivered() {
		firstTokenMS = int(time.Since(started).Milliseconds())
		if firstTokenMS <= 0 {
			firstTokenMS = 1
		}
	}

	clientGone := sw.SoftGone() || errors.Is(err, r.Context().Err()) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isSoftClientWriteError(err)
	hasPayload := streamer.HasClientPayload() || streamer.HasPendingTools() || streamer.HasUnackedTools()
	// Upstream mid-stream drop after client already saw content/tools: soft Complete
	// only. response.failed mid-turn surfaces as Claude/Codex "Server error mid-response".
	upstreamMidError := err != nil && !clientGone
	if upstreamMidError && !hasPayload {
		msg, errType := openAIErrorFromCause(err)
		_ = emitFrames(streamer.Fail(msg, errType), true)
		return fillStreamUsage(usage, streamer), firstTokenMS, err
	}
	respUsage := responsesUsageFromOpenAI(usage)
	// Always try to close the Responses envelope so Codex / Claude Code leave "running".
	if termErr := emitFrames(streamer.Complete(&respUsage), true); termErr != nil && !clientGone && !upstreamMidError {
		return fillStreamUsage(usage, streamer), firstTokenMS, termErr
	}
	// Soft-fail recovery: more Complete rebuilds (tools + completed).
	for attempt := 0; attempt < 4 && streamer.NeedsFinishRetry(); attempt++ {
		_ = emitFrames(streamer.Complete(&respUsage), true)
	}
	// True empty: no produced payload at all (and nothing pending/unacked).
	if !streamer.HasClientPayload() {
		_ = emitFrames(streamer.Fail("empty model output", "server_error"), true)
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		if upstreamMidError && err != nil {
			return fillStreamUsage(usage, streamer), firstTokenMS, err
		}
		return fillStreamUsage(usage, streamer), firstTokenMS, empty
	}
	if streamer.ClientDeliveryOK() {
		if clientGone || upstreamMidError {
			return fillStreamUsage(usage, streamer), firstTokenMS, nil
		}
		return fillStreamUsage(usage, streamer), firstTokenMS, err
	}
	// Recovery exhausted. Distinguish:
	//  1) Half-open tools (emitted, never Ack'd) → real "Tool use interrupted" fail.
	//  2) Payload already delivered (tools Ack'd / content written) but terminal
	//     never Ack'd → soft-close. Emitting response.failed after a real tool/text
	//     delivery is what Claude Code surfaces as intermittent mid-response /
	//     "Tool use interrupted" with admin ok=false tokens=0 ttft>0.
	//  3) Everything else (incomplete-only tools dropped) → empty fail.
	if streamer.HalfOpenTools() {
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		if !streamer.TerminalDelivered() {
			_ = emitFrames(streamer.Fail("empty model output", "server_error"), true)
		}
		if upstreamMidError && err != nil {
			return fillStreamUsage(usage, streamer), firstTokenMS, err
		}
		return fillStreamUsage(usage, streamer), firstTokenMS, empty
	}
	if streamer.PayloadDelivered() {
		// Soft-close: client already has usable output. Do not Fail over it.
		return fillStreamUsage(usage, streamer), firstTokenMS, nil
	}
	empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
	if !streamer.TerminalDelivered() {
		_ = emitFrames(streamer.Fail("empty model output", "server_error"), true)
	}
	if upstreamMidError && err != nil {
		return fillStreamUsage(usage, streamer), firstTokenMS, err
	}
	return fillStreamUsage(usage, streamer), firstTokenMS, empty
}

// fillStreamUsage patches zero usage from LiveStreamer output when the upstream
// omitted the final usage frame (common on soft-close / short tool turns).
// Stamps _estimated_* markers so recordResponsesUsage can surface accurate
// usage_estimated_fields (previously flags were dropped and completion looked "real").
func fillStreamUsage(usage map[string]any, streamer *responses.LiveStreamer) map[string]any {
	hint := 0
	if streamer != nil {
		hint = streamer.EstimateOutputTokens()
	}
	filled, flags := fillMissingUsage(usage, nil, hint)
	if filled == nil {
		filled = map[string]any{}
	}
	if flags.EstimatedCompletion {
		filled["_estimated_completion"] = true
	}
	if flags.EstimatedPrompt {
		filled["_estimated_prompt"] = true
	}
	if flags.EstimatedTotal {
		filled["_estimated_total"] = true
	}
	if flags.Missing {
		filled["_usage_missing"] = true
	}
	if streamer != nil {
		filled["_streamer_output_chars"] = streamer.OutputChars()
		filled["_streamer_estimate"] = hint
	}
	return filled
}
