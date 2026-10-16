package adapters

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"testing"
)

func TestAllToolsKeepSameSessionChoicesSeparate(t *testing.T) {
	c := configFixture(t)
	session := "same-session"
	tools := []string{"codex", "claude", "opencode", "kiro", "cursor", "gemini", "antigravity"}
	requests := map[string]memory.Object{}
	ids := map[string]bool{}
	for _, tool := range tools {
		request, e := prepareTool(c, tool, session, c.ProjectRoot, true)
		if e != nil || request["tool"] != tool || ids[memory.Text(request["request"])] {
			t.Fatal(tool, request, e)
		}
		ids[memory.Text(request["request"])] = true
		requests[tool] = request
	}
	for _, tool := range tools {
		repeated, e := prepareTool(c, tool, session, c.ProjectRoot, true)
		if e != nil || repeated["request"] != requests[tool]["request"] {
			t.Fatal("another client superseded pending choice", tool, repeated, e)
		}
	}
	for i, tool := range tools {
		selected := "a"
		if i%2 != 0 {
			selected = "none"
		}
		request := requests[tool]
		answer := memory.Object{"request": request["request"], "question_id": "synthetic-answer-" + tool, "session": session, "selected": selected}
		if _, e := Select(c, memory.Text(request["request"]), memory.Text(answer["question_id"]), answer); e != nil {
			t.Fatal(tool, e)
		}
	}
	s, _ := c.Store(false)
	for i, tool := range tools {
		b, e := s.Binding(tool, session)
		if e != nil {
			t.Fatal(e)
		}
		none, e := s.OptedOut(tool, session)
		if e != nil {
			t.Fatal(e)
		}
		if i%2 == 0 && (b == nil || b["task"] != "a" || none) {
			t.Fatal("explicit binding changed", tool, b, none)
		}
		if i%2 != 0 && (b != nil || !none) {
			t.Fatal("none choice changed", tool, b, none)
		}
		r, e := prepareTool(c, tool, session, c.ProjectRoot, true)
		if e != nil || r != nil {
			t.Fatal("answered client re-prompted", tool, r, e)
		}
	}
}

func TestAllToolCaptureRequiresChoiceAndSharesRawSemantics(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	tools := []string{"codex", "claude", "opencode", "kiro", "cursor", "gemini", "antigravity"}
	for _, tool := range tools {
		p := memory.Object{"hook_event_name": "UserPromptSubmit", "session_id": "same", "cwd": c.ProjectRoot, "event_id": "same-occurrence", "prompt": "unchosen"}
		if r, e := Capture(c, tool, p); e != nil || r != nil {
			t.Fatal(tool, r, e)
		}
		start := sessionStart(c, "same", "startup")
		if r, e := Capture(c, tool, start); e != nil || r["hookSpecificOutput"] == nil {
			t.Fatal(tool, r, e)
		}
		request, e := prepareTool(c, tool, "same", c.ProjectRoot, true)
		if e != nil {
			t.Fatal(e)
		}
		answer := memory.Object{"request": request["request"], "question_id": "synthetic-" + tool, "session": "same", "selected": "a"}
		if _, e := Select(c, memory.Text(request["request"]), memory.Text(answer["question_id"]), answer); e != nil {
			t.Fatal(e)
		}
		p["prompt"] = "chosen body token=fixture-private"
		if _, e := Capture(c, tool, p); e != nil {
			t.Fatal(e)
		}
		if _, e := Capture(c, tool, p); e != nil {
			t.Fatal(e)
		}
	}
	records, e := s.Records("a", "", 100, 0)
	if e != nil || len(records) != 7 {
		t.Fatal(records, e)
	}
	seen := map[string]bool{}
	for _, v := range records {
		seen[memory.Text(v["tool"])] = true
		if v["authority"] != "native-event" {
			t.Fatal(v)
		}
	}
	if len(seen) != 7 {
		t.Fatal("clients merged occurrence identity", seen)
	}
	if records, e := s.Records("a", "fixture-private", 100, 0); e != nil || len(records) != 0 {
		t.Fatal("redaction differs by client", records, e)
	}
}

func TestUnsupportedSelectionCannotWriteBinding(t *testing.T) {
	c := configFixture(t)
	if _, e := PrepareTool(c, "unknown", "same", c.ProjectRoot); e == nil {
		t.Fatal("unknown tool accepted")
	}
	s, _ := c.Store(false)
	if e := s.Bind("unknown", "same", "a", c.ProjectRoot); e == nil {
		t.Fatal("unknown binding accepted")
	}
}
