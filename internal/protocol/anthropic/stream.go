package anthropic

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

type ToolDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type toolState struct {
	id        string
	name      string
	arguments string
	block     int
	started   bool
	stopped   bool
	// clientAcked is true only after the full start+delta+stop group was written
	// to the client. Soft write failures leave started/stopped set but unacked so
	// RequeueUnackedTools can re-emit a complete tool_use instead of leaving
	// Claude Code on a half-open block ("Tool use interrupted").
	clientAcked bool
}

// StreamAssembler converts chat-completion deltas into an Anthropic event
// sequence while preserving dense block indexes and one active tool block.
type StreamAssembler struct {
	messageID string
	model     string
	allowed   []string
	maxTools  int

	started        bool
	nextBlock      int
	textBlock      int
	thinkingBlock  int
	tools          map[int]*toolState
	toolsStarted   int
	sawTool        bool
	toolsRequested bool
	held           []heldDelta
	outputRunes    int
	// pendingClientAcks tracks tool indexes whose frames were just produced by
	// emitReadyTools but not yet confirmed delivered (AckEmittedTools).
	pendingClientAcks []int
	// pendingMessageStart is true after Start produced message_start frames that
	// have not yet been AckEmittedTools'd. Soft write failures leave this set so
	// Start/Finish can re-emit the envelope instead of leaving Claude Code with
	// no message_start (half-open stream → intermittent "Tool use interrupted").
	pendingMessageStart bool
	// messageStartAcked is true only after message_start was written successfully.
	messageStartAcked bool
	// pendingTerminal is true after Finish produced message_delta+message_stop
	// frames that have not yet been AckEmittedTools'd. Soft write of that batch
	// must clear this (via Requeue) so the next Finish re-emits the terminal —
	// otherwise Claude Code hangs / "Tool use interrupted" with tools but no stop.
	pendingTerminal bool
	// terminalEmitted is set only after message_delta+message_stop were Ack'd
	// (successfully written). Soft-write recovery Finish may re-emit tools without
	// duplicating the terminal envelope (Claude Code rejects double message_stop).
	terminalEmitted bool
	// contentDelivered: text/thinking frames survived a successful Write+Flush.
	// Distinct from textBlock/thinkingBlock being open (produced) — soft-fail
	// before Ack must not count as client-visible for TTFT / soft-close.
	contentDelivered bool
}

type heldDelta struct {
	content   string
	reasoning string
}

func NewStreamAssembler(messageID, model string, toolsRequested bool, maxTools int, allowed []string) *StreamAssembler {
	return &StreamAssembler{
		messageID:      messageID,
		model:          model,
		allowed:        append([]string(nil), allowed...),
		maxTools:       maxTools,
		textBlock:      -1,
		thinkingBlock:  -1,
		tools:          make(map[int]*toolState),
		toolsRequested: toolsRequested,
	}
}

func (s *StreamAssembler) Start(inputTokens int) []string {
	// Already delivered to the client — do not re-emit message_start.
	if s.messageStartAcked {
		return nil
	}
	// Already produced unacked frames this cycle; wait for Ack or Requeue.
	if s.pendingMessageStart {
		return nil
	}
	s.started = true
	s.pendingMessageStart = true
	return []string{event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.messageID, "type": "message", "role": "assistant",
			"content": []any{}, "model": s.model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": inputTokens, "output_tokens": 0,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			},
		},
	})}
}

func (s *StreamAssembler) Feed(content, reasoning string, calls []ToolDelta) []string {
	frames := s.Start(0)
	// When tools are declared, keep TEXT held until a tool arrives / Finish.
	// REASONING must stream live so long Claude Code thinking turns keep the
	// SSE pipe warm (idle proxies ~60s otherwise cut the connection).
	if s.toolsRequested && !s.sawTool {
		if content != "" {
			s.held = append(s.held, heldDelta{content: content})
			s.outputRunes += len([]rune(content))
			content = ""
		}
		// reasoning falls through to emitText below (live)
	} else if s.toolsRequested && s.sawTool {
		// After first tool, still emit text/reasoning — the model may include
		// explanations or summaries alongside tool_use blocks.
		// Previously this dropped all content which violated the Anthropic
		// streaming spec where text and tool_use blocks can be interleaved.
	}
	if content != "" || reasoning != "" {
		frames = append(frames, s.emitText(reasoning, content)...)
	}
	if len(calls) == 0 {
		return frames
	}
	frames = append(frames, s.closeThinking()...)
	frames = append(frames, s.closeText()...)
	for _, call := range calls {
		state := s.tools[call.Index]
		if state == nil {
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("toolu_go_%d", call.Index)
			}
			state = &toolState{id: id, block: -1}
			s.tools[call.Index] = state
		}
		if state.stopped {
			continue
		}
		if state.id == "" && call.ID != "" {
			state.id = call.ID
		}
		if call.Name != "" {
			state.name = mergeName(state.name, call.Name)
			state.name = toolcall.CanonicalName(state.name, s.allowed)
		}
		if call.Arguments != "" {
			state.arguments = toolcall.Merge(state.arguments, call.Arguments, state.name)
		}
	}
	frames = append(frames, s.emitReadyTools(false)...)
	return frames
}

// HasClientPayload reports whether any user-visible content was or will be
// emitted: text, thinking, held text, or tool_use. Envelope-only message_start
// is NOT a client payload (empty upstream must still fail).
func (s *StreamAssembler) HasClientPayload() bool {
	if s == nil {
		return false
	}
	// True only when the client has received (or will on Finish release) user-visible
	// output: started tools, text/thinking blocks, held text, or output runes.
	// Incomplete tools that are still pending (name/args held, never started) are NOT
	// payload — they are tracked by HasPendingTools. Counting them (or bare sawTool)
	// as payload was the intermittent ok=true tokens=0 path ("Tool use interrupted").
	if s.toolsStarted > 0 {
		return true
	}
	if s.outputRunes > 0 {
		// outputRunes may be inflated by held text — held text is released on Finish
		// when no tool emits, so it counts as eventual client payload.
		return true
	}
	if s.textBlock >= 0 || s.thinkingBlock >= 0 {
		return true
	}
	if len(s.held) > 0 {
		return true
	}
	for _, state := range s.tools {
		if state != nil && (state.started || state.clientAcked) {
			return true
		}
	}
	return false
}

// toolCapReached is true when the outbound max-tools policy has already emitted
// its full budget. Excess tools are permanently dropped and must not look pending
// (avoids message_stop re-emit loops → Claude Code "Tool use interrupted").
func (s *StreamAssembler) toolCapReached() bool {
	return s != nil && s.maxTools > 0 && s.toolsStarted >= s.maxTools
}

// HasPendingTools reports buffered but not-yet-emitted tool arguments.
// When the max-tools cap is filled, excess tools do not count as pending.
func (s *StreamAssembler) HasPendingTools() bool {
	if s == nil || s.toolCapReached() {
		return false
	}
	for _, state := range s.tools {
		if state == nil || state.started || state.stopped {
			continue
		}
		if state.name != "" || strings.TrimSpace(state.arguments) != "" {
			return true
		}
	}
	return false
}

// HasHeldContent reports text held for tool-only turns (toolsRequested && !sawTool).
// Upstream may still be streaming held text without reasoning — client sees silence
// and reverse proxies cut Claude Code mid-turn. Callers should force SSE keepalive.
func (s *StreamAssembler) HasHeldContent() bool {
	if s == nil {
		return false
	}
	return len(s.held) > 0
}

// OutputRunes returns accumulated text/thinking/tool-arg runes for usage fallback
// when upstream omits the usage frame (soft-close / short tool turns).
func (s *StreamAssembler) OutputRunes() int {
	if s == nil {
		return 0
	}
	return s.outputRunes
}

// EstimateOutputTokens approximates completion tokens (~4 runes/token).
// Returns at least 1 when a client payload exists; 0 when the stream was empty.
func (s *StreamAssembler) EstimateOutputTokens() int {
	if s == nil {
		return 0
	}
	if s.outputRunes > 0 {
		tok := s.outputRunes / 4
		if tok < 1 {
			tok = 1
		}
		return tok
	}
	if s.HasClientPayload() {
		return 1
	}
	return 0
}

// NeedsClientKeepalive is true when the assembler is buffering work the client
// cannot yet see (held text and/or incomplete tools).
func (s *StreamAssembler) NeedsClientKeepalive() bool {
	return s.HasHeldContent() || s.HasPendingTools()
}

// HasUnackedTools reports tools whose start+delta+stop frames were produced but
// not yet AckEmittedTools'd (soft write may have failed mid-group). Also true when
// message_start or message_stop was produced but not acked (envelope recovery).
func (s *StreamAssembler) HasUnackedTools() bool {
	if s == nil {
		return false
	}
	if s.pendingMessageStart || s.pendingTerminal {
		return true
	}
	if len(s.pendingClientAcks) > 0 {
		return true
	}
	for _, state := range s.tools {
		if state != nil && state.started && !state.clientAcked {
			return true
		}
	}
	return false
}

// TerminalDelivered is true only after message_delta+message_stop were Ack'd
// (successfully written to the client). Soft-fail of the terminal batch leaves
// this false so the server can Finish again after Requeue.
func (s *StreamAssembler) TerminalDelivered() bool {
	return s != nil && s.terminalEmitted
}

// PayloadDelivered is true when the client has already received useful output:
// text/thinking that survived Write+Flush (contentDelivered), or at least one
// tool that was client-Ack'd. Unlike HasClientPayload (true as soon as frames
// are produced or held), this only counts content that landed on the wire.
//
// Used after recovery exhaustion so we soft-close a real delivery instead of
// TerminalError("empty model output") when only message_stop soft-failed —
// that false empty is intermittent Claude Code "Tool use interrupted".
func (s *StreamAssembler) PayloadDelivered() bool {
	if s == nil {
		return false
	}
	// Only content that survived Write+Flush (or Ack'd tools). Open block indices
	// alone must NOT count — that set admin ttft_ms=1 on the first thinking frame
	// even when the real model payload came seconds later (or never).
	if s.contentDelivered {
		return true
	}
	for _, state := range s.tools {
		if state != nil && state.clientAcked {
			return true
		}
	}
	return false
}

// AckContentDelivered marks text/thinking frames as successfully written.
// Call after a Write+Flush of non-tool payload (text_delta / thinking_delta).
func (s *StreamAssembler) AckContentDelivered() {
	if s == nil {
		return
	}
	if s.outputRunes > 0 || s.textBlock >= 0 || s.thinkingBlock >= 0 {
		s.contentDelivered = true
	}
}

// HalfOpenTools is true when any tool_use was started (framed) but never client-Ack'd.
func (s *StreamAssembler) HalfOpenTools() bool {
	if s == nil {
		return false
	}
	for _, state := range s.tools {
		if state != nil && state.started && !state.clientAcked {
			return true
		}
	}
	if len(s.pendingClientAcks) > 0 {
		for _, idx := range s.pendingClientAcks {
			state := s.tools[idx]
			if state == nil || (state.started && !state.clientAcked) {
				return true
			}
		}
	}
	return false
}

// ClientDeliveryOK reports a fully closed Anthropic turn safe for ok=true:
// message_stop Ack'd AND no half-open tool_use. Text/thinking-only turns are OK
// once the terminal is Ack'd. Only tools that actually started (frames produced)
// require client Ack — pending incomplete tools that never left the hold buffer
// do not poison a text/thinking delivery.
func (s *StreamAssembler) ClientDeliveryOK() bool {
	if s == nil || !s.terminalEmitted {
		return false
	}
	if s.HasUnackedTools() {
		return false
	}
	anyStarted := false
	anyAcked := false
	for _, state := range s.tools {
		if state == nil {
			continue
		}
		// Only started/acked tools matter. name-only pending tools were never
		// framed; dropping them at Finish is not "Tool use interrupted".
		if state.started || state.clientAcked {
			anyStarted = true
		}
		if state.clientAcked {
			anyAcked = true
		}
	}
	if anyStarted {
		return anyAcked
	}
	return s.HasClientPayload()
}

// NeedsFinishRetry is true when a prior Finish write soft-failed and recovery
// still has work: unacked frames, requeued ready tools, or message_stop never
// Ack'd. Used by the server after emitFrames(Finish) — Requeue clears the
// "unacked" flags, so HasUnackedTools alone misses terminal soft-fails.
//
// Incomplete tools that will never coerce complete must NOT keep retrying after
// terminal — that re-emits message_stop in a loop (Claude Code double-stop /
// "Tool use interrupted").
func (s *StreamAssembler) NeedsFinishRetry() bool {
	if s == nil {
		return false
	}
	if s.HasUnackedTools() {
		return true
	}
	if !s.terminalEmitted {
		// Terminal never landed: retry so envelope can close. Pending incomplete
		// tools alone are enough reason to try Finish once (force-finish path).
		return s.sawTool || s.HasPendingTools() || s.HasClientPayload() || s.started || s.messageStartAcked || s.pendingMessageStart
	}
	// Terminal landed: only retry when a complete tool can still be emitted
	// (soft-fail requeue of a ready tool). Incomplete pending tools stay dropped.
	return s.hasReadyUnemittedTools()
}

// hasReadyUnemittedTools is true when a non-started tool would pass force-finish
// CompleteJSON after coercion — i.e. Finish can still emit a real tool_use group.
// Respects the outbound max-tools cap so capped excess tools do not retry Finish.
func (s *StreamAssembler) hasReadyUnemittedTools() bool {
	if s == nil || s.toolCapReached() {
		return false
	}
	for _, state := range s.tools {
		if state == nil || state.started || state.stopped || state.clientAcked {
			continue
		}
		if state.name == "" {
			continue
		}
		args := toolcall.CoerceCompleteJSON(state.arguments, state.name)
		if toolcall.CompleteJSON(args, state.name) {
			return true
		}
	}
	return false
}

// AckMessageStart marks a successfully written message_start envelope.
func (s *StreamAssembler) AckMessageStart() {
	if s == nil || !s.pendingMessageStart {
		return
	}
	s.messageStartAcked = true
	s.pendingMessageStart = false
}

// AckTerminal marks a successfully written message_delta+message_stop pair.
func (s *StreamAssembler) AckTerminal() {
	if s == nil || !s.pendingTerminal {
		return
	}
	s.terminalEmitted = true
	s.pendingTerminal = false
}

// UnackTerminal rolls back a previously Ack'd terminal so Finish can re-emit
// message_stop AFTER requeued tool_use groups. Emitting tool_use after
// message_stop is what Claude Code reports as "Tool use interrupted".
func (s *StreamAssembler) UnackTerminal() {
	if s == nil {
		return
	}
	s.terminalEmitted = false
	s.pendingTerminal = false
}

// AckFirstPendingTools marks the first n tools from pendingClientAcks as
// client-delivered. Call only after a Write+Flush that contained exactly those
// tool groups in the same order as pendingClientAcks (one batch may hold one
// tool when emitFrames splits on tool start). Prefer AckToolsInPayload when
// writes are split and some groups may soft-fail independently.
//
// Using the full AckEmittedTools after a text/thinking-only flush would wrongly
// mark not-yet-written tools acked → "Tool use interrupted" on soft fail of the
// real tool batch.
func (s *StreamAssembler) AckFirstPendingTools(n int) {
	if s == nil || n <= 0 {
		return
	}
	if n > len(s.pendingClientAcks) {
		n = len(s.pendingClientAcks)
	}
	for i := 0; i < n; i++ {
		if state := s.tools[s.pendingClientAcks[i]]; state != nil {
			state.clientAcked = true
		}
	}
	s.pendingClientAcks = s.pendingClientAcks[n:]
}

// AckToolsInPayload marks started-but-unacked tools whose id appears in the
// successfully written payload. Safe when Finish splits multi-tool frames into
// separate Writes and some soft-fail: FIFO AckFirstPendingTools would otherwise
// Ack the failed tool when a later tool Write succeeds → missing tool_use /
// "Tool use interrupted".
func (s *StreamAssembler) AckToolsInPayload(payload string) {
	if s == nil || payload == "" {
		return
	}
	if !strings.Contains(payload, `"type":"tool_use"`) && !strings.Contains(payload, "tool_use") {
		return
	}
	acked := make(map[int]bool)
	for index, state := range s.tools {
		if state == nil || state.clientAcked || !state.started {
			continue
		}
		matched := false
		if state.id != "" && strings.Contains(payload, state.id) {
			matched = true
		} else if state.id == "" && state.name != "" && strings.Contains(payload, `"name":"`+state.name+`"`) {
			// Weak match: only when a single unacked tool has this name.
			sameName := 0
			for _, other := range s.tools {
				if other != nil && !other.clientAcked && other.started && other.name == state.name {
					sameName++
				}
			}
			if sameName == 1 {
				matched = true
			}
		}
		if !matched {
			continue
		}
		state.clientAcked = true
		acked[index] = true
	}
	if len(acked) == 0 {
		return
	}
	// Drop acked indexes from pendingClientAcks (preserve order of remainder).
	if len(s.pendingClientAcks) == 0 {
		return
	}
	kept := s.pendingClientAcks[:0]
	for _, idx := range s.pendingClientAcks {
		if !acked[idx] {
			kept = append(kept, idx)
		}
	}
	s.pendingClientAcks = kept
}

// AckEmittedTools marks ALL pending tools + message_start + terminal from the
// last emit as successfully written. Prefer AckToolsInPayload / AckMessageStart
// / AckTerminal when a Write batch may not contain every pending frame.
func (s *StreamAssembler) AckEmittedTools() {
	if s == nil {
		return
	}
	s.AckMessageStart()
	s.AckTerminal()
	if n := len(s.pendingClientAcks); n > 0 {
		s.AckFirstPendingTools(n)
	}
}

// RequeueUnackedTools rolls back tools that were framed but never acked so
// Finish/emitReadyTools can re-emit a complete start+delta+stop group. Without
// this, a soft write mid-group leaves Claude Code with content_block_start and
// no stop → "Tool use interrupted".
//
// Block indexes are NOT reclaimed: re-emit assigns a fresh dense index via
// nextBlock. Gaps from a failed write are fine because the client never saw them.
//
// Also clears a pending (unacked) message_start / message_stop so Start/Finish can
// re-emit the envelope when those writes soft-failed.
//
// If any tool is requeued while terminal was already Ack'd, UnackTerminal so
// recovery re-emits tools THEN message_stop (never tool_use after stop).
func (s *StreamAssembler) RequeueUnackedTools() {
	if s == nil {
		return
	}
	if s.pendingMessageStart && !s.messageStartAcked {
		// Client never received message_start; clear pending so Start re-emits.
		s.pendingMessageStart = false
		s.started = false
	}
	// Soft write of terminal batch failed: allow Finish to re-emit message_stop.
	if s.pendingTerminal && !s.terminalEmitted {
		s.pendingTerminal = false
	}
	requeuedTool := false
	for _, state := range s.tools {
		if state == nil || state.clientAcked || !state.started {
			continue
		}
		state.started = false
		state.stopped = false
		state.block = -1
		if s.toolsStarted > 0 {
			s.toolsStarted--
		}
		requeuedTool = true
	}
	s.pendingClientAcks = s.pendingClientAcks[:0]
	// Keep sawTool only when an acked tool exists or a force-finishable tool remains.
	// Incomplete name+args that will never CompleteJSON must not set sawTool —
	// that mislabels empty turns as tool_use and ClientDeliveryOK via payload.
	anyAcked := false
	anyReady := false
	for _, state := range s.tools {
		if state == nil {
			continue
		}
		if state.clientAcked {
			anyAcked = true
		}
		if !state.started && !state.stopped && state.name != "" {
			args := toolcall.CoerceCompleteJSON(state.arguments, state.name)
			if toolcall.CompleteJSON(args, state.name) {
				anyReady = true
			}
		}
	}
	if anyAcked || anyReady {
		s.sawTool = true
	} else if s.toolsStarted == 0 {
		s.sawTool = false
	}
	// Never leave tools to re-emit after a delivered message_stop.
	if requeuedTool && s.terminalEmitted {
		s.UnackTerminal()
	}
}

func (s *StreamAssembler) Finish(finishReason string, usage Usage) []string {
	// Soft write may have left unacked tools/terminal; requeue before force-finish
	// so we re-emit complete tool groups and/or message_stop instead of silence.
	// Requeue clears terminalEmitted when tools need re-emit so we never produce
	// tool_use after message_stop ("Tool use interrupted").
	s.RequeueUnackedTools()
	// toolsOnly only when terminal already landed and no tool work remains to frame.
	toolsOnly := s.terminalEmitted && !s.HasPendingTools()
	frames := s.Start(usage.PromptTokens)
	for _, state := range s.tools {
		if state.started || state.stopped {
			continue
		}
		// Name may have arrived late; ensure CanonicalName + edit aliases re-apply.
		if state.name != "" {
			state.name = toolcall.CanonicalName(state.name, s.allowed)
		}
		if state.name == "" {
			continue
		}
		// Force-finish recovery: trailing junk / mild truncation / late Update→Edit
		// rename should not drop otherwise-valid tools intermittently.
		state.arguments = toolcall.CoerceCompleteJSON(state.arguments, state.name)
		// If still incomplete, retry under Edit schema (Update→Edit rename race:
		// name may have been Update when first args arrived; after CanonicalName
		// it's Edit and edit-only aliases/defaults must re-apply).
		if !toolcall.CompleteJSON(state.arguments, state.name) {
			for _, tryName := range []string{
				toolcall.CanonicalName("Edit", s.allowed),
				toolcall.CanonicalName("Update", s.allowed),
				"Edit",
			} {
				tryName = strings.TrimSpace(tryName)
				if tryName == "" || tryName == state.name {
					continue
				}
				if coerced := toolcall.CoerceCompleteJSON(state.arguments, tryName); toolcall.CompleteJSON(coerced, tryName) {
					state.name = tryName
					state.arguments = coerced
					break
				}
			}
		}
	}
	// Always emit held text before tools — the model may have produced
	// explanatory text before the first tool_use and discarding it causes
	// the client to miss important context.
	for _, delta := range s.held {
		frames = append(frames, s.emitText(delta.reasoning, delta.content)...)
	}
	s.held = nil
	frames = append(frames, s.closeThinking()...)
	frames = append(frames, s.closeText()...)
	// force=true: after CoerceCompleteJSON, use CompleteJSON (not Strict) so
	// salvaged/truncated tools still emit instead of vanishing mid-turn.
	frames = append(frames, s.emitReadyTools(true)...)
	frames = append(frames, s.closeTools()...)

	if toolsOnly {
		// Tools re-emit after a prior Ack'd terminal; do not duplicate message_stop.
		return frames
	}

	outputTokens := usage.CompletionTokens
	if outputTokens <= 0 && s.outputRunes > 0 {
		outputTokens = s.outputRunes / 4
		if outputTokens == 0 {
			outputTokens = 1
		}
	}
	// Always emit message_delta + message_stop so Claude Code can leave "running"
	// even when tools were incomplete/dropped. Mark pending until AckTerminal
	// (soft write of this batch must be able to re-emit via Requeue+Finish).
	// stop_reason tool_use only when a tool actually started/acked — pending
	// incomplete tools that vanished must not claim tool_use (hollow success).
	hasToolUse := s.toolsStarted > 0 || s.sawTool
	if hasToolUse {
		// Confirm at least one started or acked tool frame exists.
		hasToolUse = false
		for _, state := range s.tools {
			if state != nil && (state.started || state.stopped || state.clientAcked) {
				hasToolUse = true
				break
			}
		}
	}
	frames = append(frames, event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": StopReason(finishReason, hasToolUse), "stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens":               outputTokens,
			"input_tokens":                usage.PromptTokens,
			"cache_read_input_tokens":     usage.CacheReadTokens,
			"cache_creation_input_tokens": usage.CacheCreationTokens,
		},
	}))
	frames = append(frames, messageStopFrame)
	s.pendingTerminal = true
	return frames
}

func (s *StreamAssembler) emitReadyTools(force bool) []string {
	indexes := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	frames := make([]string, 0)
	// Fresh pending batch for this emit; previous unacked tools should have been
	// requeued via RequeueUnackedTools before re-entry.
	s.pendingClientAcks = s.pendingClientAcks[:0]
	for _, index := range indexes {
		state := s.tools[index]
		if state.stopped || state.started {
			continue
		}
		if state.name == "" {
			continue
		}
		// Live path (force=false): normalize + CompleteJSONStrict so truncation
		// repair cannot mark an unterminated fragment complete.
		// Force path (force=true, Finish already CoerceCompleteJSON'd): use
		// CompleteJSON / object salvage so tools still emit instead of vanishing
		// mid-turn (Claude Code "Tool use interrupted").
		normalized := toolcall.NormalizeJSON(state.arguments, state.name)
		if normalized == "" {
			normalized = state.arguments
		}
		ready := false
		if force {
			// Finish already ran CoerceCompleteJSON; accept CompleteJSON only.
			// Do NOT salvage incomplete objects — that would emit half Edit/Update
			// payloads Claude Code then treats as broken tool use.
			ready = toolcall.CompleteJSON(normalized, state.name)
		} else if toolcall.CompleteJSONStrict(normalized, state.name) {
			ready = true
		}
		if !ready {
			// Do NOT break: a lower index may be incomplete while a higher
			// index already has complete JSON. Breaking here hung Claude Code
			// tasks that received tools out of order / partial early slots.
			continue
		}
		if s.maxTools > 0 && s.toolsStarted >= s.maxTools {
			break
		}
		state.arguments = normalized
		state.block = s.nextBlock
		s.nextBlock++
		state.started = true
		state.clientAcked = false
		s.toolsStarted++
		s.sawTool = true
		s.held = nil
		argsJSON := state.arguments
		if strings.TrimSpace(argsJSON) == "" {
			argsJSON = "{}"
		}
		frames = append(frames, event("content_block_start", map[string]any{
			"type": "content_block_start", "index": state.block,
			"content_block": map[string]any{
				"type": "tool_use", "id": state.id, "name": state.name, "input": map[string]any{},
			},
		}))
		frames = append(frames, event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": state.block,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": argsJSON},
		}))
		s.outputRunes += len([]rune(argsJSON))
		frames = append(frames, event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": state.block,
		}))
		state.stopped = true
		s.pendingClientAcks = append(s.pendingClientAcks, index)
	}
	return frames
}

func (s *StreamAssembler) emitText(reasoning, content string) []string {
	frames := make([]string, 0, 4)
	if reasoning != "" {
		frames = append(frames, s.closeTools()...)
		frames = append(frames, s.closeText()...)
		if s.thinkingBlock < 0 {
			s.thinkingBlock = s.nextBlock
			s.nextBlock++
			frames = append(frames, event("content_block_start", map[string]any{
				"type": "content_block_start", "index": s.thinkingBlock,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}))
		}
		frames = append(frames, event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.thinkingBlock,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		}))
		s.outputRunes += len([]rune(reasoning))
	}
	if content != "" {
		frames = append(frames, s.closeTools()...)
		frames = append(frames, s.closeThinking()...)
		if s.textBlock < 0 {
			s.textBlock = s.nextBlock
			s.nextBlock++
			frames = append(frames, event("content_block_start", map[string]any{
				"type": "content_block_start", "index": s.textBlock,
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
		}
		frames = append(frames, event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.textBlock,
			"delta": map[string]any{"type": "text_delta", "text": content},
		}))
		s.outputRunes += len([]rune(content))
	}
	return frames
}

func (s *StreamAssembler) closeText() []string {
	if s.textBlock < 0 {
		return nil
	}
	index := s.textBlock
	s.textBlock = -1
	return []string{event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})}
}

func (s *StreamAssembler) closeThinking() []string {
	if s.thinkingBlock < 0 {
		return nil
	}
	index := s.thinkingBlock
	s.thinkingBlock = -1
	return []string{event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})}
}

func (s *StreamAssembler) closeTools() []string {
	frames := make([]string, 0)
	for _, state := range s.tools {
		if state.started && !state.stopped {
			frames = append(frames, event("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": state.block,
			}))
			state.stopped = true
		}
	}
	return frames
}

func mergeName(current, incoming string) string {
	if current == "" {
		return incoming
	}
	if incoming == "" || current == incoming || len(current) > len(incoming) && current[:len(incoming)] == incoming {
		return current
	}
	if len(incoming) > len(current) && incoming[:len(current)] == current {
		return incoming
	}
	return incoming
}

func ParseEvents(frames []string) []map[string]any {
	out := make([]map[string]any, 0, len(frames))
	for _, frame := range frames {
		for _, line := range splitLines(frame) {
			if len(line) < 5 || line[:5] != "data:" {
				continue
			}
			var payload map[string]any
			if json.Unmarshal([]byte(trimSpace(line[5:])), &payload) == nil {
				out = append(out, payload)
			}
		}
	}
	return out
}

func splitLines(value string) []string {
	var lines []string
	start := 0
	for index, r := range value {
		if r == '\n' {
			lines = append(lines, value[start:index])
			start = index + 1
		}
	}
	if start < len(value) {
		lines = append(lines, value[start:])
	}
	return lines
}

func trimSpace(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	return value
}
