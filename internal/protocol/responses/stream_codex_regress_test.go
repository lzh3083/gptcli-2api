package responses

import (
	"strings"
	"testing"
)

// Codex regression: upstream sometimes re-sends a fuller name after deltas
// already streamed under a shorter prefix (e.g. "exec" → "exec_command").
// mergeName prefers the longer incoming string, so the tool identity flips
// mid-buffer; emission + cmd projection must use the final name.
func TestNameFlipMidStreamStillEmitsUnderFinalName(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_flip", "grok", nil, 0)
	frames := s.ToolDeltas([]ToolDelta{
		{Index: 0, ID: "c1", Name: "exec", Arguments: `{"command":`},
	})
	if strings.Contains(strings.Join(frames, ""), "function_call") {
		t.Fatalf("must not emit incomplete fragment")
	}
	frames = s.ToolDeltas([]ToolDelta{
		{Index: 0, ID: "c1", Name: "exec_command", Arguments: `{"command":"pwd"}`},
	})
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "exec_command") {
		t.Fatalf("expected exec_command function_call:\n%s", joined)
	}
	// Codex shell default: projected to cmd even without explicit key map.
	if !strings.Contains(joined, `\"cmd\"`) {
		t.Fatalf("expected cmd projection:\n%s", joined)
	}
	if strings.Contains(joined, `\"command\"`) {
		t.Fatalf("command key must not leak to Codex:\n%s", joined)
	}
}

// Codex regression: an incomplete fragment flushed at stream end must be
// dropped, but a later complete tool at a different index must still emit —
// emitReadyTools must not let one bad tool poison the rest of the flush.
func TestForceFlushSkipsBadToolKeepsGoodTool(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_flush", "grok", nil, 0)
	_ = s.ToolDeltas([]ToolDelta{
		{Index: 0, ID: "bad", Name: "shell", Arguments: `{"command":`},
		{Index: 1, ID: "good", Name: "shell", Arguments: `{"command":"ls"}`},
	})
	frames := s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, `"call_id":"bad"`) {
		t.Fatalf("incomplete tool must be dropped at force flush:\n%s", joined)
	}
	if !strings.Contains(joined, `"call_id":"good"`) {
		t.Fatalf("complete tool must survive force flush:\n%s", joined)
	}
	if !strings.Contains(joined, "response.completed") {
		t.Fatalf("envelope must still close:\n%s", joined)
	}
}
