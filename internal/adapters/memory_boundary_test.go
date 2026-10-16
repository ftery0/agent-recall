package adapters

import (
	"bytes"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureIgnoresEmptyAndMalformedOccurrenceIDs(t *testing.T) {
	cases := []struct {
		name, event string
		fields      func(int) memory.Object
	}{
		{"empty primary", "PostToolUse", func(i int) memory.Object {
			return memory.Object{"event_id": "", "tool_use_id": []string{"one", "two"}[i]}
		}},
		{"whitespace primary", "PostToolUse", func(i int) memory.Object {
			return memory.Object{"event_id": " \t", "tool_use_id": []string{"one", "two"}[i]}
		}},
		{"malformed primary", "PostToolUse", func(i int) memory.Object {
			return memory.Object{"event_id": false, "tool_use_id": []string{"one", "two"}[i]}
		}},
		{"empty call", "PostToolUse", func(int) memory.Object {
			return memory.Object{"tool_use_id": "", "turn_id": "shared-turn"}
		}},
		{"empty prompt turn", "UserPromptSubmit", func(int) memory.Object {
			return memory.Object{"turn_id": ""}
		}},
		{"empty stop turn", "Stop", func(int) memory.Object {
			return memory.Object{"turn_id": ""}
		}},
		{"turn fallback", "UserPromptSubmit", func(i int) memory.Object {
			return memory.Object{"event_id": "", "tool_use_id": nil, "turn_id": []string{"one", "two"}[i]}
		}},
		{"missing IDs equal text", "UserPromptSubmit", func(int) memory.Object { return memory.Object{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := configFixture(t)
			s, _ := c.Store(false)
			if err := s.Bind("codex", "synthetic", "a", c.ProjectRoot); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				payload := tc.fields(i)
				payload["session_id"], payload["cwd"], payload["hook_event_name"] = "synthetic", c.ProjectRoot, tc.event
				payload["prompt"], payload["tool_response"], payload["last_assistant_message"] = "same text", "same result", "same report"
				if _, err := Capture(c, "codex", payload); err != nil {
					t.Fatal(err)
				}
			}
			records, err := s.Records("a", "", 100, 0)
			if err != nil || len(records) != 2 {
				t.Fatal("independent events merged", records, err)
			}
			health, err := s.Health()
			if err != nil || health["invalid"] != 0 || health["pending"] != 0 {
				t.Fatal(health, err)
			}
		})
	}
}

func TestMalformedSelectionChoicesDoNotConsumeOrBind(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"null", nil}, {"object", memory.Object{}}, {"string", "invalid"},
		{"nonobject entry", []any{"invalid"}}, {"missing task", []any{memory.Object{}}},
		{"numeric task", []any{memory.Object{"task": 1}}},
	}
	for _, tc := range cases {
		for _, selected := range []string{"a", "none"} {
			t.Run(tc.name+"/"+selected, func(t *testing.T) {
				c := configFixture(t)
				s, _ := c.Store(false)
				request, err := PrepareTool(c, "cursor", "synthetic", c.ProjectRoot)
				if err != nil {
					t.Fatal(err)
				}
				request["choices"] = tc.value
				raw := memory.Packed(request)
				id := memory.Text(request["request"])
				path := filepath.Join(s.Root, "selections", id+".json")
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				answer := memory.Object{"request": id, "question_id": "synthetic-question", "session": "synthetic", "selected": selected}
				if _, err = Select(c, id, "synthetic-question", answer); err == nil || !strings.Contains(err.Error(), "invalid selection choices") {
					t.Fatal("corrupt request accepted", err)
				}
				binding, err := s.Binding("cursor", "synthetic")
				if err != nil || binding != nil {
					t.Fatal("failed request bound", binding, err)
				}
				optedOut, err := s.OptedOut("cursor", "synthetic")
				if err != nil || optedOut {
					t.Fatal("failed request changed opt-out", optedOut, err)
				}
				preserved, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(raw, preserved) {
					t.Fatal("corrupt original changed", err)
				}
				db, err := s.Connect(true)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var sources int
				if err = db.QueryRow("SELECT COUNT(*) FROM sources").Scan(&sources); err != nil || sources != 0 {
					t.Fatal("failed selection wrote evidence", sources, err)
				}
			})
		}
	}
}
