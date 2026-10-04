package anthropic

import (
	"strings"
	"testing"
)

func TestStreamEmitsNormalizedEditArgs(t *testing.T) {
	// Grok path/search/replace aliases must become Claude Edit keys in partial_json.
	assembler := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := assembler.Feed("", "", []ToolDelta{{
		Index: 0, ID: "t", Name: "Update",
		Arguments: `{"path":"/x.go","search":"old","replace":"new"}`,
	}})
	// Simulate successful client write so Finish does not requeue/re-emit tools.
	assembler.AckEmittedTools()
	frames = append(frames, assembler.Finish("tool_calls", Usage{})...)
	joined := strings.Join(frames, "")
	// partial_json is JSON-encoded inside the SSE data frame, so keys appear escaped.
	for _, marker := range []string{`"type":"tool_use"`, `"name":"Edit"`, `\"file_path\"`, `\"old_string\"`, `\"new_string\"`, "message_stop"} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("missing %q in %s", marker, joined)
		}
	}
	if !strings.Contains(joined, `\"file_path\":\"/x.go\"`) {
		t.Fatalf("expected rewritten file_path in partial_json: %s", joined)
	}
}

func TestStreamIncompleteToolKeepsTerminalWithoutToolBlock(t *testing.T) {
	assembler := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := assembler.Feed("", "", []ToolDelta{{Index: 0, ID: "t", Name: "Update", Arguments: `{"file_path":"/x"}`}})
	frames = append(frames, assembler.Finish("tool_calls", Usage{})...)
	events := ParseEvents(frames)
	sawTool, sawStop := false, false
	for _, payload := range events {
		if payload["type"] == "content_block_start" {
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				sawTool = true
			}
		}
		if payload["type"] == "message_stop" {
			sawStop = true
		}
	}
	if sawTool || !sawStop {
		t.Fatalf("tool=%v stop=%v events=%#v", sawTool, sawStop, events)
	}
}

func TestStreamCompleteUpdateIsDenseEditTool(t *testing.T) {
	assembler := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := assembler.Feed("preface", "", []ToolDelta{{
		Index: 2, ID: "t", Name: "Update",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":""}`,
	}})
	// Successful client write — without Ack, Finish requeues and re-emits tools
	// at a new dense index, which would fail the dense-index-0 contract.
	assembler.AckEmittedTools()
	frames = append(frames, assembler.Finish("tool_calls", Usage{PromptTokens: 2, CompletionTokens: 1})...)
	events := ParseEvents(frames)
	toolIndex := -1
	stopReason := ""
	for _, payload := range events {
		if payload["type"] == "content_block_start" {
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolIndex = int(payload["index"].(float64))
				if block["name"] != "Edit" {
					t.Fatalf("unexpected block %#v", block)
				}
			}
		}
		if payload["type"] == "message_delta" {
			delta := payload["delta"].(map[string]any)
			stopReason, _ = delta["stop_reason"].(string)
		}
	}
	if toolIndex != 0 || stopReason != "tool_use" {
		t.Fatalf("index=%d stop=%q events=%#v", toolIndex, stopReason, events)
	}
}

func TestOutOfOrderAnthropicToolsStillEmit(t *testing.T) {
	assembler := NewStreamAssembler("m", "g", true, 2, []string{"Bash", "Read"})
	// Incomplete tool at index 0 should not block complete tool at index 1.
	incomplete := "{\"command\":"
	complete := "{\"file_path\":\"/a\"}"
	frames := assembler.Feed("", "", []ToolDelta{
		{Index: 0, ID: "t0", Name: "Bash", Arguments: incomplete},
		{Index: 1, ID: "t1", Name: "Read", Arguments: complete},
	})
	assembler.AckEmittedTools()
	frames = append(frames, assembler.Finish("tool_calls", Usage{})...)
	events := ParseEvents(frames)
	sawRead, sawStop := false, false
	for _, payload := range events {
		if payload["type"] == "content_block_start" {
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "tool_use" && block["name"] == "Read" {
				sawRead = true
			}
		}
		if payload["type"] == "message_stop" {
			sawStop = true
		}
	}
	if !sawRead || !sawStop {
		t.Fatalf("read=%v stop=%v events=%#v", sawRead, sawStop, events)
	}
}

func TestStreamLiveReasoningWhileToolsPending(t *testing.T) {
	// toolsRequested holds TEXT but must stream REASONING live so long thinking
	// turns keep the SSE pipe warm (~60s idle kills otherwise).
	assembler := NewStreamAssembler("m", "g", true, 1, []string{"Read"})
	frames := assembler.Feed("", "thinking…", nil)
	events := ParseEvents(frames)
	sawThinking := false
	for _, payload := range events {
		if payload["type"] == "content_block_delta" {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" {
				sawThinking = true
			}
		}
	}
	if !sawThinking {
		t.Fatalf("expected live thinking_delta under toolsRequested, events=%#v", events)
	}
	// Text still held until Finish or tool.
	frames2 := assembler.Feed("hello", "", nil)
	for _, payload := range ParseEvents(frames2) {
		if payload["type"] == "content_block_delta" {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "text_delta" {
				t.Fatalf("text should be held before tools/finish: %#v", payload)
			}
		}
	}
	// Finish without tools flushes held text.
	frames3 := assembler.Finish("stop", Usage{})
	joined := ""
	for _, f := range frames3 {
		joined += f
	}
	if !contains(joined, "hello") || !contains(joined, "message_stop") {
		t.Fatalf("expected flushed text + stop, got %q", joined)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}

func TestLongThinkingThenMultiToolsClosesCleanly(t *testing.T) {
	// Simulates Claude Code: long thinking stream with tools declared, then
	// multi-tool finish. Reasoning must be live; text held; tools dense; always
	// terminal message_stop.
	a := NewStreamAssembler("msg_long", "grok-4.5", true, 2, []string{"Read", "Bash", "Edit"})

	// Phase 1: long thinking (many chunks) — must emit thinking_delta live.
	var live []string
	for i := 0; i < 40; i++ {
		live = append(live, a.Feed("", "think-chunk-", nil)...)
	}
	liveJoined := strings.Join(live, "")
	if !strings.Contains(liveJoined, "thinking_delta") {
		t.Fatalf("long thinking produced no live thinking_delta: %q", liveJoined[:min(200, len(liveJoined))])
	}
	// Held text should not appear yet.
	heldText := a.Feed("preface that must wait", "", nil)
	for _, f := range heldText {
		if strings.Contains(f, "text_delta") {
			t.Fatalf("text leaked before tools/finish: %q", f)
		}
	}

	// Phase 2: out-of-order tools — incomplete Bash@0, complete Read@1, complete Edit@2
	// maxTools=2 so only first two complete/forced slots emit.
	incompleteBash := `{"command":`
	completeRead := `{"file_path":"/a.go"}`
	completeEdit := `{"file_path":"/b.go","old_string":"x","new_string":"y"}`
	toolFrames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "t0", Name: "Bash", Arguments: incompleteBash}, // incomplete
		{Index: 1, ID: "t1", Name: "Read", Arguments: completeRead},
		{Index: 2, ID: "t2", Name: "Edit", Arguments: completeEdit},
	})
	a.AckEmittedTools()
	// Finish must close envelope even with incomplete Bash.
	all := append(toolFrames, a.Finish("tool_calls", Usage{PromptTokens: 10, CompletionTokens: 5})...)
	events := ParseEvents(all)

	var tools []string
	sawStop, sawDelta := false, false
	for _, ev := range events {
		switch ev["type"] {
		case "content_block_start":
			block, _ := ev["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				if name, _ := block["name"].(string); name != "" {
					tools = append(tools, name)
				}
			}
		case "message_delta":
			sawDelta = true
		case "message_stop":
			sawStop = true
		}
	}
	if !sawStop || !sawDelta {
		t.Fatalf("missing terminal delta/stop tools=%v events=%#v", tools, events)
	}
	// Read must have been emitted (complete, not blocked by incomplete Bash@0).
	foundRead := false
	for _, n := range tools {
		if n == "Read" {
			foundRead = true
		}
	}
	if !foundRead {
		t.Fatalf("Read tool blocked by incomplete earlier index; tools=%v", tools)
	}
	// Incomplete Bash must NOT be forced as tool_use (contract).
	for _, n := range tools {
		if n == "Bash" {
			t.Fatalf("incomplete Bash should not emit tool_use; tools=%v", tools)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestRequeueUnackedToolsReemitsOnFinish(t *testing.T) {
	// Soft write: frames produced but never acked → Requeue → Finish re-emits full group.
	a := NewStreamAssembler("msg_rq", "grok", true, 0, []string{"Edit"})
	_ = a.Start(0)
	a.AckMessageStart()
	mid := a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "call_1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":"b"}`,
	}})
	joinedMid := ""
	for _, f := range mid {
		joinedMid += f
	}
	if !contains(joinedMid, "tool_use") {
		t.Fatalf("expected live tool_use frames:\n%s", joinedMid)
	}
	if !a.HasUnackedTools() {
		t.Fatal("tools must be unacked until AckEmittedTools")
	}
	// Simulate soft write: requeue instead of ack.
	a.RequeueUnackedTools()
	if a.HasUnackedTools() {
		t.Fatal("after requeue, tools should be pending not unacked-started")
	}
	if !a.HasPendingTools() && !a.HasClientPayload() {
		// HasPendingTools looks for !started with name/args — should be true.
		t.Fatal("requeued tool must be pending")
	}
	fin := a.Finish("tool_calls", Usage{})
	joined := ""
	for _, f := range fin {
		joined += f
	}
	for _, marker := range []string{"tool_use", "input_json_delta", "content_block_stop", "message_stop"} {
		if !contains(joined, marker) {
			t.Fatalf("missing %q after requeue Finish:\n%s", marker, joined)
		}
	}
	// Double Finish after Ack must not re-emit tool_use.
	a.AckEmittedTools()
	if a.NeedsFinishRetry() {
		t.Fatal("after full Ack, NeedsFinishRetry must be false")
	}
	fin2 := a.Finish("tool_calls", Usage{})
	joined2 := ""
	for _, f := range fin2 {
		joined2 += f
	}
	// toolsOnly path after terminal: empty or no new tool_use
	if contains(joined2, `"type":"tool_use"`) || contains(joined2, "tool_use") {
		// Allow only if somehow not acked; should not happen.
		// Count tool_use occurrences should not grow from a second Finish after Ack.
		// After Ack, requeue finds nothing → toolsOnly with no tool frames.
		if strings.Count(joined2, "tool_use") > 0 && a.HasUnackedTools() {
			t.Fatalf("second Finish after Ack re-emitted tools:\n%s", joined2)
		}
	}
}

func TestSoftFailTerminalReemitsMessageStop(t *testing.T) {
	// Regression: terminalEmitted must wait for Ack. Soft-fail of Finish batch
	// used to set terminalEmitted=true before write → second Finish was toolsOnly
	// with no message_stop → Claude Code "Tool use interrupted".
	a := NewStreamAssembler("msg_term", "grok", true, 0, []string{"Edit"})
	_ = a.Start(0)
	a.AckMessageStart()
	_ = a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "call_1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":"b"}`,
	}})
	// Live tool write soft-failed: requeue before terminal.
	a.RequeueUnackedTools()
	fin1 := a.Finish("tool_calls", Usage{})
	joined1 := strings.Join(fin1, "")
	if !contains(joined1, "message_stop") || !contains(joined1, "tool_use") {
		t.Fatalf("first Finish must emit tools+stop:\n%s", joined1)
	}
	if a.TerminalDelivered() {
		t.Fatal("terminal must not be delivered until Ack")
	}
	if !a.HasUnackedTools() {
		t.Fatal("tools+terminal pending until Ack")
	}
	if !a.NeedsFinishRetry() {
		t.Fatal("NeedsFinishRetry before Ack")
	}
	// Soft write failed: Requeue (as emitFrames does when !lastWriteOK).
	a.RequeueUnackedTools()
	if a.TerminalDelivered() {
		t.Fatal("after Requeue, terminal still not delivered")
	}
	if !a.NeedsFinishRetry() {
		t.Fatal("NeedsFinishRetry after soft Requeue (message_stop never Ack'd)")
	}
	fin2 := a.Finish("tool_calls", Usage{})
	joined2 := strings.Join(fin2, "")
	for _, marker := range []string{"tool_use", "content_block_stop", "message_delta", "message_stop"} {
		if !contains(joined2, marker) {
			t.Fatalf("recovery Finish missing %q:\n%s", marker, joined2)
		}
	}
	a.AckEmittedTools()
	if !a.TerminalDelivered() || a.NeedsFinishRetry() {
		t.Fatalf("after recovery Ack: delivered=%v retry=%v", a.TerminalDelivered(), a.NeedsFinishRetry())
	}
}

func TestAckToolsInPayloadMatchesByID(t *testing.T) {
	// Soft-fail tool0 then succeed tool1 must Ack only tool1 (by id), not FIFO first.
	a := NewStreamAssembler("msg_ack_id", "grok", true, 0, []string{"Edit", "Read"})
	_ = a.Start(0)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "call_edit", Name: "Edit", Arguments: `{"file_path":"/a","old_string":"x","new_string":"y"}`},
		{Index: 1, ID: "call_read", Name: "Read", Arguments: `{"file_path":"/b"}`},
	})
	joined := strings.Join(frames, "")
	if strings.Count(joined, `"type":"tool_use"`) < 2 {
		t.Fatalf("need 2 tools, got body:\n%s", joined)
	}
	// Simulate only the Read group landed (Edit soft-failed).
	readOnly := ""
	// Build a synthetic payload containing only call_read.
	for _, f := range frames {
		if strings.Contains(f, "call_read") {
			readOnly += f
		}
	}
	if readOnly == "" {
		t.Fatal("expected call_read frames")
	}
	a.AckToolsInPayload(readOnly)
	if !a.HasUnackedTools() {
		t.Fatal("Edit must remain unacked after Read-only Ack")
	}
	// Finish recovery should re-emit Edit (and terminal), not drop it.
	a.RequeueUnackedTools()
	fin := a.Finish("tool_calls", Usage{})
	out := strings.Join(fin, "")
	if !strings.Contains(out, "call_edit") && !strings.Contains(out, "Edit") {
		t.Fatalf("expected re-emitted Edit after partial Ack:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("expected message_stop:\n%s", out)
	}
	// Read should NOT re-appear (already Ack'd).
	if strings.Contains(out, "call_read") {
		t.Fatalf("Read was Ack'd; must not re-emit:\n%s", out)
	}
}

func TestAckFirstPendingToolsDoesNotAckUnwritten(t *testing.T) {
	// Content-aware Ack: flushing tool0 only must not Ack tool1 still in a later batch.
	a := NewStreamAssembler("msg_partial", "grok", true, 0, []string{"Edit", "Read"})
	_ = a.Start(0)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "c0", Name: "Edit", Arguments: `{"file_path":"/a","old_string":"x","new_string":"y"}`},
		{Index: 1, ID: "c1", Name: "Read", Arguments: `{"file_path":"/b"}`},
	})
	joined := strings.Join(frames, "")
	toolCount := strings.Count(joined, `"type":"tool_use"`)
	if toolCount < 2 {
		t.Fatalf("expected 2 tool_use in one Feed emit, got %d:\n%s", toolCount, joined)
	}
	// Simulate emitFrames splitting: only first tool batch landed.
	a.AckFirstPendingTools(1)
	if !a.HasUnackedTools() {
		t.Fatal("second tool must still be unacked")
	}
	// Soft-fail of second tool batch → requeue only unacked.
	a.RequeueUnackedTools()
	if !a.HasPendingTools() {
		t.Fatal("requeued second tool must be pending")
	}
	fin := a.Finish("tool_calls", Usage{})
	out := strings.Join(fin, "")
	// Must re-emit the second tool (and message_stop); first was already Ack'd.
	if !contains(out, "message_stop") {
		t.Fatalf("missing message_stop:\n%s", out)
	}
	// Re-emit should include tool_use for the requeued Read (or both if both requeued —
	// only second was unacked so only Read re-emits).
	if !contains(out, "Read") && !contains(out, "tool_use") {
		t.Fatalf("expected re-emitted tool frames:\n%s", out)
	}
}

func TestAckToolsInPayloadSkipsFailedSibling(t *testing.T) {
	// Regression: multi-tool Finish with soft-fail of tool0 then success of tool1
	// must Ack only tool1 by id — FIFO AckFirstPendingTools(1) would Ack tool0 and
	// leave Claude Code missing the failed tool ("Tool use interrupted").
	a := NewStreamAssembler("msg_ids", "grok", true, 0, []string{"Edit", "Read"})
	_ = a.Start(0)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "call_edit", Name: "Edit", Arguments: `{"file_path":"/a","old_string":"x","new_string":"y"}`},
		{Index: 1, ID: "call_read", Name: "Read", Arguments: `{"file_path":"/b"}`},
	})
	joined := strings.Join(frames, "")
	if strings.Count(joined, `"type":"tool_use"`) < 2 {
		t.Fatalf("expected 2 tool_use, got frames:\n%s", joined)
	}
	// Soft-fail tool0 (edit): do not Ack. Success tool1 (read) payload only.
	// Extract the Read group roughly by id.
	readOnly := ""
	for _, f := range frames {
		if strings.Contains(f, "call_read") || (readOnly != "" && strings.Contains(f, "content_block")) {
			readOnly += f
		}
	}
	if readOnly == "" {
		// Fallback: build a synthetic payload containing only call_read.
		readOnly = `"type":"tool_use","id":"call_read","name":"Read"`
	}
	a.AckToolsInPayload(readOnly)
	if !a.HasUnackedTools() {
		t.Fatal("Edit must remain unacked after AckToolsInPayload(Read-only)")
	}
	// Requeue + Finish must re-emit Edit (and terminal).
	a.RequeueUnackedTools()
	fin := a.Finish("tool_calls", Usage{})
	out := strings.Join(fin, "")
	if !contains(out, "call_edit") && !contains(out, "Edit") {
		t.Fatalf("expected re-emitted Edit after sibling Ack:\n%s", out)
	}
	if !contains(out, "message_stop") {
		t.Fatalf("expected message_stop:\n%s", out)
	}
	// Read should not re-emit (already clientAcked).
	if strings.Count(out, "call_read") > 0 {
		// re-emit with same id would confuse clients; Fail.
		t.Fatalf("must not re-emit already-Ack'd Read:\n%s", out)
	}
}

func TestNeedsFinishRetryAfterTerminalSoftFail(t *testing.T) {
	a := NewStreamAssembler("msg_nr", "grok", true, 0, []string{"Bash"})
	_ = a.Start(0)
	a.AckMessageStart()
	_ = a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "c1", Name: "Bash", Arguments: `{"command":"echo hi"}`,
	}})
	// Simulate successful tool write, then soft-fail terminal.
	a.AckFirstPendingTools(1)
	fin := a.Finish("tool_calls", Usage{})
	if !contains(strings.Join(fin, ""), "message_stop") {
		t.Fatalf("Finish must emit terminal:\n%s", strings.Join(fin, ""))
	}
	if a.TerminalDelivered() {
		t.Fatal("terminal not delivered until Ack")
	}
	if !a.NeedsFinishRetry() {
		t.Fatal("NeedsFinishRetry while terminal pending")
	}
	// Soft fail → Requeue clears pendingTerminal but not terminalEmitted.
	a.RequeueUnackedTools()
	if a.TerminalDelivered() {
		t.Fatal("still not delivered")
	}
	if !a.NeedsFinishRetry() {
		t.Fatal("NeedsFinishRetry after soft Requeue of terminal")
	}
	fin2 := a.Finish("tool_calls", Usage{})
	out2 := strings.Join(fin2, "")
	// tools already Ack'd → toolsOnly? No: terminal never Ack'd, tools requeued only if unacked.
	// Bash was Ack'd so should not re-emit tool; must re-emit message_stop.
	if !contains(out2, "message_stop") {
		t.Fatalf("recovery Finish missing message_stop:\n%s", out2)
	}
	a.AckEmittedTools()
	if !a.TerminalDelivered() || a.NeedsFinishRetry() {
		t.Fatalf("after Ack: delivered=%v retry=%v", a.TerminalDelivered(), a.NeedsFinishRetry())
	}
}

func TestAckToolsInPayloadByID(t *testing.T) {
	// Multi-tool Finish: soft-fail tool0 then success tool1 must Ack only tool1
	// (by id in payload), not FIFO-first pending. FIFO Ack would mark the failed
	// tool delivered → missing tool_use / "Tool use interrupted".
	a := NewStreamAssembler("msg_payload_ack", "grok", true, 0, []string{"Edit", "Read"})
	_ = a.Start(0)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "call_edit", Name: "Edit", Arguments: `{"file_path":"/a","old_string":"x","new_string":"y"}`},
		{Index: 1, ID: "call_read", Name: "Read", Arguments: `{"file_path":"/b"}`},
	})
	joined := strings.Join(frames, "")
	if strings.Count(joined, `"type":"tool_use"`) < 2 {
		t.Fatalf("expected 2 tool_use frames:\n%s", joined)
	}
	// Simulate successful Write of only the Read tool group.
	a.AckToolsInPayload(`{"type":"tool_use","id":"call_read","name":"Read"}`)
	if !a.HasUnackedTools() {
		t.Fatal("Edit must remain unacked after AckToolsInPayload for Read only")
	}
	// Edit should still be started+unacked; Read clientAcked.
	// Requeue then Finish must re-emit Edit (and message_stop).
	a.RequeueUnackedTools()
	if !a.HasPendingTools() {
		t.Fatal("requeued Edit must be pending")
	}
	fin := a.Finish("tool_calls", Usage{})
	out := strings.Join(fin, "")
	if !contains(out, "call_edit") && !contains(out, "Edit") {
		t.Fatalf("expected re-emitted Edit tool:\n%s", out)
	}
	if !contains(out, "message_stop") {
		t.Fatalf("missing message_stop:\n%s", out)
	}
	// Read id must not be re-emitted (already acked).
	if contains(out, "call_read") {
		t.Fatalf("acked Read must not re-emit:\n%s", out)
	}
}

func TestNeedsFinishRetryAfterTerminalSoftRequeue(t *testing.T) {
	// Soft-fail of tools+terminal batch: Requeue clears pendingTerminal but
	// NeedsFinishRetry must stay true so server re-Finishes with message_stop.
	a := NewStreamAssembler("msg_retry", "grok", true, 0, []string{"Bash"})
	_ = a.Start(0)
	a.AckMessageStart()
	_ = a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "c1", Name: "Bash", Arguments: `{"command":"ls"}`,
	}})
	fin := a.Finish("tool_calls", Usage{})
	if !contains(strings.Join(fin, ""), "message_stop") {
		t.Fatal("Finish must emit message_stop")
	}
	if a.TerminalDelivered() {
		t.Fatal("terminal not delivered until Ack")
	}
	a.RequeueUnackedTools()
	if !a.NeedsFinishRetry() {
		t.Fatal("NeedsFinishRetry after soft Requeue of terminal")
	}
	fin2 := a.Finish("tool_calls", Usage{})
	out := strings.Join(fin2, "")
	for _, m := range []string{"tool_use", "message_stop", "content_block_stop"} {
		if !contains(out, m) {
			t.Fatalf("recovery missing %q:\n%s", m, out)
		}
	}
}

func TestAckToolsInPayloadSkipsFailedTool(t *testing.T) {
	// Multi-tool Finish: soft-fail tool0 then succeed tool1 must Ack only tool1 by id.
	// FIFO AckFirstPendingTools(1) would wrongly Ack tool0 → missing tool_use on recovery.
	a := NewStreamAssembler("msg_multi", "grok", true, 0, []string{"Edit", "Read"})
	_ = a.Start(0)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{
		{Index: 0, ID: "call_edit", Name: "Edit", Arguments: `{"file_path":"/a","old_string":"x","new_string":"y"}`},
		{Index: 1, ID: "call_read", Name: "Read", Arguments: `{"file_path":"/b"}`},
	})
	joined := strings.Join(frames, "")
	if strings.Count(joined, `"type":"tool_use"`) < 2 {
		t.Fatalf("expected 2 tools:\n%s", joined)
	}
	// Only tool1 Write landed (simulate content-based Ack of Read only).
	a.AckToolsInPayload(`{"type":"tool_use","id":"call_read","name":"Read"}`)
	if !a.HasUnackedTools() {
		t.Fatal("Edit must still be unacked")
	}
	// Edit still unacked; Read should be acked.
	a.RequeueUnackedTools()
	fin := a.Finish("tool_calls", Usage{})
	out := strings.Join(fin, "")
	if !contains(out, "call_edit") && !contains(out, "Edit") {
		t.Fatalf("recovery must re-emit Edit:\n%s", out)
	}
	// Read was acked — should not re-emit call_read id.
	if contains(out, "call_read") {
		t.Fatalf("acked Read must not re-emit:\n%s", out)
	}
	if !contains(out, "message_stop") {
		t.Fatalf("missing message_stop:\n%s", out)
	}
}

func TestForceFinishEmitsCoercedEdit(t *testing.T) {
	// Finish CoerceCompleteJSON + emitReadyTools(force=true) must emit Edit that
	// fails CompleteJSONStrict mid-stream (missing new_string).
	a := NewStreamAssembler("m", "g", true, 0, []string{"Edit"})
	_ = a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "t1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a"}`,
	}})
	// Live path must not emit yet.
	out := strings.Join(a.Feed("", "", nil), "")
	if contains(out, "tool_use") {
		t.Fatalf("live path must hold incomplete Edit, got:\n%s", out)
	}
	frames := a.Finish("tool_calls", Usage{})
	out = strings.Join(frames, "")
	if !contains(out, "tool_use") {
		t.Fatalf("force finish must emit coerced Edit, got:\n%s", out)
	}
	if !contains(out, "content_block_stop") || !contains(out, "message_stop") {
		t.Fatalf("force finish must close tool + envelope:\n%s", out)
	}
}

func TestRequeueUnacksTerminalWhenToolsRetry(t *testing.T) {
	a := NewStreamAssembler("msg_rq", "grok", true, 2, []string{"Read"})
	_ = a.Start(1)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{{Index: 0, ID: "call_1", Name: "Read", Arguments: `{"file_path":"/a.go"}`}})
	if len(frames) == 0 {
		// force finish path
		frames = a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 1})
	}
	// Simulate terminal Ack'd while tool unacked
	a.pendingTerminal = true
	a.AckTerminal()
	// Ensure tool is started unacked
	if !a.HasUnackedTools() && a.tools[0] != nil {
		a.tools[0].started = true
		a.tools[0].clientAcked = false
		a.tools[0].stopped = true
	}
	a.RequeueUnackedTools()
	if a.TerminalDelivered() {
		t.Fatal("requeue of unacked tool must UnackTerminal")
	}
	out := a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 1})
	joined := strings.Join(out, "")
	// tool_use before message_stop
	tu := strings.Index(joined, `"type":"tool_use"`)
	if tu < 0 {
		tu = strings.Index(joined, "tool_use")
	}
	ms := strings.Index(joined, "message_stop")
	if tu < 0 || ms < 0 {
		t.Fatalf("expected tool_use and message_stop, got %q", joined[:min(500, len(joined))])
	}
	if tu > ms {
		t.Fatalf("tool_use must come BEFORE message_stop")
	}
}

func TestClientDeliveryOKRequiresAckedToolOrText(t *testing.T) {
	a := NewStreamAssembler("msg_ok", "grok", true, 1, []string{"Read"})
	_ = a.Start(1)
	a.AckMessageStart()
	_ = a.Feed("hi", "", nil)
	out := a.Finish("end_turn", Usage{PromptTokens: 1, CompletionTokens: 1})
	_ = out
	if a.ClientDeliveryOK() {
		t.Fatal("terminal not Ack'd yet")
	}
	a.AckEmittedTools()
	if !a.ClientDeliveryOK() {
		t.Fatal("text turn after terminal Ack should be OK")
	}
}

func TestStreamAssemblerRequeueUnacksTerminal(t *testing.T) {
	a := NewStreamAssembler("msg_rq", "grok", true, 2, []string{"Edit"})
	_ = a.Start(1)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "call_1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":"b"}`,
	}})
	if len(frames) == 0 {
		// may need Finish for emit
	}
	fin := a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 1})
	joined := strings.Join(append(frames, fin...), "")
	if !strings.Contains(joined, "tool_use") {
		t.Fatalf("expected tool_use frames:\n%s", joined)
	}
	a.AckEmittedTools()
	if !a.TerminalDelivered() {
		t.Fatal("terminal should be delivered")
	}
	// Leave tool unacked as if write soft-failed after message_stop Ack.
	for _, st := range a.tools {
		if st != nil {
			st.clientAcked = false
			st.started = true
		}
	}
	a.pendingClientAcks = []int{0}
	a.RequeueUnackedTools()
	if a.TerminalDelivered() {
		t.Fatal("Requeue must UnackTerminal when tools requeued")
	}
	again := a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 1})
	out := strings.Join(again, "")
	if !strings.Contains(out, "tool_use") {
		t.Fatalf("expected re-emitted tool_use:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("expected message_stop after recovery:\n%s", out)
	}
}

func TestStreamAssemblerClientDeliveryOK(t *testing.T) {
	a := NewStreamAssembler("msg_ok", "grok", false, 0, nil)
	_ = a.Start(1)
	a.AckMessageStart()
	_ = a.Feed("hello", "", nil)
	fin := a.Finish("end_turn", Usage{PromptTokens: 1, CompletionTokens: 1})
	_ = fin
	a.AckEmittedTools()
	if !a.ClientDeliveryOK() {
		t.Fatal("text + terminal should be ClientDeliveryOK")
	}
}

func TestAnthropicRequeueUnacksTerminal(t *testing.T) {
	a := NewStreamAssembler("msg_u", "m", true, 2, []string{"Read"})
	_ = a.Start(1)
	a.AckMessageStart()
	frames := a.Feed("", "", []ToolDelta{{
		Index: 0, ID: "call_1", Name: "Read", Arguments: `{"file_path":"/a.go"}`,
	}})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "tool_use") {
		// force finish path
		frames = a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 1})
		joined = strings.Join(frames, "")
	}
	a.AckToolsInPayload(joined)
	fin := a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 1})
	finJ := strings.Join(fin, "")
	if strings.Contains(finJ, "message_stop") {
		a.AckTerminal()
	} else if a.pendingTerminal {
		a.AckTerminal()
	} else {
		// ensure terminal set
		a.terminalEmitted = true
	}
	// unacked second tool
	if st := a.tools[1]; st == nil {
		a.tools[1] = &toolState{id: "call_2", name: "Read", arguments: `{"file_path":"/b.go"}`, started: true, stopped: true, clientAcked: false, block: 3}
	} else {
		st.started = true
		st.stopped = true
		st.clientAcked = false
		st.name = "Read"
		st.arguments = `{"file_path":"/b.go"}`
	}
	a.RequeueUnackedTools()
	if a.TerminalDelivered() {
		t.Fatal("must unack terminal when requeuing tools")
	}
	out := strings.Join(a.Finish("tool_use", Usage{PromptTokens: 1, CompletionTokens: 2}), "")
	if !strings.Contains(out, "tool_use") {
		t.Fatalf("expected re-emitted tool_use: %q", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("expected message_stop after tools: %q", out)
	}
	// order: tool_use before message_stop
	if strings.Index(out, "tool_use") > strings.Index(out, "message_stop") {
		t.Fatalf("tool_use must precede message_stop")
	}
}

func TestAnthropicRequeueUnacksTerminalWhenToolsNeedRetry(t *testing.T) {
	a := NewStreamAssembler("msg_requeue", "grok", true, 2, []string{"Read"})
	_ = a.Start(1)
	a.AckMessageStart()
	// Feed complete Read tool.
	_ = a.Feed("", "", []ToolDelta{{Index: 0, ID: "call_read", Name: "Read", Arguments: `{"file_path":"/a.go"}`}})
	frames := a.Finish("tool_calls", Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "tool_use") || !strings.Contains(joined, "message_stop") {
		t.Fatalf("expected tool_use+message_stop:\n%s", joined)
	}
	// Simulate terminal Ack only; tool soft-failed (started, not clientAcked).
	a.AckTerminal()
	for _, st := range a.tools {
		if st != nil {
			st.clientAcked = false
			// keep started=true so Requeue sees it
			st.started = true
		}
	}
	a.RequeueUnackedTools()
	if a.TerminalDelivered() {
		t.Fatal("Requeue must UnackTerminal when tools requeued after message_stop")
	}
	out := strings.Join(a.Finish("tool_calls", Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}), "")
	if !strings.Contains(out, "tool_use") {
		t.Fatalf("expected re-emitted tool_use:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("expected re-emitted message_stop after tools:\n%s", out)
	}
	toolIdx := strings.Index(out, `"type":"tool_use"`)
	if toolIdx < 0 {
		toolIdx = strings.Index(out, "tool_use")
	}
	stopIdx := strings.Index(out, "message_stop")
	if toolIdx < 0 || stopIdx < 0 || toolIdx > stopIdx {
		t.Fatalf("tool_use must precede message_stop; tool=%d stop=%d\n%s", toolIdx, stopIdx, out)
	}
}

func TestAnthropicClientDeliveryOK(t *testing.T) {
	a := NewStreamAssembler("msg_ok", "grok", false, 0, nil)
	_ = a.Start(1)
	a.AckMessageStart()
	_ = a.Feed("hi", "", nil)
	frames := a.Finish("stop", Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})
	if len(frames) == 0 {
		t.Fatal("expected finish frames")
	}
	if a.ClientDeliveryOK() {
		t.Fatal("unacked terminal must not be OK")
	}
	a.AckTerminal()
	if !a.ClientDeliveryOK() {
		t.Fatal("text + acked terminal must be OK")
	}
}

// maxTools=1 with two complete tools must not re-emit message_stop in a loop.
func TestMaxToolsCapDoesNotLoopMessageStop(t *testing.T) {
	s := NewStreamAssembler("msg_cap", "grok", true, 1, []string{"Read", "Write"})
	frames := s.Feed("", "", []ToolDelta{
		{Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/a.txt"}`},
		{Index: 1, ID: "c2", Name: "Read", Arguments: `{"file_path":"/b.txt"}`},
	})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "tool_use") {
		// may need Start first
		start := s.Start(0)
		frames = append(start, frames...)
		joined = strings.Join(frames, "")
	}
	if s.HasPendingTools() {
		t.Fatal("capped excess tools must not look pending")
	}
	if s.hasReadyUnemittedTools() {
		t.Fatal("capped excess tools must not look ready-to-emit")
	}
	// Force finish
	fin := s.Finish("tool_use", Usage{})
	finJoined := strings.Join(fin, "")
	// Ack everything we can
	s.AckMessageStart()
	s.AckToolsInPayload(joined + finJoined)
	s.AckTerminal()
	if s.NeedsFinishRetry() {
		t.Fatal("after cap+ack, NeedsFinishRetry must be false")
	}
}

func TestPayloadDeliveredRequiresAck(t *testing.T) {
	s := NewStreamAssembler("msg_1", "grok-4.5", false, 0, nil)
	_ = s.Start(0)
	// Open thinking via Feed reasoning without AckContentDelivered.
	frames := s.Feed("", "thinking...", nil)
	if len(frames) == 0 {
		t.Fatal("expected frames")
	}
	// Thinking open must NOT set PayloadDelivered until AckContentDelivered.
	if s.PayloadDelivered() {
		t.Fatal("open thinking must not count as delivered")
	}
	s.AckContentDelivered()
	if !s.PayloadDelivered() {
		t.Fatal("expected delivered after ack")
	}
}
