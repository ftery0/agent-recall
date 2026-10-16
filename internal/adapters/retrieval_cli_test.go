package adapters

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func retrievalFixture(t *testing.T) (Config, *memory.Store, string) {
	t.Helper()
	c := configFixture(t)
	s, e := c.Store(false)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 7; i++ {
		_, e = s.Ingest(memory.Object{"key": fmt.Sprintf("raw-%d", i), "task": "a", "session": "s", "kind": "raw", "subject": "request", "body": fmt.Sprintf("기억 원문 %d", i), "source_keys": []any{}})
		if e != nil {
			t.Fatal(e)
		}
	}
	id, e := s.Artifact([]byte("가나다🙂마바"), "synthetic://source")
	if e != nil {
		t.Fatal(e)
	}
	return c, s, id
}

func TestLegacyRetrievalOutputUnchanged(t *testing.T) {
	c, s, id := retrievalFixture(t)
	recall, _ := s.Recall("a")
	history, _ := s.Records("a", "", 100, 0)
	search, _ := s.Records("a", "원문", 100, 0)
	page, _ := s.Records("a", "원문", 2, 2)
	source, body, _ := s.Source(id)
	source["body"] = string(body)
	for _, tc := range []struct {
		args []string
		want any
	}{
		{[]string{"recall", "--task", "a"}, recall},
		{[]string{"history", "--task", "a"}, history},
		{[]string{"search", "원문", "--task", "a"}, search},
		{[]string{"search", "--task", "a", "원문", "--limit", "2", "--after", "2"}, page},
		{[]string{"source", id}, source},
	} {
		got, e := MemoryCLI(c, tc.args)
		if e != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%v: got %v want %v: %v", tc.args, got, tc.want, e)
		}
	}
	output := filepath.Join(t.TempDir(), "source.txt")
	got, e := MemoryCLI(c, []string{"source", id, "--output", output})
	if e != nil || got.(memory.Object)["body"] != nil {
		t.Fatal(got, e)
	}
	written, e := os.ReadFile(output)
	if e != nil || string(written) != string(body) {
		t.Fatal(string(written), e)
	}
	if _, e = MemoryCLI(c, []string{"source", id, "--output", output}); e == nil {
		t.Fatal("source output overwrote existing file")
	}
}

func TestBriefCLIAndSearchPaging(t *testing.T) {
	c, s, _ := retrievalFixture(t)
	want, e := s.Brief("a", 6000)
	if e != nil {
		t.Fatal(e)
	}
	got, e := MemoryCLI(c, []string{"brief", "--task", "a"})
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, want, e)
	}
	got, e = MemoryCLI(c, []string{"brief", "--budget", "1", "--task", "a"})
	if e != nil || got.(memory.Object)["over_budget"] != true {
		t.Fatal(got, e)
	}
	first, e := MemoryCLI(c, []string{"search", "--view", "brief", "원문", "--task", "a"})
	if e != nil {
		t.Fatal(e)
	}
	f := first.(memory.Object)
	if memory.Number(f["limit"]) != 5 || len(f["matches"].([]memory.Object)) != 5 || f["has_more"] != true {
		t.Fatal(f)
	}
	second, e := MemoryCLI(c, []string{"search", "원문", "--view", "brief", "--task", "a", "--limit", "2", "--offset", "5"})
	if e != nil {
		t.Fatal(e)
	}
	q := second.(memory.Object)
	if len(q["matches"].([]memory.Object)) != 2 || q["has_more"] != false {
		t.Fatal(q)
	}
	seen := map[string]bool{}
	for _, result := range []memory.Object{f, q} {
		for _, match := range result["matches"].([]memory.Object) {
			key := memory.Text(match["evidence"])
			if key == "" || seen[key] {
				t.Fatal("missing or duplicate evidence", match)
			}
			seen[key] = true
		}
	}
}

func TestSourceExcerptCLIUnicode(t *testing.T) {
	c, _, id := retrievalFixture(t)
	got, e := MemoryCLI(c, []string{"source", id, "--offset", "2", "--max-chars", "2"})
	if e != nil {
		t.Fatal(e)
	}
	v := got.(memory.Object)
	if v["text"] != "다🙂" || memory.Number(v["next_offset"]) != 4 || memory.Number(v["total_chars"]) != 6 || v["truncated"] != true {
		t.Fatal(v)
	}
	got, e = MemoryCLI(c, []string{"source", id, "--offset", "4"})
	if e != nil || got.(memory.Object)["text"] != "마바" {
		t.Fatal(got, e)
	}
	got, e = MemoryCLI(c, []string{"source", id, "--max-chars", "1"})
	if e != nil || got.(memory.Object)["text"] != "가" {
		t.Fatal(got, e)
	}
	output := filepath.Join(t.TempDir(), "excerpt.txt")
	if _, e = MemoryCLI(c, []string{"source", id, "--offset", "0", "--output", output}); e == nil {
		t.Fatal("excerpt accepted output")
	}
	if _, e = os.Stat(output); !os.IsNotExist(e) {
		t.Fatal("rejected excerpt created output")
	}
}

func TestRetrievalOptionsRejected(t *testing.T) {
	c := configFixture(t)
	for _, args := range [][]string{
		{"brief"}, {"brief", "--task", "outside"},
		{"brief", "--task", "a", "unexpected"}, {"brief", "--task", "a", "--limit", "2"},
		{"brief", "--task", "a", "--budget", "0"}, {"brief", "--task", "a", "--budget", "1000001"},
		{"brief", "--task", "a", "--budget", "no"}, {"brief", "--task", "a", "--budget"},
		{"brief", "--task", "--budget", "50"}, {"brief", "--task", "a", "--unknown", "x"},
		{"recall", "--task", "a", "--budget", "50"}, {"history", "--task", "a", "--offset", "1"},
		{"search", "--task", "a", "--view", "full", "query"},
		{"search", "--task", "a", "--view", "brief"},
		{"search", "--task", "a", "--view", "brief", ""},
		{"search", "--task", "a", "--view", "brief", "query", "extra"},
		{"search", "--task", "a", "--view", "brief", "query", "--after", "0"},
		{"search", "--task", "a", "--view", "brief", "query", "--offset", "-1"},
		{"search", "--task", "a", "--view", "brief", "query", "--limit", "101"},
		{"search", "--task", "a", "--view", "brief", "query", "--limit", "0"},
		{"search", "--task", "a", "--offset", "1", "query"},
		{"source", "--max-chars", "2"}, {"source", "id", "--max-chars", "0"},
		{"source", "id", "--max-chars", "100001"}, {"source", "id", "--offset", "-1"},
		{"source", "id", "--max-chars", "2", "--task", "a"},
		{"health", "--view", "brief"}, {"replay", "--max-chars", "2"},
	} {
		if _, e := MemoryCLI(c, args); e == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}

func TestTasksSummaryAndCompletedSelection(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	state := memory.Object{"key": "state-a", "task": "a", "session": "s", "kind": "state", "subject": "task", "body": strings.Repeat("한🙂", 100), "status": "active", "expected_revision": 0}
	if _, e := s.Ingest(state); e != nil {
		t.Fatal(e)
	}
	choices, e := Tasks(c)
	if e != nil || len(choices) != 2 || len([]rune(memory.Text(choices[0]["summary"]))) != 160 || choices[0]["summary_truncated"] != true || choices[1]["summary_truncated"] != false {
		t.Fatal(choices, e)
	}
	r, e := Prepare(c, "session", c.ProjectRoot)
	if e != nil {
		t.Fatal(e)
	}
	state["key"], state["status"], state["expected_revision"] = "state-a-complete", "complete", 1
	if _, e = s.Ingest(state); e != nil {
		t.Fatal(e)
	}
	a := memory.Object{"request": r["request"], "question_id": "q", "session": "session", "selected": "a"}
	if _, e = Select(c, memory.Text(r["request"]), "q", a); e == nil {
		t.Fatal("completed task accepted from older selection")
	}
	choices, e = Tasks(c)
	if e != nil || len(choices) != 1 || choices[0]["task"] != "b" {
		t.Fatal(choices, e)
	}
	a["selected"] = "b"
	selected, e := Select(c, memory.Text(r["request"]), "q", a)
	if e != nil || !strings.Contains(memory.Text(selected["instruction"]), "memory brief --task b") {
		t.Fatal(selected, e)
	}
	bound, e := Capture(c, "codex", memory.Object{"hook_event_name": "SessionStart", "session_id": "session", "cwd": c.ProjectRoot})
	if e != nil || !strings.Contains(memory.Text(bound["systemMessage"]), "memory brief --task b") {
		t.Fatal(bound, e)
	}
}

func TestMemoryReadsOmitResponseOnly(t *testing.T) {
	c := configFixture(t)
	s, _ := c.Store(false)
	if e := s.Bind("codex", "session", "a", c.ProjectRoot); e != nil {
		t.Fatal(e)
	}
	for i, tc := range []struct {
		cmd  string
		omit bool
	}{
		{"harness memory brief --task a", true},
		{"/home/me/.local/share/harness/bin/harness-0.1.0 memory brief --task a", true},
		{`"/path with spaces/harness-0.1.0" --config "/a config.json" --project p memory search --view brief --task a query`, true},
		{"harness --project=p memory source event:raw --offset 0", true},
		{"python3 /home/me/memory.py --config /tmp/config.json recall --task a", true},
		{`'/usr/local/bin/python3' '/경로 공간/memory.py' --config='/a config.json' recall --task a`, true},
		{`"harness" memory search '인용 안의 ; 문장' --task a`, true},
		{"harness memory record brief.json", false},
		{"harness policy check --task a", false},
		{"cat /tmp/harness-brief-report.txt", false},
		{`echo 'harness memory brief --task a'`, false},
		{"harness memory brief --task a; cat unrelated.log", false},
		{"harness memory brief --task a && cat unrelated.log", false},
		{"harness memory brief --task a\ncat unrelated.log", false},
		{"harness memory brief --task a | cat unrelated.log", false},
		{`printf '%s' 'harness memory brief --task a'`, false},
		{`harness memory record 'harness memory brief --task a'`, false},
		{`harness memory search "$(cat unrelated.log)" --task a`, false},
		{"harness memory search `cat unrelated.log` --task a", false},
		{"harness memory brief --task a > unrelated.log", false},
	} {
		marker := fmt.Sprintf("response-marker-%d", i)
		payload := memory.Object{"hook_event_name": "PostToolUse", "session_id": "session", "cwd": c.ProjectRoot, "tool_use_id": fmt.Sprintf("tool-%d", i), "tool_input": memory.Object{"cmd": tc.cmd}, "tool_response": marker}
		if _, e := Capture(c, "codex", payload); e != nil {
			t.Fatal(e)
		}
		records, e := s.Records("a", marker, 100, 0)
		if e != nil || (tc.omit && len(records) != 0) || (!tc.omit && len(records) != 1) {
			t.Fatalf("%s: %v %v", tc.cmd, records, e)
		}
	}
}
