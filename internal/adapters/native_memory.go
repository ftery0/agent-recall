package adapters

import (
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
)

func NormalizeMemoryPayload(tool string, payload memory.Object) (memory.Object, error) {
	if !memory.ValidTool(tool) {
		return nil, errors.New("unsupported adapter")
	}
	if tool != "cursor" {
		return payload, nil
	}
	result := memory.Object{}
	for key, value := range payload {
		result[key] = value
	}
	events := map[string]string{
		"sessionStart": "SessionStart", "beforeSubmitPrompt": "UserPromptSubmit",
		"postToolUse": "PostToolUse", "stop": "Stop",
	}
	if event := events[memory.Text(payload["hook_event_name"])]; event != "" {
		result["hook_event_name"] = event
	}
	if memory.Text(result["session_id"]) == "" {
		result["session_id"] = payload["conversation_id"]
	}
	if memory.Text(result["turn_id"]) == "" {
		result["turn_id"] = payload["generation_id"]
	}
	if memory.Text(result["cwd"]) == "" {
		switch roots := payload["workspace_roots"].(type) {
		case []any:
			if len(roots) == 1 {
				result["cwd"] = roots[0]
			}
		case []string:
			if len(roots) == 1 {
				result["cwd"] = roots[0]
			}
		}
	}
	if result["tool_name"] == "Shell" {
		result["tool_name"] = "Bash"
	}
	if output, exists := result["tool_output"]; exists {
		result["tool_response"] = output
		// Keep one output field so retrieval filtering and redaction cover it.
		delete(result, "tool_output")
	}
	return result, nil
}

// Client hook protocols differ; a Stop notice must never trigger another turn.
func MemoryHookOutput(tool string, payload, result memory.Object) any {
	context := ""
	if specific, ok := result["hookSpecificOutput"].(map[string]any); ok {
		context = memory.Text(specific["additionalContext"])
	}
	if context == "" {
		context = memory.Text(result["systemMessage"])
	}
	event := memory.Text(payload["hook_event_name"])
	switch tool {
	case "cursor":
		if event == "UserPromptSubmit" {
			return memory.Object{"continue": true}
		}
		if context != "" && (event == "SessionStart" || event == "PostToolUse") {
			return memory.Object{"additional_context": context}
		}
		return nil
	case "kiro":
		if event == "SessionStart" || event == "UserPromptSubmit" {
			if context != "" {
				return context
			}
		}
		return nil
	default:
		if result == nil {
			return nil
		}
		return result
	}
}
