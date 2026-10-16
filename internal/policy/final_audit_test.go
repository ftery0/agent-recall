package policy

import (
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

func commitFixture(t *testing.T, root string) {
	t.Helper()
	for _, args := range [][]string{{"-C", root, "add", "."}, {"-C", root, "-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture", "commit", "-qm", "before"}} {
		if raw, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatal(string(raw), err)
		}
	}
}

func TestManualComponentCountUsesEachDirectoryOnce(t *testing.T) {
	for _, tc := range []struct{ old, added, expectedCode int }{{6, 2, 0}, {7, 1, 0}, {6, 3, 1}} {
		c := nativeFixture(t)
		for i := range tc.old {
			os.WriteFile(filepath.Join(c.Root, string(rune('A'+i))+".tsx"), []byte("export {};\n"), 0600)
		}
		commitFixture(t, c.Root)
		paths := []string{}
		for i := range tc.added {
			name := string(rune('K'+i)) + ".tsx"
			os.WriteFile(filepath.Join(c.Root, name), []byte("export {};\n"), 0600)
			paths = append(paths, name)
		}
		out, code, err := StructureCheck(c.Root, paths, "")
		if err != nil || code != tc.expectedCode || code == 0 && out != "" || code == 1 && !strings.Contains(out, "9개") {
			t.Fatal(tc, out, code, err)
		}
	}
}

func TestGitOutputOptionIsNeverReadOnlyEvidence(t *testing.T) {
	for _, command := range []string{"git diff --output=generated.py", "git diff --output generated.py", "git show --output=result.py", "git log --output=result.py"} {
		if _, complete := shellTargets(command); complete {
			t.Fatalf("write option treated as read-only: %s", command)
		}
	}
	if paths, complete := shellTargets("git diff --stat"); !complete || len(paths) != 0 {
		t.Fatal("ordinary diff read changed", paths, complete)
	}
	c := nativeFixture(t)
	os.WriteFile(filepath.Join(c.Root, "input.txt"), []byte(strings.Repeat("old\n", 220)), 0600)
	commitFixture(t, c.Root)
	os.WriteFile(filepath.Join(c.Root, "input.txt"), []byte(strings.Repeat("new\n", 220)), 0600)
	payload := memory.Object{"hook_event_name": "PreToolUse", "cwd": c.Root, "tool_name": "Bash", "tool_use_id": "one", "tool_input": memory.Object{"command": "git diff --output=generated.py"}}
	if _, err := c.Event(payload); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "diff", "--output=generated.py")
	cmd.Dir = c.Root
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	payload["hook_event_name"] = "PostToolUse"
	payload["tool_response"] = memory.Object{"exit_code": 0}
	if _, err := c.Event(payload); err != nil {
		t.Fatal(err)
	}
	out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
	if err != nil || out["edit_evidence_verified"] != false || out["systemMessage"] != nil || len(out["unverified"].([]string)) == 0 {
		t.Fatal(out, err)
	}
}

func boundedFileError(t *testing.T, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("nonregular file accepted")
		}
	case <-time.After(600 * time.Millisecond):
		t.Fatal("nonregular file read blocked")
	}
}

func TestSpecialFileReadsAreBoundedAndRegularSymlinksWork(t *testing.T) {
	c := nativeFixture(t)
	file := filepath.Join(c.Root, "regular.go")
	os.WriteFile(file, []byte("package fixture\n"), 0600)
	link := filepath.Join(c.Root, "link.go")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if raw, err := readRegularFile(link); err != nil || string(raw) != "package fixture\n" {
		t.Fatal(string(raw), err)
	}
	if digest, err := fingerprint(filepath.Join(c.Root, "missing.go")); err != nil || digest != "" {
		t.Fatal(digest, err)
	}
	fifo := filepath.Join(c.Root, "stream.go")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	fifoLink := filepath.Join(c.Root, "stream-link.go")
	os.Symlink(fifo, fifoLink)
	directory := filepath.Join(c.Root, "folder.go")
	os.Mkdir(directory, 0700)
	for _, path := range []string{fifo, fifoLink, directory} {
		boundedFileError(t, func() error { _, err := fingerprint(path); return err })
		boundedFileError(t, func() error { _, err := StructureMeasure(c.Root, []string{path}); return err })
		boundedFileError(t, func() error { _, _, err := StructureCheck(c.Root, []string{path}, ""); return err })
		boundedFileError(t, func() error {
			_, err := c.Event(memory.Object{"hook_event_name": "PreToolUse", "cwd": c.Root, "tool_name": "Write", "tool_input": memory.Object{"file_path": path}})
			return err
		})
	}
}

func TestSpecialMetadataReadsCannotBlockHooks(t *testing.T) {
	for _, kind := range []string{"state", "failure", "baseline"} {
		c := nativeFixture(t)
		state := freshState(c.Root)
		state.Observed = true
		path := c.path()
		if kind != "state" {
			memory.PrivateWrite(path, memory.Packed(state))
		}
		switch kind {
		case "failure":
			path = c.failurePath()
		case "baseline":
			path = filepath.Join(c.Runtime, "baseline.json")
		}
		os.MkdirAll(filepath.Dir(path), 0700)
		if err := syscall.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		boundedFileError(t, func() error {
			if kind == "baseline" {
				_, _, err := StructureCheck(c.Root, nil, path)
				return err
			}
			_, err := c.Event(memory.Object{"hook_event_name": "Stop"})
			return err
		})
	}
}

func TestStopDoesNotReadLegacyBlockingMetadata(t *testing.T) {
	c := nativeFixture(t)
	state := freshState(c.Root)
	state.Observed = true
	file := filepath.Join(c.Root, "long.go")
	os.WriteFile(file, []byte(strings.Repeat("x\n", 401)), 0600)
	digest, _ := fingerprint(file)
	state.Files["long.go"] = Change{Before: Metrics{}, After: digest}
	memory.PrivateWrite(c.path(), memory.Packed(state))
	os.Mkdir(filepath.Join(c.Root, ".claude"), 0700)
	os.WriteFile(filepath.Join(c.Root, ".claude", "checks.json"), memory.Packed(memory.Object{"checks": []any{memory.Object{"name": "fail", "command": "false", "pattern": `\.go$`}}}), 0600)
	seen := filepath.Join(c.Runtime, "structure-check", "codex-"+c.Session+".seen")
	os.MkdirAll(filepath.Dir(seen), 0700)
	if err := syscall.Mkfifo(seen, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
		if err == nil && (out["decision"] != nil || out["checks_status"] != "not_run" || !strings.Contains(memory.Text(out["systemMessage"]), "401줄")) {
			err = fmt.Errorf("unexpected Stop result: %v", out)
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(600 * time.Millisecond):
		t.Fatal("Stop read legacy blocking metadata")
	}
}

func TestManualSymlinkRootKeepsCanonicalFiles(t *testing.T) {
	c := nativeFixture(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(c.Root, alias); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(c.Root, "new.go")
	os.WriteFile(file, []byte(strings.Repeat("x\n", 401)), 0600)
	paths := []string{filepath.Join(alias, "new.go"), "./new.go", file}
	metrics, err := StructureMeasure(alias, paths)
	if err != nil || len(metrics) != 1 || metrics[0].Path != "new.go" || metrics[0].Lines != 401 {
		t.Fatal(metrics, err)
	}
	output, code, err := StructureCheck(alias, paths, "")
	if err != nil || code != 1 || strings.Count(output, "401줄") != 1 {
		t.Fatal(output, code, err)
	}
	outside := filepath.Join(t.TempDir(), "outside.go")
	os.WriteFile(outside, []byte("x\n"), 0600)
	os.Symlink(outside, filepath.Join(c.Root, "outside-link.go"))
	if records, err := StructureMeasure(alias, []string{"outside-link.go", "../outside.go", "deleted.go"}); err != nil || len(records) != 0 {
		t.Fatal(records, err)
	}
	if output, code, err := StructureCheck(alias, []string{"outside-link.go", "../outside.go", "deleted.go"}, ""); err != nil || code != 0 || output != "" {
		t.Fatal(output, code, err)
	}
}

func TestSpecialFailureMarkerCannotExtendLockDeadline(t *testing.T) {
	c := nativeFixture(t)
	lock, err := openRegularFile(strings.TrimSuffix(c.path(), ".json")+".lock", os.O_CREATE|os.O_RDWR)
	if os.IsNotExist(err) {
		os.MkdirAll(filepath.Dir(c.path()), 0700)
		lock, err = openRegularFile(strings.TrimSuffix(c.path(), ".json")+".lock", os.O_CREATE|os.O_RDWR)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err = syscall.Mkfifo(c.failurePath(), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan memory.Object, 1)
	go func() {
		out, _ := c.Event(memory.Object{"hook_event_name": "PreToolUse", "cwd": c.Root, "tool_name": "Write", "tool_input": memory.Object{"file_path": filepath.Join(c.Root, "lost.go")}})
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(memory.Text(out["systemMessage"]), "미확인") {
			t.Fatal(out)
		}
	case <-time.After(600 * time.Millisecond):
		t.Fatal("failure marker blocked beyond lock deadline")
	}
}

func TestStopStructureErrorsKeepDetailsAndUnresolvedNotice(t *testing.T) {
	c, p := collector(t)
	path := filepath.Join(c.Root, "one.go")
	content := strings.Repeat("x\n", 401)
	if _, err := c.Event(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	p["hook_event_name"] = "PostToolUse"
	if _, err := c.Event(p); err != nil {
		t.Fatal(err)
	}
	state, err := c.state()
	if err != nil {
		t.Fatal(err)
	}
	legacy := "연속 편집 소유권 미확인: legacy.go"
	state.Unverified = []string{legacy}
	if err := memory.PrivateWrite(c.path(), memory.Packed(state)); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "structure.sh")
	raw := "#!/bin/sh\ncase \"$(cat \"$1/mode\")\" in\nwarn) printf '[구조 검사]\\none.go: 401줄\\n'; exit 1;;\nerror) printf 'raw failure details\\nsecond line\\n'; exit 3;;\nchange) printf 'changed\\n' >> \"$2\"; exit 0;;\nok) exit 0;;\nesac\n"
	if err := os.WriteFile(script, []byte(raw), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_STRUCTURE_CHECK", script)
	stop := func(mode string) memory.Object {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.Root, "mode"), []byte(mode), 0600); err != nil {
			t.Fatal(err)
		}
		out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
		if err != nil || out["edit_evidence_verified"] != false || out["checks_status"] != "not_run" || out["passed"] != nil || out["decision"] != nil {
			t.Fatal("structure result changed edit or project-check evidence", out, err)
		}
		return out
	}
	first := stop("warn")
	message := memory.Text(first["systemMessage"])
	if !strings.Contains(message, "401줄") || strings.Contains(message, legacy) || first["structure"] != message {
		t.Fatal("structure output included hidden ownership notes", first)
	}
	state, err = c.state()
	if err != nil || state.LastNotice == "" || state.Cache != nil {
		t.Fatal("structure notice or cache evidence changed", state, err)
	}
	notice := state.LastNotice
	for repeat := 0; repeat < 2; repeat++ {
		out := stop("error")
		shown := memory.Text(out["systemMessage"])
		if !strings.Contains(shown, "종료 상태 3") || strings.Contains(shown, "raw failure") || strings.Contains(shown, legacy) || strings.Contains(shown, "\n") || len(shown) > 256 {
			t.Fatal("execution error was hidden or included raw evidence", out)
		}
	}
	state, err = c.state()
	if err != nil || state.LastNotice != notice || len(state.UnverifiedEvents) != 1 || len(state.Unverified) != 2 || state.Cache != nil {
		t.Fatal("error replaced the structure notice or duplicated evidence", state, err)
	}
	exported, err := c.Event(memory.Object{"hook_event_name": "Export"})
	if err != nil || !strings.Contains(strings.Join(exported["unverified"].([]string), "\n"), "raw failure details\nsecond line") {
		t.Fatal("export lost detailed execution failure", exported, err)
	}
	if out := stop("warn"); out["systemMessage"] != nil || out["structure"] != message {
		t.Fatal("execution error falsely resolved the structure notice", out)
	}
	out := stop("change")
	shown := memory.Text(out["systemMessage"])
	if !strings.Contains(shown, "검사 중") || strings.Contains(shown, legacy) || strings.Contains(shown, "one.go") || strings.Contains(shown, "\n") || len(shown) > 256 {
		t.Fatal("evidence change was hidden or included detailed paths", out)
	}
	state, err = c.state()
	if err != nil || state.LastNotice != notice || !strings.Contains(strings.Join(state.Unverified, "\n"), "검사 중 파일 또는 구조 근거가 바뀌어 소유권 미확인: one.go") {
		t.Fatal("evidence change resolved the notice or lost its detail", state, err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if out := stop("warn"); out["systemMessage"] != nil || out["structure"] != message {
		t.Fatal("evidence change falsely resolved the structure notice", out)
	}
	if out := stop("ok"); out["systemMessage"] != nil || out["structure"] != "" {
		t.Fatal("confirmed resolution kept a structure notice", out)
	}
	state, err = c.state()
	if err != nil || state.LastNotice != "" || state.Cache != nil {
		t.Fatal("confirmed result did not resolve display or cached unverified evidence", state, err)
	}
	if out := stop("warn"); memory.Text(out["systemMessage"]) != message {
		t.Fatal("resolved structure warning did not reappear", out)
	}
}

func TestStopIgnoresNonStructureDriftWhenConfirmingResolution(t *testing.T) {
	for _, path := range []string{"record.md", "generated/one.go", "types.d.ts"} {
		t.Run(path, func(t *testing.T) {
			c, _ := collector(t)
			file := filepath.Join(c.Root, "one.go")
			if err := os.WriteFile(file, []byte("short\n"), 0600); err != nil {
				t.Fatal(err)
			}
			digest, err := fingerprint(file)
			if err != nil {
				t.Fatal(err)
			}
			state := freshState(c.Root)
			state.Observed = true
			state.Files["one.go"] = Change{After: digest}
			state.Files[path] = Change{After: strings.Repeat("0", 64)}
			state.LastNotice = memory.Hash([]byte("previous structure notice"))
			if err := memory.PrivateWrite(c.path(), memory.Packed(state)); err != nil {
				t.Fatal(err)
			}
			out, err := c.Event(memory.Object{"hook_event_name": "Stop"})
			if err != nil || out["systemMessage"] != nil || out["structure"] != "" || out["edit_evidence_verified"] != false || len(out["unverified"].([]string)) != 1 {
				t.Fatal("unrelated drift changed clean structure evidence", out, err)
			}
			state, err = c.state()
			if err != nil || state.LastNotice != "" {
				t.Fatal("non-structure drift prevented confirmed resolution", state, err)
			}
		})
	}
}
