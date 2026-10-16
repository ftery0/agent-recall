package adapters

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/policy"
	"os"
	"regexp"
)

var (
	gitWrite      = regexp.MustCompile(`\bgit\b[^\n]*\bcommit\b|\bgh\s+(pr|issue)\s+(create|edit|comment)\b`)
	aiAttribution = regexp.MustCompile(`(?i)co-authored-by:[^\n]*(openai\.com|anthropic\.com|codex|claude|copilot|gemini|cursor)|generated with \[?(codex|claude code)`)
)

// AttributionHook stops a commit or PR command that carries an AI co-author line.
// Codex is told by its account setting to add one, and no local setting turns that off.
func AttributionHook(payload memory.Object) memory.Object {
	input, _ := payload["tool_input"].(map[string]any)
	command := memory.Text(input["command"]) + memory.Text(input["cmd"])
	if memory.Text(payload["hook_event_name"]) != "PreToolUse" || !gitWrite.MatchString(command) || !aiAttribution.MatchString(command) {
		return nil
	}
	return memory.Object{"hookSpecificOutput": memory.Object{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       "deny",
		"permissionDecisionReason": "커밋·PR 에 AI 공동 작성자(Co-authored-by)나 생성 표기를 넣지 않는다. 그 줄만 빼고 같은 명령을 다시 실행한다.",
	}}
}

// StructureHook keeps structural evidence independent of memory collection and
// native tool permissions. Existing approval, Orca and Claude lifecycle hooks stay separate.
func StructureHook(tool string, payload memory.Object) (memory.Object, error) {
	out, err := policy.EditScope(payload, tool, os.TempDir())
	if err != nil {
		return memory.Object{"systemMessage": "미확인: 편집 범위의 실행 전후 근거를 기록하거나 읽지 못했습니다. " + err.Error()}, nil
	}
	return out, nil
}
