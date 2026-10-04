package server

import (
	"strconv"
	"sync/atomic"
)

// Lightweight process-wide stream counters for /metrics.
// Cheap atomics only — no labels per-request (keeps hot path allocation-free).
var (
	streamWritesTotal     atomic.Uint64
	streamBytesTotal      atomic.Uint64
	streamKeepalivesTotal atomic.Uint64
	streamSoftGoneTotal   atomic.Uint64
	streamCoalesceFlush   atomic.Uint64
)

func streamMetricsPrometheus() string {
	var b []byte
	b = append(b, "# HELP g2a_stream_writes_total Client SSE Write+Flush operations.\n"...)
	b = append(b, "# TYPE g2a_stream_writes_total counter\n"...)
	b = append(b, "g2a_stream_writes_total{implementation=\"go\"} "...)
	b = append(b, strconv.FormatUint(streamWritesTotal.Load(), 10)...)
	b = append(b, '\n')

	b = append(b, "# HELP g2a_stream_bytes_total Client SSE payload bytes written.\n"...)
	b = append(b, "# TYPE g2a_stream_bytes_total counter\n"...)
	b = append(b, "g2a_stream_bytes_total{implementation=\"go\"} "...)
	b = append(b, strconv.FormatUint(streamBytesTotal.Load(), 10)...)
	b = append(b, '\n')

	b = append(b, "# HELP g2a_stream_keepalives_total SSE keepalive frames written (after throttle).\n"...)
	b = append(b, "# TYPE g2a_stream_keepalives_total counter\n"...)
	b = append(b, "g2a_stream_keepalives_total{implementation=\"go\"} "...)
	b = append(b, strconv.FormatUint(streamKeepalivesTotal.Load(), 10)...)
	b = append(b, '\n')

	b = append(b, "# HELP g2a_stream_soft_gone_total Soft client disconnects detected mid-stream.\n"...)
	b = append(b, "# TYPE g2a_stream_soft_gone_total counter\n"...)
	b = append(b, "g2a_stream_soft_gone_total{implementation=\"go\"} "...)
	b = append(b, strconv.FormatUint(streamSoftGoneTotal.Load(), 10)...)
	b = append(b, '\n')

	b = append(b, "# HELP g2a_stream_coalesce_flush_total Coalesced text/thinking flushes (batch >1 frame).\n"...)
	b = append(b, "# TYPE g2a_stream_coalesce_flush_total counter\n"...)
	b = append(b, "g2a_stream_coalesce_flush_total{implementation=\"go\"} "...)
	b = append(b, strconv.FormatUint(streamCoalesceFlush.Load(), 10)...)
	b = append(b, '\n')
	return string(b)
}

// streamSnapshot returns a JSON-friendly map of process stream counters for admin UI.
func streamSnapshot() map[string]any {
	return map[string]any{
		"writes_total":         streamWritesTotal.Load(),
		"bytes_total":          streamBytesTotal.Load(),
		"keepalives_total":     streamKeepalivesTotal.Load(),
		"soft_gone_total":      streamSoftGoneTotal.Load(),
		"coalesce_flush_total": streamCoalesceFlush.Load(),
	}
}
