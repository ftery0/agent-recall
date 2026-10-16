package adapters

import (
	"encoding/json"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestExactCWDDoesNotIncludeDescendantsOrSources(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "harness")
	if e := os.Mkdir(project, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(filepath.Join(root, "unrelated"), 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(t.TempDir(), "task-link")
	if e := os.Symlink(root, alias); e != nil {
		t.Fatal(e)
	}
	escape := filepath.Join(project, "escape")
	if e := os.Symlink(filepath.Join(root, "unrelated"), escape); e != nil {
		t.Fatal(e)
	}
	c := Config{ProjectRoot: project, ExactCWDs: []string{root}}
	for _, cwd := range []string{root, alias, filepath.Join(alias, "harness"), filepath.Join(project, "child")} {
		if !c.Permitted(cwd) {
			t.Fatalf("explicit scope rejected: %s", cwd)
		}
	}
	for _, cwd := range []string{"", filepath.Join(root, "unrelated"), filepath.Join(alias, "unrelated"), escape, filepath.Dir(root)} {
		if c.Permitted(cwd) {
			t.Fatalf("exact scope widened: %s", cwd)
		}
	}
	if c.SourceAllowed(filepath.Join(root, "handoff.md")) || c.SourceAllowed(filepath.Join(alias, "handoff.md")) {
		t.Fatal("exact cwd must not authorize parent source imports")
	}
	if !c.SourceAllowed(filepath.Join(project, "handoff.md")) {
		t.Fatal("existing project source scope lost")
	}
}

func TestLoadExactCWDAndQuotedRuntimeCommand(t *testing.T) {
	c := configFixture(t)
	exact := t.TempDir()
	c.ExactCWDs = []string{exact}
	path := filepath.Join(t.TempDir(), "config ' $HARNESS_QUOTE_TEST.json")
	write := func() {
		t.Helper()
		raw, e := json.Marshal(map[string]any{"schema": 2, "projects": []Config{c}})
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write()
	loaded, e := Load(path, "", exact)
	if e != nil || loaded.Project != c.Project || !reflect.DeepEqual(loaded.ExactCWDs, c.ExactCWDs) {
		t.Fatal(loaded, e)
	}
	command, e := loaded.memoryCommand()
	if e != nil {
		t.Fatal(e)
	}
	raw, e := exec.Command("/bin/sh", "-c", "set -- "+command+"; printf '%s\\n' \"$@\"").Output()
	if e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	want := []string{executable, "--config", path, "--project", c.Project, "memory"}
	if got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n"); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime command changed arguments: got %q want %q", got, want)
	}
	if strings.Contains(string(memory.Packed(loaded.Object())), path) {
		t.Fatal("runtime config path leaked into saved project config")
	}
	if _, e = Load(path, "", filepath.Join(exact, "child")); e == nil {
		t.Fatal("index selected project for unconfigured child")
	}
	c.ExactCWDs = []string{"relative"}
	write()
	if _, e = Load(path, c.Project, exact); e == nil {
		t.Fatal("relative exact cwd accepted")
	}
}

func TestExactCWDNativeMemoryLifecycle(t *testing.T) {
	c := configFixture(t)
	exact := t.TempDir()
	c.ExactCWDs = []string{exact}
	p := memory.Object{"hook_event_name": "SessionStart", "session_id": "native-session", "cwd": exact, "source": "startup"}
	out, e := Capture(c, "codex", p)
	if e != nil {
		t.Fatal(e)
	}
	context, ok := out["hookSpecificOutput"].(map[string]any)
	if !ok || context["hookEventName"] != "SessionStart" {
		t.Fatal("missing native model context", out)
	}
	command, e := c.memoryCommand()
	if e != nil || !strings.Contains(memory.Text(context["additionalContext"]), command+" selection --request") {
		t.Fatal("selection command missing absolute runtime", out, e)
	}
	request, e := Prepare(c, "native-session", exact)
	if e != nil {
		t.Fatal(e)
	}
	answer := memory.Object{"request": request["request"], "question_id": "actual-question", "session": "native-session", "selected": "a"}
	selected, e := Select(c, memory.Text(request["request"]), "actual-question", answer)
	if e != nil || !strings.Contains(memory.Text(selected["instruction"]), command+" brief --task a") {
		t.Fatal(selected, e)
	}
	for _, source := range []string{"startup", "resume", "clear", "compact"} {
		p["source"] = source
		out, e = Capture(c, "codex", p)
		if e != nil {
			t.Fatal(e)
		}
		context, ok = out["hookSpecificOutput"].(map[string]any)
		if !ok || context["hookEventName"] != "SessionStart" || !strings.Contains(memory.Text(context["additionalContext"]), command+" brief --task a") {
			t.Fatal("bound SessionStart lacks model-visible resume command", out)
		}
	}
	for i, event := range []string{"UserPromptSubmit", "PostToolUse", "Stop"} {
		p["hook_event_name"], p["turn_id"], p["tool_use_id"] = event, "turn", "tool"
		p["prompt"], p["tool_response"], p["last_assistant_message"] = "request", "result", "report"
		if _, e = Capture(c, "codex", p); e != nil {
			t.Fatal(event, e)
		}
		s, _ := c.Store(false)
		records, e := s.Records("a", "", 100, 0)
		if e != nil || len(records) != i+1 {
			t.Fatal(event, records, e)
		}
	}
	p["cwd"] = filepath.Join(exact, "unrelated")
	if out, e = Capture(c, "codex", p); e != nil || out != nil {
		t.Fatal("unrelated descendant captured", out, e)
	}
}
