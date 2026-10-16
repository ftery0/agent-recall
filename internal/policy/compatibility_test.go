package policy

import (
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func nativeFixture(t *testing.T) Collector {
	t.Helper()
	root := t.TempDir()
	if err := exec.Command("git", "init", "-q", root).Run(); err != nil {
		t.Fatal(err)
	}
	root, err := memory.Resolve(root)
	if err != nil {
		t.Fatal(err)
	}
	return Collector{Root: root, Runtime: t.TempDir(), Tool: "codex", Session: "native", compat: true}
}

func TestNativeContinuesPythonOwnedBaseline(t *testing.T) {
	c := nativeFixture(t)
	path := filepath.Join(c.Root, "renamed.go")
	os.WriteFile(path, []byte(strings.Repeat("x\n", 500)), 0600)
	digest, _ := fingerprint(path)
	state := memory.Object{"root": c.Root, "observed": true, "files": memory.Object{"renamed.go": memory.Object{"before": Metrics{Exists: false, Lines: 500}, "after": digest}}, "directories": memory.Object{".": 0}, "pending": memory.Object{}, "unverified": []string{}}
	if err := memory.PrivateWrite(c.path(), memory.Packed(state)); err != nil {
		t.Fatal(err)
	}
	out, err := EditScope(memory.Object{"hook_event_name": "Stop", "cwd": c.Root, "session_id": c.Session}, c.Tool, c.Runtime)
	if err != nil || out != nil {
		t.Fatal(out, err)
	}
	loaded, err := c.state()
	if err != nil || loaded.Files["renamed.go"].Before.Lines != 500 {
		t.Fatal(loaded, err)
	}
}

func TestNativeBusyWritePreservesFailureEvidence(t *testing.T) {
	c := nativeFixture(t)
	state := freshState(c.Root)
	state.Observed = true
	if err := memory.PrivateWrite(c.path(), memory.Packed(state)); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(strings.TrimSuffix(c.path(), ".json")+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, err := c.Event(memory.Object{"hook_event_name": "PreToolUse", "tool_name": "Write", "cwd": c.Root, "tool_input": memory.Object{"file_path": filepath.Join(c.Root, "lost.go")}})
	if err != nil || !strings.Contains(memory.Text(out["systemMessage"]), "잠금") || time.Since(start) > 600*time.Millisecond {
		t.Fatal(out, err, time.Since(start))
	}
	syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	out, err = c.Event(memory.Object{"hook_event_name": "Stop"})
	if err != nil || out["edit_evidence_verified"] != false || out["systemMessage"] != nil || !strings.Contains(strings.Join(out["unverified"].([]string), "\n"), "잠금") {
		t.Fatal(out, err)
	}
	state, err = c.state()
	if err != nil || !strings.Contains(strings.Join(state.Unverified, "\n"), "잠금") || len(state.UnverifiedEvents) != 1 {
		t.Fatal("lock failure evidence was not retained", state, err)
	}
	out, err = c.Event(memory.Object{"hook_event_name": "Export"})
	if err != nil || !strings.Contains(strings.Join(out["unverified"].([]string), "\n"), "잠금") {
		t.Fatal("export lost lock failure evidence", out, err)
	}
}

func TestNativeTypedCorruptionCannotBecomeEvidence(t *testing.T) {
	c := nativeFixture(t)
	for key, value := range map[string]any{"observed": nil, "directories": map[string]any{".": -1}, "unverified": nil, "unverified_events": nil, "pending": nil, "files": []any{}, "check_cache": map[string]any{"key": strings.Repeat("0", 64), "code": 0, "output": []any{}}} {
		raw := memory.Packed(freshState(c.Root))
		var state memory.Object
		json.Unmarshal(raw, &state)
		state[key] = value
		if err := memory.PrivateWrite(c.path(), memory.Packed(state)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Event(memory.Object{"hook_event_name": "Stop"}); err == nil {
			t.Fatalf("%s corruption accepted", key)
		}
	}
}

func TestLegacyUnverifiedStringsRemainEvidenceWithoutInventedCallIDs(t *testing.T) {
	c := nativeFixture(t)
	note := "범위·호출 근거 부족 또는 동시 변경: Bash"
	legacy := memory.Object{"root": c.Root, "observed": true, "files": memory.Object{}, "directories": memory.Object{}, "pending": memory.Object{}, "unverified": []string{note}}
	if err := memory.PrivateWrite(c.path(), memory.Packed(legacy)); err != nil {
		t.Fatal(err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
		if err != nil || out["edit_evidence_verified"] != false || out["checks_status"] != "not_run" || len(out["unverified"].([]string)) != 1 || out["systemMessage"] != nil {
			t.Fatal("legacy evidence was lost or displayed", repeat, out, err)
		}
	}
	state, err := c.state()
	if err != nil || len(state.UnverifiedEvents) != 0 || len(state.Unverified) != 1 || state.Unverified[0] != note {
		t.Fatal("a historical call identity was invented", state, err)
	}
	if _, err := c.Event(memory.Object{"hook_event_name": "PostToolUse", "tool_name": "Bash", "tool_use_id": "new-call", "tool_input": memory.Object{"command": "cp old.go new.go"}}); err != nil {
		t.Fatal(err)
	}
	out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
	if err != nil || out["systemMessage"] != nil || out["edit_evidence_verified"] != false || len(out["unverified"].([]string)) != 1 || out["unverified"].([]string)[0] != note {
		t.Fatal("new call lost or displayed historical evidence", out, err)
	}
	state, err = c.state()
	if err != nil || len(state.UnverifiedEvents) != 1 || len(state.Unverified) != 1 {
		t.Fatal("new call identity was not preserved separately", state, err)
	}
}

func TestInvalidUnverifiedEventMetadataCannotBecomeEvidence(t *testing.T) {
	c := nativeFixture(t)
	for _, events := range []any{memory.Object{"not-a-digest": "note"}, memory.Object{strings.Repeat("0", 64): "unrecorded note"}, memory.Object{strings.Repeat("0", 64): nil}} {
		state, err := memory.Decode(memory.Packed(freshState(c.Root)))
		if err != nil {
			t.Fatal(err)
		}
		state["unverified"] = []string{"note"}
		state["unverified_events"] = events
		if err := memory.PrivateWrite(c.path(), memory.Packed(state)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.state(); err == nil {
			t.Fatal("invalid event metadata accepted", events)
		}
	}
}

func TestManualBaselineCannotAttributeLaterExternalEdit(t *testing.T) {
	c := nativeFixture(t)
	path := filepath.Join(c.Root, "mine.go")
	os.WriteFile(path, []byte("x\n"), 0600)
	digest, _ := fingerprint(path)
	b := baseline{Root: c.Root, Files: map[string]Change{"mine.go": {Before: Metrics{}, After: digest}}, Directories: map[string]int{".": 0}}
	file := filepath.Join(c.Runtime, "baseline.json")
	memory.PrivateWrite(file, memory.Packed(b))
	os.WriteFile(path, []byte(strings.Repeat("external\n", 401)), 0600)
	output, code, err := StructureCheck(c.Root, []string{"mine.go"}, file)
	if err == nil || code != 2 || strings.Contains(output, "[구조 검사]") {
		t.Fatal(output, code, err)
	}
}

func TestManualBaselineRejectsCorruptComponentCount(t *testing.T) {
	c := nativeFixture(t)
	for i := range 9 {
		os.WriteFile(filepath.Join(c.Root, string(rune('A'+i))+".tsx"), []byte("export {};\n"), 0600)
	}
	path := "I.tsx"
	digest, _ := fingerprint(filepath.Join(c.Root, path))
	b := baseline{Root: c.Root, Files: map[string]Change{path: {Before: Metrics{Components: 8}, After: digest}}, Directories: map[string]int{".": 8}}
	file := filepath.Join(c.Runtime, "baseline.json")
	for _, invalid := range []any{map[string]any{".": -99}, map[string]any{".": nil}, map[string]any{".": 8.5}, map[string]any{"../outside": 8}, nil} {
		var raw memory.Object
		json.Unmarshal(memory.Packed(b), &raw)
		raw["directories"] = invalid
		if err := memory.PrivateWrite(file, memory.Packed(raw)); err != nil {
			t.Fatal(err)
		}
		output, code, err := StructureCheck(c.Root, []string{path}, file)
		if err == nil || code != 2 || output != "" {
			t.Fatalf("corrupt directory %v accepted: %s %d %v", invalid, output, code, err)
		}
	}
	memory.PrivateWrite(file, memory.Packed(b))
	output, code, err := StructureCheck(c.Root, []string{path}, file)
	if err != nil || code != 1 || !strings.Contains(output, "9개") {
		t.Fatal(output, code, err)
	}
}

func TestDiscontinuousEditKeepsLastVerifiedChangeAndNotice(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			c, p := collector(t)
			c.Tool = tool
			edit := func(path, id, content string) {
				t.Helper()
				p["tool_input"] = memory.Object{"file_path": filepath.Join(c.Root, path)}
				p["tool_use_id"] = id
				p["hook_event_name"] = "PreToolUse"
				if _, err := c.Event(p); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(c.Root, path), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				p["hook_event_name"] = "PostToolUse"
				if _, err := c.Event(p); err != nil {
					t.Fatal(err)
				}
			}
			content := strings.Repeat("a\n", 410)
			edit("one.go", "one-first", content)
			edit("two.go", "two-first", "short\n")
			first, err := c.Event(memory.Object{"hook_event_name": "Stop"})
			if err != nil || memory.Text(first["systemMessage"]) == "" {
				t.Fatal(first, err)
			}
			state, err := c.state()
			if err != nil {
				t.Fatal(err)
			}
			change, notice := state.Files["one.go"], state.LastNotice
			if err := os.WriteFile(filepath.Join(c.Root, "one.go"), []byte(strings.Repeat("b\n", 410)), 0600); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"one-discontinuous", "one-following"} {
				edit("one.go", id, id+"\n")
				state, err = c.state()
				if err != nil || len(state.Files) != 2 || state.Files["one.go"] != change || len(state.Unverified) != 1 {
					t.Fatal("unknown edit changed last verified metrics or hash", state, err)
				}
				out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
				if err != nil || out["systemMessage"] != nil || out["edit_evidence_verified"] != false || out["structure"] != "" || out["verified_files"] != 1 {
					t.Fatal("unknown edit became verified or was displayed", out, err)
				}
				state, err = c.state()
				if err != nil || state.LastNotice != notice {
					t.Fatal("unknown edit falsely resolved the notice", state, err)
				}
			}
			exported, err := c.Event(memory.Object{"hook_event_name": "Export"})
			if err != nil || len(exported["files"].([]string)) != 1 || len(exported["unverified"].([]string)) != 2 || len(state.UnverifiedEvents) != 2 {
				t.Fatal("export included unknown edit or lost its events", exported, state, err)
			}
			if err := os.WriteFile(filepath.Join(c.Root, "one.go"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
			if err != nil || out["systemMessage"] != nil || out["structure"] != first["structure"] || out["edit_evidence_verified"] != false || out["verified_files"] != 2 {
				t.Fatal("restored prior content repeated the notice or erased unknown evidence", out, err)
			}
			edit("one.go", "one-resolved", "short\n")
			out, err = c.Event(memory.Object{"hook_event_name": "Stop"})
			state, stateErr := c.state()
			if err != nil || stateErr != nil || out["structure"] != "" || out["systemMessage"] != nil || state.LastNotice != "" {
				t.Fatal("confirmed edit did not resolve structure", out, state, err, stateErr)
			}
			edit("one.go", "one-returned", content)
			out, err = c.Event(memory.Object{"hook_event_name": "Stop"})
			if err != nil || memory.Text(out["systemMessage"]) != memory.Text(first["systemMessage"]) || out["edit_evidence_verified"] != false {
				t.Fatal("confirmed recurrence was suppressed or erased unknown evidence", out, err)
			}
		})
	}
}

func TestCollectedDeletionRequiresContinuousEvidenceToResolveNotice(t *testing.T) {
	for _, continuous := range []bool{true, false} {
		t.Run(fmt.Sprint(continuous), func(t *testing.T) {
			c, p := collector(t)
			file := filepath.Join(c.Root, "one.go")
			if _, err := c.Event(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(strings.Repeat("a\n", 410)), 0600); err != nil {
				t.Fatal(err)
			}
			p["hook_event_name"] = "PostToolUse"
			if _, err := c.Event(p); err != nil {
				t.Fatal(err)
			}
			if out, err := c.Event(memory.Object{"hook_event_name": "Stop"}); err != nil || memory.Text(out["systemMessage"]) == "" {
				t.Fatal(out, err)
			}
			state, err := c.state()
			if err != nil {
				t.Fatal(err)
			}
			previous, notice := state.Files["one.go"], state.LastNotice
			if !continuous {
				if err := os.WriteFile(file, []byte("external\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p["tool_name"] = "apply_patch"
			p["tool_input"] = memory.Object{"command": "*** Delete File: one.go"}
			p["tool_use_id"] = "delete"
			p["hook_event_name"] = "PreToolUse"
			if _, err := c.Event(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			p["hook_event_name"] = "PostToolUse"
			if _, err := c.Event(p); err != nil {
				t.Fatal(err)
			}
			out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
			state, stateErr := c.state()
			if err != nil || stateErr != nil || out["systemMessage"] != nil || out["structure"] != "" || out["edit_evidence_verified"] != continuous {
				t.Fatal("deletion changed edit evidence or display", out, state, err, stateErr)
			}
			if continuous {
				if out["verified_files"] != 1 || state.Files["one.go"].Before != previous.Before || state.Files["one.go"].After != "" || state.LastNotice != "" {
					t.Fatal("verified deletion lost its baseline or did not resolve structure", out, state)
				}
			} else if out["verified_files"] != 0 || state.Files["one.go"] != previous || state.LastNotice != notice || len(state.Unverified) != 1 {
				t.Fatal("unknown deletion erased verified evidence or resolved structure", out, state)
			}
		})
	}
}
