package adapters

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sessionStart(c Config, session, source string) memory.Object {
	return memory.Object{"hook_event_name": "SessionStart", "session_id": session, "cwd": c.ProjectRoot, "source": source}
}

func pendingSelection(t *testing.T, c Config, session string) memory.Object {
	t.Helper()
	s, e := c.Store(false)
	if e != nil {
		t.Fatal(e)
	}
	id, e := os.ReadFile(filepath.Join(s.Root, "selections", memory.Hash([]byte(session))+".current"))
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(filepath.Join(s.Root, "selections", string(id)+".json"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := memory.Decode(raw)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestSessionStartReusesUnansweredSelection(t *testing.T) {
	c := configFixture(t)
	var first memory.Object
	for _, source := range []string{"startup", "resume", "compact"} {
		out, e := Capture(c, "codex", sessionStart(c, "session", source))
		if e != nil || out["hookSpecificOutput"] == nil {
			t.Fatal(out, e)
		}
		r := pendingSelection(t, c, "session")
		if first == nil {
			first = r
		} else if string(memory.Packed(first)) != string(memory.Packed(r)) {
			t.Fatal("unanswered selection replaced", first, r)
		}
	}
	s, _ := c.Store(false)
	if binding, e := s.Binding("codex", "session"); e != nil || binding != nil {
		t.Fatal("hook bound without an actual response", binding, e)
	}
	answer := memory.Object{"request": first["request"], "question_id": "actual-question", "session": "session", "selected": "a"}
	if _, e := Select(c, memory.Text(first["request"]), "actual-question", answer); e != nil {
		t.Fatal("the original answer became stale after compact", e)
	}
}

func TestNoneSuppressesStartupResumeAndCompact(t *testing.T) {
	c := configFixture(t)
	r, e := Prepare(c, "session", c.ProjectRoot)
	if e != nil {
		t.Fatal(e)
	}
	answer := memory.Object{"request": r["request"], "question_id": "actual-question", "session": "session", "selected": "none"}
	if _, e = Select(c, memory.Text(r["request"]), "actual-question", answer); e != nil {
		t.Fatal(e)
	}
	for _, source := range []string{"startup", "resume", "compact"} {
		out, e := Capture(c, "codex", sessionStart(c, "session", source))
		if e != nil || out != nil {
			t.Fatal("opted-out session prompted again", source, out, e)
		}
	}
	out, e := Capture(c, "codex", sessionStart(c, "new-session", "startup"))
	if e != nil || out["hookSpecificOutput"] == nil {
		t.Fatal("opt-out leaked to another session", out, e)
	}
	// An explicit fresh question can change the choice without restoring old
	// execution permission or reusing the consumed response.
	r, e = Prepare(c, "session", c.ProjectRoot)
	if e != nil {
		t.Fatal(e)
	}
	answer["request"], answer["selected"] = r["request"], "b"
	if _, e = Select(c, memory.Text(r["request"]), "actual-question", answer); e != nil {
		t.Fatal(e)
	}
	s, _ := c.Store(false)
	if b, e := s.Binding("codex", "session"); e != nil || b["task"] != "b" {
		t.Fatal(b, e)
	}
}

func TestSessionSelectionRefreshesChangedScopeAndChoices(t *testing.T) {
	c := configFixture(t)
	first, e := prepare(c, "session", c.ProjectRoot, true)
	if e != nil {
		t.Fatal(e)
	}
	cwd := filepath.Join(c.ProjectRoot, "child")
	second, e := prepare(c, "session", cwd, true)
	if e != nil || second["request"] == first["request"] {
		t.Fatal("changed scope kept old request", first, second, e)
	}
	c.Tasks = []string{"b"}
	third, e := prepare(c, "session", cwd, true)
	if e != nil || third["request"] == second["request"] {
		t.Fatal("changed choices kept old request", second, third, e)
	}
	answer := memory.Object{"request": second["request"], "question_id": "q", "session": "session", "selected": "a"}
	if _, e = Select(c, memory.Text(second["request"]), "q", answer); e == nil {
		t.Fatal("superseded answer accepted")
	}
}

func TestWaitingHookSelectionRespectsCommittedChoice(t *testing.T) {
	for _, selected := range []string{"none", "a"} {
		t.Run(selected, func(t *testing.T) {
			c := configFixture(t)
			s, _ := c.Store(false)
			unlock, e := memory.Lock(filepath.Join(s.Root, "selections", ".lock"))
			if e != nil {
				t.Fatal(e)
			}
			type outcome struct {
				request memory.Object
				err     error
			}
			started := make(chan struct{})
			done := make(chan outcome, 1)
			go func() {
				close(started)
				r, e := prepare(c, "session", c.ProjectRoot, true)
				done <- outcome{r, e}
			}()
			<-started
			// Select holds this same selection lock when it records the actual
			// choice. The hook cannot pass its internal recheck until release.
			if selected == "none" {
				e = s.Unbind("codex", "session")
			} else {
				e = s.Bind("codex", "session", selected, c.ProjectRoot)
			}
			unlock()
			if e != nil {
				t.Fatal(e)
			}
			out := <-done
			if out.err != nil || out.request != nil {
				t.Fatal("waiting hook replaced an actual choice", out)
			}
			if _, e := os.Stat(filepath.Join(s.Root, "selections", memory.Hash([]byte("session"))+".current")); !os.IsNotExist(e) {
				t.Fatal("waiting hook wrote a stale prompt", e)
			}
		})
	}
}

func TestBoundCaptureAutomaticallyRecoversQueuedAndDuplicateEvents(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	if e := s.Bind("codex", "session", "a", c.ProjectRoot); e != nil {
		t.Fatal(e)
	}
	p := memory.Object{"hook_event_name": "PostToolUse", "session_id": "session", "cwd": c.ProjectRoot, "tool_use_id": "one", "tool_input": memory.Object{"command": "cat a.txt"}, "tool_response": "raw original"}
	unlock, e := memory.Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		started := time.Now()
		out, e := Capture(c, "codex", p)
		if e != nil || !strings.Contains(memory.Text(out["systemMessage"]), "대기열") || time.Since(started) > 2*time.Second {
			unlock()
			t.Fatal("capture lost event or waited too long", out, e, time.Since(started))
		}
	}
	unlock()
	p["tool_use_id"] = "two"
	if out, e := Capture(c, "codex", p); e != nil || out != nil {
		t.Fatal(out, e)
	}
	h, e := s.Health()
	if e != nil || h["events"] != 2 || h["pending"] != 0 || h["invalid"] != 0 || h["integrity"] != "ok" {
		t.Fatal(h, e)
	}
	records, e := s.Records("a", "raw original", 100, 0)
	if e != nil || len(records) != 2 {
		t.Fatal("raw content lost or duplicate stored", records, e)
	}
	records, e = s.Records("b", "", 100, 0)
	if e != nil || len(records) != 0 {
		t.Fatal("capture leaked across tasks", records, e)
	}
}

func TestRetrievalExclusionLeavesPayloadAndUnknownShapesIntact(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	if e := s.Bind("codex", "session", "a", c.ProjectRoot); e != nil {
		t.Fatal(e)
	}
	response := strings.Repeat("response-marker password=private-value ", 10000)
	for i, input := range []memory.Object{
		{"command": "harness memory brief --task a"},
		{"command": "harness memory brief --task a; cat result.txt"},
		{"code": "harness memory brief --task a"},
	} {
		p := memory.Object{"hook_event_name": "PostToolUse", "session_id": "session", "cwd": c.ProjectRoot, "tool_use_id": fmt.Sprint(i), "tool_input": input, "tool_response": response}
		if _, e := Capture(c, "codex", p); e != nil {
			t.Fatal(e)
		}
		if p["tool_response"] != response {
			t.Fatal("retrieval filtering mutated caller input")
		}
	}
	records, e := s.Records("a", "response-marker", 100, 0)
	if e != nil || len(records) != 2 {
		t.Fatal("compound or unsupported command output lost", len(records), e)
	}
	records, e = s.Records("a", "private-value", 100, 0)
	if e != nil || len(records) != 0 {
		t.Fatal("preserved output was not redacted", records, e)
	}
}
