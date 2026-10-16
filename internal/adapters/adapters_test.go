package adapters

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"testing"
)

func configFixture(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	c := Config{Schema: 1, Category: "personal", Project: "project", ProjectRoot: root, DataRoot: filepath.Join(root, "data"), Tasks: []string{"a", "b"}, SourceRoots: []string{root}}
	if _, e := c.Store(true); e != nil {
		t.Fatal(e)
	}
	return c
}
func TestConfigCategories(t *testing.T) {
	root := t.TempDir()
	for category, ok := range map[string]bool{"personal": true, "company": true, "account": false, "": false} {
		path := filepath.Join(root, "config-"+category+".json")
		raw := `{"schema":1,"category":"` + category + `","project":"p","project_root":"` + root + `","data_root":"` + filepath.Join(root, "data") + `","tasks":["a"],"source_roots":[]}`
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := Load(path, "", root); (e == nil) != ok {
			t.Fatalf("category %q: err=%v", category, e)
		}
	}
}
func TestSelectionAndStaleReply(t *testing.T) {
	c := configFixture(t)
	old, e := Prepare(c, "session", c.ProjectRoot)
	if e != nil {
		t.Fatal(e)
	}
	latest, e := Prepare(c, "session", c.ProjectRoot)
	if e != nil {
		t.Fatal(e)
	}
	answer := memory.Object{"request": old["request"], "question_id": "q", "session": "session", "selected": "a"}
	if _, e = Select(c, memory.Text(old["request"]), "q", answer); e == nil {
		t.Fatal("stale accepted")
	}
	answer["request"] = latest["request"]
	if _, e = Select(c, memory.Text(latest["request"]), "q", answer); e != nil {
		t.Fatal(e)
	}
	if _, e = Select(c, memory.Text(latest["request"]), "q", answer); e == nil {
		t.Fatal("reused reply")
	}
	s, _ := c.Store(false)
	b, _ := s.Binding("codex", "session")
	if b["task"] != "a" {
		t.Fatal(b)
	}
}
func TestCaptureUsesToolIDsAndAvoidsRecursion(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	s.Bind("codex", "session", "a", c.ProjectRoot)
	p := memory.Object{"hook_event_name": "PostToolUse", "session_id": "session", "cwd": c.ProjectRoot, "turn_id": "turn", "tool_use_id": "one", "tool_input": memory.Object{"command": "harness memory recall --task a"}, "tool_response": "private recursive body"}
	if _, e := Capture(c, "codex", p); e != nil {
		t.Fatal(e)
	}
	p["tool_use_id"] = "two"
	if _, e := Capture(c, "codex", p); e != nil {
		t.Fatal(e)
	}
	records, _ := s.Records("a", "private recursive body", 100, 0)
	if len(records) != 0 {
		t.Fatal(records)
	}
	records, _ = s.Records("a", "", 100, 0)
	if len(records) != 2 {
		t.Fatal(records)
	}
	p["transcript_path"] = "changed"
	p["permission_mode"] = "new"
	if _, e := Capture(c, "codex", p); e != nil {
		t.Fatal(e)
	}
	h, _ := s.Health()
	if h["events"] != 2 {
		t.Fatal(h)
	}
}
func TestOutsideScopeDoesNotOpenDB(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	os.Remove(s.Path)
	out, e := Capture(c, "codex", memory.Object{"hook_event_name": "SessionStart", "session_id": "s", "cwd": t.TempDir()})
	if e != nil || out != nil {
		t.Fatal(out, e)
	}
}
func TestRedaction(t *testing.T) {
	v := Redact(memory.Object{"api_key": "hidden", "body": "Bearer abcdef token=secret-value"}).(map[string]any)
	if v["api_key"] != "[redacted]" || v["body"] != "Bearer [redacted] token=[redacted]" {
		t.Fatal(v)
	}
}
func TestNoneSelection(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	s.Bind("codex", "session", "a", c.ProjectRoot)
	r, _ := Prepare(c, "session", c.ProjectRoot)
	a := memory.Object{"request": r["request"], "question_id": "q", "session": "session", "selected": "none"}
	if _, e := Select(c, memory.Text(r["request"]), "q", a); e != nil {
		t.Fatal(e)
	}
	b, e := s.Binding("codex", "session")
	if e != nil || b != nil {
		t.Fatal(b, e)
	}
}

func TestHookTransportShapes(t *testing.T) {
	c := configFixture(t)
	p := memory.Object{"hook_event_name": "SessionStart", "session_id": "s", "cwd": c.ProjectRoot}
	r, e := Hook(c, true, "codex", p, t.TempDir())
	if e != nil || r["hookSpecificOutput"] == nil {
		t.Fatal(r, e)
	}
	r, e = Hook(c, true, "claude", p, t.TempDir())
	if e != nil || r["hookSpecificOutput"] == nil {
		t.Fatal("Claude should ask for a choice", r, e)
	}
	s, _ := c.Store(false)
	if b, e := s.Binding("claude", "s"); e != nil || b != nil {
		t.Fatal("Claude selected without an actual answer", b, e)
	}
	if _, e = Hook(c, true, "bad", p, t.TempDir()); e == nil {
		t.Fatal("unsupported adapter")
	}
}
