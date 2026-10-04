package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamMetricsIncrementOnWrite(t *testing.T) {
	beforeW := streamWritesTotal.Load()
	beforeB := streamBytesTotal.Load()
	beforeK := streamKeepalivesTotal.Load()
	rec := httptest.NewRecorder()
	sw := newSSEWriter(rec, rec, context.Background())
	if err := sw.WriteString("data: hi\n\n", true); err != nil {
		t.Fatal(err)
	}
	if streamWritesTotal.Load() != beforeW+1 {
		t.Fatalf("writes %d -> %d", beforeW, streamWritesTotal.Load())
	}
	if streamBytesTotal.Load() <= beforeB {
		t.Fatalf("bytes did not increase")
	}
	if err := sw.Keepalive(": keepalive\n\n", 1, true); err != nil {
		t.Fatal(err)
	}
	if streamKeepalivesTotal.Load() != beforeK+1 {
		t.Fatalf("keepalives %d -> %d", beforeK, streamKeepalivesTotal.Load())
	}
	out := streamMetricsPrometheus()
	for _, m := range []string{"g2a_stream_writes_total", "g2a_stream_bytes_total", "g2a_stream_keepalives_total"} {
		if !strings.Contains(out, m) {
			t.Fatalf("missing %s in %q", m, out)
		}
	}
}
