package policy

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScopeDoesNotExpandOrRestore(t *testing.T) {
	root := t.TempDir()
	s := Scope{Root: root, Runtime: t.TempDir(), Task: "task", Session: "session"}
	path := filepath.Join(root, "one.go")
	g := memory.Object{"task": "task", "session": "session", "action": "edit", "paths": []any{path}, "source": "event:user-request"}
	if _, e := s.Update("grant", g); e != nil {
		t.Fatal(e)
	}
	s.Update("status", nil)
	v, e := s.Check("edit", path)
	if e != nil || v["decision"] != "in-scope" {
		t.Fatal(v, e)
	}
	v, _ = s.Check("push", "git push")
	if v["decision"] != "question-required" {
		t.Fatal(v)
	}
	other := s
	other.Session = "new"
	v, _ = other.Check("edit", path)
	if v["decision"] != "question-required" {
		t.Fatal(v)
	}
	s.Update("hold", nil)
	v, _ = s.Check("edit", path)
	if v["decision"] != "question-required" {
		t.Fatal(v)
	}
	if _, e = s.Update("resume", memory.Object{}); e == nil {
		t.Fatal("unconfirmed resume")
	}
	s.Update("resume", memory.Object{"task": "task", "session": "session", "question_id": "actual-q"})
	v, _ = s.Check("edit", path)
	if v["decision"] != "in-scope" {
		t.Fatal(v)
	}
}
func collector(t *testing.T) (Collector, memory.Object) {
	t.Helper()
	root := t.TempDir()
	root, err := memory.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	c := Collector{Root: root, Runtime: t.TempDir(), Tool: "codex", Session: "session", Task: "task"}
	p := memory.Object{"hook_event_name": "PreToolUse", "cwd": root, "session_id": "session", "tool_name": "Write", "tool_use_id": "one", "turn_id": "turn", "tool_input": memory.Object{"file_path": filepath.Join(root, "one.go")}}
	return c, p
}
func TestCollectorReportsLongFileWithoutBlocking(t *testing.T) {
	c, p := collector(t)
	if _, e := c.Event(p); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(c.Root, "one.go"), []byte(strings.Repeat("a\n", 410)), 0600)
	p["hook_event_name"] = "PostToolUse"
	if _, e := c.Event(p); e != nil {
		t.Fatal(e)
	}
	p["hook_event_name"] = "Stop"
	out, e := c.Event(p)
	if e != nil || out["decision"] != nil || !strings.Contains(memory.Text(out["systemMessage"]), "410줄") {
		t.Fatal(out, e)
	}
}

func TestStopDoesNotRunRegisteredChecks(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			c, p := collector(t)
			c.Tool = tool
			if tool == "claude" {
				delete(p, "turn_id")
			}
			os.Mkdir(filepath.Join(c.Root, ".claude"), 0700)
			cfg := memory.Object{"checks": []any{memory.Object{"name": "fixture", "command": "printf x >> check-runs; false", "pattern": `\.go$`}}}
			config := filepath.Join(c.Root, ".claude", "checks.json")
			os.WriteFile(config, memory.Packed(cfg), 0600)
			path := filepath.Join(c.Root, "one.go")
			for i, content := range []string{"first\n", "second\n"} {
				p["tool_use_id"] = content
				p["hook_event_name"] = "PreToolUse"
				if _, err := c.Event(p); err != nil {
					t.Fatal(err)
				}
				os.WriteFile(path, []byte(content), 0600)
				p["hook_event_name"] = "PostToolUse"
				if _, err := c.Event(p); err != nil {
					t.Fatal(err)
				}
				p["hook_event_name"] = "Stop"
				for repeat := 0; repeat < 2; repeat++ {
					p["stop_hook_active"] = repeat != 0
					out, err := c.Event(p)
					if err != nil || out["decision"] != nil || out["reason"] != nil || out["passed"] != nil || out["checks_status"] != "not_run" || len(out["checks"].([]memory.Object)) != 0 || out["edit_evidence_verified"] != true {
						t.Fatal(i, repeat, out, err)
					}
				}
			}
			if _, err := os.Stat(filepath.Join(c.Root, "check-runs")); !os.IsNotExist(err) {
				t.Fatal("Stop executed a project command", err)
			}
			if raw, err := os.ReadFile(config); err != nil || string(raw) != string(memory.Packed(cfg)) {
				t.Fatal("Stop changed the project registry", string(raw), err)
			}
			checks, err := RunChecks(Scope{Root: c.Root}, []string{path})
			if err != nil || len(checks) != 1 || checks[0]["executed"] != true || checks[0]["passed"] != false || checks[0]["exit_code"] != 1 {
				t.Fatal("explicit failed check lost its result", checks, err)
			}
			if raw, err := os.ReadFile(filepath.Join(c.Root, "check-runs")); err != nil || string(raw) != "x" {
				t.Fatal("explicit check did not run once", string(raw), err)
			}
			os.WriteFile(config, []byte("{"), 0600)
			if out, err := c.Event(p); err != nil || out["checks_status"] != "not_run" || out["edit_evidence_verified"] != true {
				t.Fatal("Stop still reads project check configuration", out, err)
			}
		})
	}
}
func TestMissingCollectorAndUnmatched(t *testing.T) {
	c, p := collector(t)
	p["hook_event_name"] = "Stop"
	r, e := c.Event(p)
	if e != nil || r["edit_evidence_verified"] != false {
		t.Fatal(r, e)
	}
	p["hook_event_name"] = "PreToolUse"
	c.Event(p)
	p["hook_event_name"] = "Stop"
	r, e = c.Event(p)
	if e != nil || r["edit_evidence_verified"] != false {
		t.Fatal(r, e)
	}
}
func TestMoveKeepsLengthBaseline(t *testing.T) {
	c, p := collector(t)
	os.WriteFile(filepath.Join(c.Root, "old.go"), []byte(strings.Repeat("x\n", 410)), 0600)
	p["tool_name"] = "apply_patch"
	p["tool_input"] = memory.Object{"command": "*** Update File: old.go\n*** Move to: new.go"}
	if _, e := c.Event(p); e != nil {
		t.Fatal(e)
	}
	os.Rename(filepath.Join(c.Root, "old.go"), filepath.Join(c.Root, "new.go"))
	p["hook_event_name"] = "PostToolUse"
	c.Event(p)
	p["hook_event_name"] = "Stop"
	out, e := c.Event(p)
	if e != nil || len(out["violations"].([]string)) != 0 {
		t.Fatal(out, e)
	}
}
func TestUnsupportedWriteNeverPasses(t *testing.T) {
	c, p := collector(t)
	p["tool_name"] = "Bash"
	p["tool_input"] = memory.Object{"command": "cp old.go new.go"}
	c.Event(p)
	p["hook_event_name"] = "PostToolUse"
	c.Event(p)
	p["hook_event_name"] = "Stop"
	out, e := c.Event(p)
	if e != nil || out["edit_evidence_verified"] != false {
		t.Fatal(out, e)
	}
}
func TestExternalEditNotAttributed(t *testing.T) {
	c, p := collector(t)
	c.Event(p)
	path := filepath.Join(c.Root, "one.go")
	os.WriteFile(path, []byte("first\n"), 0600)
	p["hook_event_name"] = "PostToolUse"
	c.Event(p)
	os.WriteFile(path, []byte("external\n"), 0600)
	p["hook_event_name"] = "Stop"
	r, e := c.Event(p)
	if e != nil || r["verified_files"] != 0 || r["edit_evidence_verified"] != false {
		t.Fatal(r, e)
	}
}
func TestChecksListedInRepoRun(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, ".claude"), 0700)
	path := filepath.Join(root, "a.go")
	os.WriteFile(path, []byte("package a"), 0600)
	config := memory.Object{"checks": []any{memory.Object{"name": "fixture", "command": "printf passed > marker", "pattern": `\.go$`}}}
	os.WriteFile(filepath.Join(root, ".claude/checks.json"), memory.Packed(config), 0600)
	s := Scope{Root: root, Runtime: t.TempDir(), Task: "t", Session: "s"}
	r, e := RunChecks(s, []string{path})
	if e != nil || len(r) != 1 || r[0]["passed"] != true {
		t.Fatal(r, e)
	}
	if _, e = os.Stat(filepath.Join(root, "marker")); e != nil {
		t.Fatal("listed check did not run")
	}
	r, e = RunChecks(s, []string{filepath.Join(root, "a.md")})
	if e != nil || len(r) != 0 {
		t.Fatal("unmatched file ran a check", r, e)
	}
}
func TestCorruptStateIsError(t *testing.T) {
	c, p := collector(t)
	os.WriteFile(c.path(), []byte("{"), 0600)
	if _, e := c.Event(p); e == nil {
		t.Fatal("corrupt state passed")
	}
}
func TestFolderDeletedCount(t *testing.T) {
	root := t.TempDir()
	files := map[string]Change{}
	for _, name := range []string{"new1.tsx", "new2.tsx", "new3.tsx"} {
		os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0600)
		files[name] = Change{Before: Metrics{Exists: false, Components: 8}, After: "exists"}
	}
	files["old1.tsx"] = Change{Before: Metrics{Exists: true}, After: ""}
	files["old2.tsx"] = Change{Before: Metrics{Exists: true}, After: ""}
	v, e := violations(root, files, map[string]int{".": 8})
	if e != nil || len(v) != 1 || !strings.Contains(v[0], "9개") {
		t.Fatal(v, e)
	}
}

func TestNonCodeFileChecksRequireExplicitInvocation(t *testing.T) {
	c, p := collector(t)
	file := filepath.Join(c.Root, "ko.json")
	p["tool_input"] = memory.Object{"file_path": file}
	os.Mkdir(filepath.Join(c.Root, ".claude"), 0700)
	cfg := memory.Object{"checks": []any{memory.Object{"name": "json-check", "command": "false", "pattern": "ko.json"}}}
	os.WriteFile(filepath.Join(c.Root, ".claude/checks.json"), memory.Packed(cfg), 0600)
	if _, e := c.Event(p); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(file, []byte("{}"), 0600)
	p["hook_event_name"] = "PostToolUse"
	c.Event(p)
	p["hook_event_name"] = "Stop"
	r, e := c.Event(p)
	if e != nil || r["decision"] != nil || r["checks_status"] != "not_run" || r["edit_evidence_verified"] != true {
		t.Fatal(r, e)
	}
	checks, e := RunChecks(Scope{Root: c.Root}, []string{file})
	if e != nil || len(checks) != 1 || checks[0]["passed"] != false {
		t.Fatal("explicit non-code check lost its failure", checks, e)
	}
}

func TestFailedCommandDoesNotBecomeVerified(t *testing.T) {
	c, p := collector(t)
	if _, e := c.Event(p); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(c.Root, "one.go"), []byte("partial write"), 0600)
	p["hook_event_name"] = "PostToolUse"
	p["tool_response"] = memory.Object{"exit_code": 1}
	if _, e := c.Event(p); e != nil {
		t.Fatal(e)
	}
	p["hook_event_name"] = "Stop"
	r, e := c.Event(p)
	if e != nil || r["verified_files"] != 0 || r["edit_evidence_verified"] != false {
		t.Fatal(r, e)
	}
}

func TestSedSideEffectsStayUnverified(t *testing.T) {
	for _, command := range []string{"sed -i '' 's/a/b/' a.go b.go", "sed 'w hidden.go' a.go", "sed -i 's/a/b/' a.go"} {
		if _, complete := shellTargets(command); complete {
			t.Fatal(command)
		}
	}
}

func TestStopWithoutEditEvidenceDoesNotClaimChecksPassed(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		c, _ := collector(t)
		c.Tool = tool
		out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
		if err != nil || out["passed"] != nil || out["checks_status"] != "not_run" || out["edit_evidence_verified"] != false || len(out["unverified"].([]string)) == 0 || out["systemMessage"] != nil {
			t.Fatal(tool, out, err)
		}
	}
}

func TestStopDriftStaysInternalWithoutDroppingEvidence(t *testing.T) {
	c, p := collector(t)
	path := filepath.Join(c.Root, "one.go")
	if _, err := c.Event(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("verified\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p["hook_event_name"] = "PostToolUse"
	if _, err := c.Event(p); err != nil {
		t.Fatal(err)
	}
	stop := func() memory.Object {
		t.Helper()
		p["hook_event_name"] = "Stop"
		out, err := c.Event(p)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("external one\n")
	first := stop()
	if first["systemMessage"] != nil || first["edit_evidence_verified"] != false || len(first["unverified"].([]string)) != 1 {
		t.Fatal("first drift was lost or displayed", first)
	}
	second := stop()
	if second["systemMessage"] != nil || second["edit_evidence_verified"] != false || second["verified_files"] != 0 || len(second["unverified"].([]string)) != 1 {
		t.Fatal("repeated display changed drift evidence", second)
	}
	p["hook_event_name"] = "Export"
	exported, err := c.Event(p)
	if err != nil || len(exported["unverified"].([]string)) != 1 || len(exported["files"].([]string)) != 0 {
		t.Fatal("export lost the unverified edit", exported, err)
	}
	write("external two\n")
	if out := stop(); out["systemMessage"] != nil || out["edit_evidence_verified"] != false || len(out["unverified"].([]string)) != 1 {
		t.Fatal("new content lost or displayed drift evidence", out)
	}
	if out := stop(); out["systemMessage"] != nil {
		t.Fatal("unchanged new content repeated", out)
	}
	if err := os.MkdirAll(filepath.Join(c.Root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, ".claude", "checks.json"), []byte(`{"checks":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if out := stop(); out["systemMessage"] != nil {
		t.Fatal("project check policy renewed an unrelated edit notice", out)
	}
	write("verified\n")
	if out := stop(); out["edit_evidence_verified"] != true || out["systemMessage"] != nil || len(out["unverified"].([]string)) != 0 {
		t.Fatal("resolved drift was not clear", out)
	}
	write("external two\n")
	if out := stop(); out["systemMessage"] != nil || out["edit_evidence_verified"] != false || len(out["unverified"].([]string)) != 1 {
		t.Fatal("returning drift was lost or displayed", out)
	}
}

func TestStopStructureNoticePreservesNewEditsWithoutRunningChecks(t *testing.T) {
	c, p := collector(t)
	path := filepath.Join(c.Root, "one.go")
	if err := os.MkdirAll(filepath.Join(c.Root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	config := memory.Object{"checks": []any{memory.Object{"name": "fixture", "command": "printf x >> check-runs", "pattern": `\.go$`}}}
	if err := os.WriteFile(filepath.Join(c.Root, ".claude", "checks.json"), memory.Packed(config), 0600); err != nil {
		t.Fatal(err)
	}
	edit := func(text string) {
		t.Helper()
		p["hook_event_name"] = "PreToolUse"
		if _, err := c.Event(p); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		p["hook_event_name"] = "PostToolUse"
		if _, err := c.Event(p); err != nil {
			t.Fatal(err)
		}
	}
	stop := func() memory.Object {
		t.Helper()
		p["hook_event_name"] = "Stop"
		out, err := c.Event(p)
		if err != nil {
			t.Fatal(err)
		}
		checks := out["checks"].([]memory.Object)
		if len(checks) != 0 || out["checks_status"] != "not_run" || out["passed"] != nil || out["decision"] != nil {
			t.Fatal("Stop claimed project checks", out)
		}
		return out
	}
	edit(strings.Repeat("a\n", 410))
	first := stop()
	message := memory.Text(first["systemMessage"])
	second := stop()
	if message == "" || second["systemMessage"] != nil || second["structure"] != first["structure"] || second["edit_evidence_verified"] != first["edit_evidence_verified"] {
		t.Fatal("repeated structure display changed result", first, second)
	}
	if _, err := os.Stat(filepath.Join(c.Root, "check-runs")); !os.IsNotExist(err) {
		t.Fatal("Stop ran project checks", err)
	}
	edit(strings.Repeat("b\n", 410))
	if out := stop(); out["systemMessage"] != nil || out["structure"] != first["structure"] || out["edit_evidence_verified"] != true {
		t.Fatal("same structure text repeated or new edit was lost", out)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if out := stop(); out["systemMessage"] != nil || out["verified_files"] != 0 || out["edit_evidence_verified"] != false || len(out["unverified"].([]string)) != 1 {
		t.Fatal("unavailable file was treated as verified", out)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("b\n", 410)), 0600); err != nil {
		t.Fatal(err)
	}
	if out := stop(); out["systemMessage"] != nil || out["structure"] != first["structure"] {
		t.Fatal("unavailable structure check resolved an existing notice", out)
	}
	p["tool_input"] = memory.Object{"file_path": filepath.Join(c.Root, "two.go")}
	p["tool_use_id"] = "two"
	p["hook_event_name"] = "PreToolUse"
	if _, err := c.Event(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Root, "two.go"), []byte("short\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p["hook_event_name"] = "PostToolUse"
	if _, err := c.Event(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("external\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out := stop(); out["systemMessage"] != nil || out["structure"] != "" || out["verified_files"] != 1 || out["edit_evidence_verified"] != false {
		t.Fatal("unavailable structure file was treated as verified", out)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("b\n", 410)), 0600); err != nil {
		t.Fatal(err)
	}
	if out := stop(); out["systemMessage"] != nil || out["structure"] != first["structure"] {
		t.Fatal("a clean subset resolved an unavailable structure warning", out)
	}
	p["tool_input"] = memory.Object{"file_path": path}
	p["tool_use_id"] = "one"
	edit(strings.Repeat("c\n", 411))
	if out := stop(); !strings.Contains(memory.Text(out["systemMessage"]), "411줄") || out["edit_evidence_verified"] != true {
		t.Fatal("new structure result was suppressed", out)
	}
	edit("short\n")
	if out := stop(); out["systemMessage"] != nil || out["structure"] != "" {
		t.Fatal("resolved structure warning remained", out)
	}
	edit(strings.Repeat("b\n", 410))
	if out := stop(); memory.Text(out["systemMessage"]) != message {
		t.Fatal("structure warning returning after resolution was suppressed", out)
	}
}

func TestStopUnverifiedCallsStayInternalWithoutDroppingEvidence(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		for _, nativeID := range []bool{true, false} {
			t.Run(tool+"/native-id="+fmt.Sprint(nativeID), func(t *testing.T) {
				c, p := collector(t)
				c.Tool = tool
				p["tool_name"] = "Bash"
				p["tool_input"] = memory.Object{"command": "cp old.go new.go"}
				if !nativeID {
					delete(p, "tool_use_id")
				}
				for _, id := range []string{"unknown-one", "unknown-two"} {
					if nativeID {
						p["tool_use_id"] = id
					}
					for _, event := range []string{"PreToolUse", "PostToolUse"} {
						p["hook_event_name"] = event
						if _, err := c.Event(p); err != nil {
							t.Fatal(err)
						}
					}
					p["hook_event_name"] = "Stop"
					first, err := c.Event(p)
					if err != nil || first["systemMessage"] != nil || first["edit_evidence_verified"] != false || len(first["unverified"].([]string)) != 1 || first["decision"] != nil {
						t.Fatal("new unverified call was lost or displayed", first, err)
					}
					second, err := c.Event(p)
					if err != nil || second["systemMessage"] != nil || second["edit_evidence_verified"] != false || len(second["unverified"].([]string)) != 1 {
						t.Fatal("repeated display changed unverified evidence", second, err)
					}
				}
				state, err := c.state()
				if err != nil || len(state.Unverified) != 1 || len(state.UnverifiedEvents) != 2 {
					t.Fatal("distinct observations were merged", state, err)
				}
				if nativeID {
					p["hook_event_name"] = "PostToolUse"
					if _, err := c.Event(p); err != nil {
						t.Fatal(err)
					}
					p["hook_event_name"] = "Stop"
					if out, err := c.Event(p); err != nil || out["systemMessage"] != nil {
						t.Fatal("duplicate native call was treated as a new event", out, err)
					}
				}
			})
		}
	}
}

func TestStopPendingCallsStayInternalAndKeepIdentities(t *testing.T) {
	c, p := collector(t)
	for _, id := range []string{"pending-one", "pending-two"} {
		p["tool_use_id"] = id
		p["hook_event_name"] = "PreToolUse"
		if _, err := c.Event(p); err != nil {
			t.Fatal(err)
		}
		p["hook_event_name"] = "Stop"
		first, err := c.Event(p)
		if err != nil || first["systemMessage"] != nil || first["edit_evidence_verified"] != false || len(first["unverified"].([]string)) != 1 {
			t.Fatal("pending event was lost or displayed", first, err)
		}
		second, err := c.Event(p)
		if err != nil || second["systemMessage"] != nil || len(second["unverified"].([]string)) != 1 || second["edit_evidence_verified"] != false {
			t.Fatal("same pending event repeated or lost its evidence", second, err)
		}
	}
	state, err := c.state()
	if err != nil || len(state.Pending) != 0 || len(state.UnverifiedEvents) != 2 {
		t.Fatal("pending event identities were lost", state, err)
	}
}

func TestCorruptNoticeEvidenceIsError(t *testing.T) {
	c, _ := collector(t)
	state, err := memory.Decode(memory.Packed(freshState(c.Root)))
	if err != nil {
		t.Fatal(err)
	}
	state["last_notice"] = "not-a-digest"
	if err := os.WriteFile(c.path(), memory.Packed(state), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.state(); err == nil {
		t.Fatal("corrupt notice evidence was accepted")
	}
}
