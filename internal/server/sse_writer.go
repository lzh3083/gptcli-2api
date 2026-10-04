package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

// sseWriter is the shared hot-path Write+Flush primitive for Chat / Responses /
// Anthropic SSE. It centralises soft-disconnect handling, short-write detection,
// reusable buffers, and keepalive throttling so each protocol path does not
// re-implement the same syscall/flush storms.
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	ctx     context.Context

	softGone bool
	lastOK   bool
	// lastWritten is bytes accepted by the ResponseWriter on the most recent
	// WriteBytes call (even when that call soft-failed as a short write).
	// Coalesce flushers advance the pending buffer by this amount so a partial
	// write is not re-sent (duplicate prefix) or silently dropped (truncation).
	lastWritten int
	buf         []byte

	// lastKeepalive gates pending-tool / idle keepalive frames. Writing a
	// keepalive on every incomplete tool-arg chunk was a major flush storm
	// under Claude Code multi-tool turns; proxies only need ~1–3s warmth.
	lastKeepalive time.Time
}

func newSSEWriter(w http.ResponseWriter, flusher http.Flusher, ctx context.Context) *sseWriter {
	return &sseWriter{
		w:       w,
		flusher: flusher,
		ctx:     ctx,
		buf:     make([]byte, 0, 4096),
	}
}

func (s *sseWriter) SoftGone() bool { return s != nil && s.softGone }
func (s *sseWriter) LastOK() bool   { return s != nil && s.lastOK }

// LastWritten returns how many bytes the last WriteBytes accepted on the wire
// (0 when nothing landed). Soft short-writes leave lastOK=false but lastWritten>0
// so coalesce flushers can drop only the delivered prefix and retry the tail.
func (s *sseWriter) LastWritten() int {
	if s == nil {
		return 0
	}
	return s.lastWritten
}

func (s *sseWriter) MarkSoftGone() {
	if s != nil && !s.softGone {
		s.softGone = true
		streamSoftGoneTotal.Add(1)
	}
}

// WriteBytes writes one payload with a single Flush. Soft client errors are
// swallowed (softGone=true, lastOK=false) so callers can Requeue unacked tools
// instead of aborting ReadSSE mid-envelope.
//
// Short writes (n < len, err==nil or soft err) set lastWritten=n and lastOK=false
// so text coalescers can advance the pending buffer and re-send only the remainder
// — full-buffer retry would duplicate the accepted prefix (garbled / repeated text).
func (s *sseWriter) WriteBytes(payload []byte, force bool) error {
	if s == nil {
		return errors.New("sse writer is nil")
	}
	s.lastOK = false
	s.lastWritten = 0
	if len(payload) == 0 {
		s.lastOK = true
		return nil
	}
	if s.softGone && !force {
		return nil
	}
	if !force && s.ctx != nil {
		select {
		case <-s.ctx.Done():
			if !s.softGone {
				s.softGone = true
				streamSoftGoneTotal.Add(1)
			}
			// Keep consuming upstream so force-finish / Complete can still run.
			return nil
		default:
		}
	}
	n, err := s.w.Write(payload)
	if n < 0 {
		n = 0
	}
	if n > len(payload) {
		n = len(payload)
	}
	s.lastWritten = n
	if err == nil && n < len(payload) {
		err = errors.New("short write: connection reset by peer")
	}
	if err != nil {
		if isSoftClientWriteError(err) || (s.ctx != nil && errors.Is(err, s.ctx.Err())) {
			if !s.softGone {
				s.softGone = true
				streamSoftGoneTotal.Add(1)
			}
			// lastOK stays false → caller requeues / advances pending by lastWritten.
			// Bytes already accepted (n>0) are not rewound — do not re-send them.
			if n > 0 {
				streamBytesTotal.Add(uint64(n))
			}
			return nil
		}
		return err
	}
	s.flusher.Flush()
	s.lastOK = true
	streamWritesTotal.Add(1)
	streamBytesTotal.Add(uint64(len(payload)))
	return nil
}

// WriteStrings joins frames into the reusable buffer and writes once.
func (s *sseWriter) WriteStrings(frames []string, force bool) error {
	if s == nil {
		return errors.New("sse writer is nil")
	}
	if len(frames) == 0 {
		s.lastOK = true
		return nil
	}
	s.buf = s.buf[:0]
	for _, frame := range frames {
		s.buf = append(s.buf, frame...)
	}
	return s.WriteBytes(s.buf, force)
}

// sseCompletePrefix returns the longest prefix of payload that ends on an SSE
// frame boundary (\n\n) and is fully contained in the first n written bytes.
// The remainder starts at the first incomplete frame (never mid-frame).
//
// Tool groups (start+delta+stop) and terminal batches use this so a soft short
// write does not re-send a already-landed content_block_start (half-open tool_use
// → Claude Code "Tool use interrupted").
func sseCompletePrefix(payload []byte, n int) (prefix, remainder []byte) {
	if len(payload) == 0 {
		return nil, nil
	}
	if n <= 0 {
		return nil, payload
	}
	if n >= len(payload) {
		// Full buffer accepted — treat as complete even if trailing frame lacks \n\n.
		return payload, nil
	}
	lastEnd := 0
	start := 0
	for start < len(payload) {
		i := bytes.Index(payload[start:], []byte("\n\n"))
		if i < 0 {
			break
		}
		end := start + i + 2
		if end <= n {
			lastEnd = end
			start = end
			continue
		}
		break
	}
	if lastEnd == 0 {
		// Mid first frame — cannot Ack; requeue path should rebuild, not slice mid-JSON.
		return nil, payload
	}
	return payload[:lastEnd], payload[lastEnd:]
}

// WriteStringsResumable writes joined frames, and on soft short-write continues
// with only the unsent complete-frame-aligned tail (up to attempts). Returns the
// complete-frame prefix that landed (for Ack) and whether the full join was OK.
//
// Used by Anthropic tool/terminal group flushes — same-byte full retries after a
// partial write duplicate content_block_start and interrupt Claude Code.
func (s *sseWriter) WriteStringsResumable(frames []string, force bool, attempts int) (delivered string, fullOK bool, err error) {
	if s == nil {
		return "", false, errors.New("sse writer is nil")
	}
	if len(frames) == 0 {
		s.lastOK = true
		return "", true, nil
	}
	if attempts < 1 {
		attempts = 1
	}
	s.buf = s.buf[:0]
	for _, frame := range frames {
		s.buf = append(s.buf, frame...)
	}
	// Copy out of reusable buf before retries mutate lastWritten/state.
	payload := append([]byte(nil), s.buf...)
	var landed []byte
	noProgress := 0
	for i := 0; i < attempts && len(payload) > 0; i++ {
		if i > 0 {
			time.Sleep(time.Duration(i) * 2 * time.Millisecond)
		}
		// Once we have landed a complete-frame prefix, force the tail so softGone
		// (set by the short write) does not swallow the remainder without writing.
		forceWrite := force || len(landed) > 0
		if err = s.WriteBytes(payload, forceWrite); err != nil {
			return string(landed), false, err
		}
		if s.LastOK() {
			landed = append(landed, payload...)
			return string(landed), true, nil
		}
		// Soft fail: keep only complete SSE frames from what landed; continue tail.
		n := s.LastWritten()
		prefix, rem := sseCompletePrefix(payload, n)
		if len(prefix) > 0 {
			landed = append(landed, prefix...)
			noProgress = 0
		}
		if len(rem) == 0 {
			// Nothing left to send (or only mid-frame junk).
			break
		}
		if len(prefix) == 0 && n == 0 {
			// Nothing landed — full-buffer retry is safe.
			noProgress++
			if noProgress >= 2 && !forceWrite {
				// Escalate to force so softGone path still attempts the write.
				force = true
			}
			continue
		}
		if len(prefix) == 0 && n > 0 {
			// Mid-frame short write: do not resend mid-JSON; caller may requeue rebuild.
			// Still try force-writing the same payload once more if attempts remain —
			// a pure force retry of the full rem is safer than cutting mid-frame.
			noProgress++
			if noProgress >= 2 {
				break
			}
			continue
		}
		payload = rem
	}
	return string(landed), false, nil
}

// WriteString is a convenience for a single SSE frame / comment.
func (s *sseWriter) WriteString(frame string, force bool) error {
	if frame == "" {
		if s != nil {
			s.lastOK = true
		}
		return nil
	}
	return s.WriteBytes([]byte(frame), force)
}

// Keepalive writes frame at most once per minInterval unless force is true and
// the interval has elapsed (or never written). Returns nil without writing when
// throttled — callers use this for pending-tool warmth, not terminal delivery.
func (s *sseWriter) Keepalive(frame string, minInterval time.Duration, force bool) error {
	if s == nil {
		return errors.New("sse writer is nil")
	}
	if frame == "" {
		return nil
	}
	if minInterval <= 0 {
		minInterval = 1500 * time.Millisecond
	}
	now := time.Now()
	if !s.lastKeepalive.IsZero() && now.Sub(s.lastKeepalive) < minInterval {
		return nil
	}
	if err := s.WriteString(frame, force); err != nil {
		return err
	}
	if s.lastOK {
		s.lastKeepalive = now
		streamKeepalivesTotal.Add(1)
	}
	return nil
}

// DefaultKeepaliveInterval is the minimum gap between forced pending-tool
// keepalives. Reverse proxies cut around 30–60s idle; 1.5s keeps the pipe warm
// without one Flush per tool-arg micro-chunk.
const DefaultKeepaliveInterval = 1500 * time.Millisecond

// Cheap SSE frame classifiers used by stream groupers (avoid re-scanning
// full JSON with ad-hoc strings.Contains at every call site).

func frameIsAnthropicToolStart(frame string) bool {
	return strings.Contains(frame, `"tool_use"`) && strings.Contains(frame, "content_block_start")
}

func frameIsResponsesToolStart(frame string) bool {
	return strings.Contains(frame, "function_call") && strings.Contains(frame, "response.output_item.added")
}

func frameIsAnthropicTerminal(frame string) bool {
	return strings.Contains(frame, "message_stop") || strings.Contains(frame, "message_delta") ||
		strings.Contains(frame, `"type":"message_stop"`) || strings.Contains(frame, `"type":"message_delta"`)
}

func frameIsResponsesTerminal(frame string) bool {
	return strings.Contains(frame, "response.completed") || strings.Contains(frame, "response.failed") ||
		strings.Contains(frame, "[DONE]")
}

func frameNeedsAnthropicImmediate(frame string) bool {
	return strings.Contains(frame, `"tool_use"`) ||
		strings.Contains(frame, "message_stop") ||
		strings.Contains(frame, "message_delta") ||
		strings.Contains(frame, "message_start") ||
		strings.Contains(frame, "event: error") ||
		strings.Contains(frame, `"type":"error"`)
}

func frameNeedsResponsesImmediate(frame string) bool {
	return strings.Contains(frame, "function_call") ||
		strings.Contains(frame, "response.completed") ||
		strings.Contains(frame, "response.failed") ||
		strings.Contains(frame, "[DONE]") ||
		strings.Contains(frame, "response.output_item.added")
}

// waitToolGap sleeps for gap or until ctx is done. Returns true if ctx cancelled.
// Uses a single Timer (not time.After) so multi-tool turns do not leak timers
// when the client soft-disconnects mid-gap.
func waitToolGap(ctx context.Context, gap time.Duration) (cancelled bool) {
	if gap <= 0 || ctx == nil {
		return false
	}
	timer := time.NewTimer(gap)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return true
	case <-timer.C:
		return false
	}
}
