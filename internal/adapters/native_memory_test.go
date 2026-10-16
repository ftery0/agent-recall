package adapters

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"reflect"
	"testing"
)

func TestCursorNativeMemory(t *testing.T) {
	native := memory.Object{"hook_event_name": "postToolUse", "conversation_id": "chat", "generation_id": "turn", "workspace_roots": []any{"/project"}, "tool_name": "Shell", "tool_output": "output", "tool_input": memory.Object{"command": "pwd"}, "tool_use_id": "call"}
	want := memory.Object{"hook_event_name": "PostToolUse", "conversation_id": "chat", "generation_id": "turn", "workspace_roots": []any{"/project"}, "session_id": "chat", "turn_id": "turn", "cwd": "/project", "tool_name": "Bash", "tool_response": "output", "tool_input": memory.Object{"command": "pwd"}, "tool_use_id": "call"}
	got, err := NormalizeMemoryPayload("cursor", native)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize: %#v, %v", got, err)
	}
	again, err := NormalizeMemoryPayload("cursor", got)
	if err != nil || !reflect.DeepEqual(again, got) || native["hook_event_name"] != "postToolUse" {
		t.Fatal("normalization must be idempotent and preserve input")
	}
}

func TestCursorRootScope(t *testing.T) {
	for _, roots := range []any{[]any{}, []any{"/first", "/second"}, "invalid"} {
		got, err := NormalizeMemoryPayload("cursor", memory.Object{"workspace_roots": roots})
		if err != nil || memory.Text(got["cwd"]) != "" {
			t.Fatal("ambiguous roots must not select a project")
		}
	}
	got, _ := NormalizeMemoryPayload("cursor", memory.Object{"cwd": "/second", "workspace_roots": []any{"/first", "/second"}})
	if got["cwd"] != "/second" {
		t.Fatal("actual cwd must take precedence")
	}
}

func TestNativeOutputDoesNotRequestContinuation(t *testing.T) {
	notice := memory.Object{"systemMessage": "notice", "hookSpecificOutput": memory.Object{"additionalContext": "context"}}
	for _, tool := range []string{"cursor", "kiro"} {
		if out := MemoryHookOutput(tool, memory.Object{"hook_event_name": "Stop"}, notice); out != nil {
			t.Fatalf("%s Stop must have no continuation output: %#v", tool, out)
		}
	}
	if out := MemoryHookOutput("cursor", memory.Object{"hook_event_name": "UserPromptSubmit"}, notice); !reflect.DeepEqual(out, memory.Object{"continue": true}) {
		t.Fatalf("Cursor prompt must continue: %#v", out)
	}
	if out := MemoryHookOutput("cursor", memory.Object{"hook_event_name": "SessionStart"}, notice); !reflect.DeepEqual(out, memory.Object{"additional_context": "context"}) {
		t.Fatalf("Cursor SessionStart: %#v", out)
	}
	if out := MemoryHookOutput("kiro", memory.Object{"hook_event_name": "SessionStart"}, notice); out != "context" {
		t.Fatalf("Kiro must use plain text: %#v", out)
	}
	if out := MemoryHookOutput("kiro", memory.Object{"hook_event_name": "PostToolUse"}, notice); out != nil {
		t.Fatal("Kiro PostToolUse must not return control output")
	}
	if out := MemoryHookOutput("codex", memory.Object{}, notice); !reflect.DeepEqual(out, notice) {
		t.Fatal("Codex output changed")
	}
}
