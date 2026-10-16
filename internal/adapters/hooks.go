package adapters

import (
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"regexp"
	"strings"
)

var secret = regexp.MustCompile(`(?i)(token|password|secret|authorization|api[_-]?key|cookie)`)
var bearer = regexp.MustCompile(`(?i)(Bearer\s+)[A-Za-z0-9._-]+`)
var token = regexp.MustCompile(`\b(?:sk|ghp|github_pat)[_-][A-Za-z0-9_-]{12,}`)
var assignment = regexp.MustCompile(`(?i)((?:[\w-]*(?:token|password|secret|api[_-]?key))\s*[=:]\s*)['"]?[^\s,'";]+`)

// Only omit output from one complete retrieval command; preserve ambiguous shell input.
const shellLiteral = `(?:"[^"$\\\x60\r\n]*"|'[^'\r\n]*'|[^\s;&|<>()"'\\$#\x60]+)`

var memoryRead = regexp.MustCompile(`^[ \t]*(?:` + commandPath(`harness(?:-[0-9][A-Za-z0-9._-]*)?`) +
	`(?:[ \t]+--(?:config|project)(?:=|[ \t]+)` + shellLiteral + `)*[ \t]+memory[ \t]+|(?:` + commandPath(`python(?:3(?:\.[0-9]+)?)?`) + `[ \t]+)?` + commandPath(`memory\.py`) +
	`(?:[ \t]+--config(?:=|[ \t]+)` + shellLiteral + `)*[ \t]+)(?:brief|recall|history|search|source|export|tasks|health)(?:[ \t]+` + shellLiteral + `)*[ \t]*$`)

func commandPath(name string) string {
	return `(?:(?:[A-Za-z0-9_./~+-]+/)?` + name + `|"(?:[^"$\\\x60\r\n]*/)?` + name + `"|'(?:[^'\r\n]*/)?` + name + `')`
}

func Redact(value any) any {
	switch v := value.(type) {
	case map[string]any:
		r := memory.Object{}
		for k, x := range v {
			if secret.MatchString(k) {
				r[k] = "[redacted]"
			} else {
				r[k] = Redact(x)
			}
		}
		return r
	case []any:
		r := make([]any, len(v))
		for i, x := range v {
			r[i] = Redact(x)
		}
		return r
	case string:
		return assignment.ReplaceAllString(token.ReplaceAllString(bearer.ReplaceAllString(v, "${1}[redacted]"), "[redacted]"), "${1}[redacted]")
	default:
		return value
	}
}
func Capture(c Config, tool string, payload memory.Object) (memory.Object, error) {
	if !memory.ValidTool(tool) {
		return nil, errors.New("unsupported adapter")
	}
	var normalizeErr error
	payload, normalizeErr = NormalizeMemoryPayload(tool, payload)
	if normalizeErr != nil {
		return nil, normalizeErr
	}
	event := memory.Text(payload["hook_event_name"])
	session := memory.Text(payload["session_id"])
	cwd := memory.Text(payload["cwd"])
	if !c.Permitted(cwd) || session == "" {
		return nil, nil
	}
	if event != "SessionStart" && event != "UserPromptSubmit" && event != "PostToolUse" && event != "Stop" {
		return nil, nil
	}
	command := ""
	if event == "SessionStart" {
		var e error
		command, e = c.memoryCommand()
		if e != nil {
			return nil, e
		}
	}
	s, e := c.Store(false)
	if e != nil {
		return nil, e
	}
	binding, e := s.Binding(tool, session)
	if e != nil {
		return nil, e
	}
	if binding == nil {
		if event == "SessionStart" {
			optedOut, e := s.OptedOut(tool, session)
			if e != nil {
				return nil, e
			}
			if optedOut {
				return nil, nil
			}
			request, e := prepareTool(c, tool, session, cwd, true)
			if e != nil {
				return nil, e
			}
			if request == nil {
				return nil, nil
			}
			return memory.Object{"hookSpecificOutput": memory.Object{"hookEventName": "SessionStart", "additionalContext": "진행 중 작업과 연결 안 함을 현재 제공되는 선택형 질문 UI로 보여주세요. 질문 UI가 없으면 짧은 선택 질문으로 실제 응답을 받으세요. 실제 응답 전에는 연결하지 않습니다. " + command + " selection --request <request> --question-id <question_id> --answer-file <응답 JSON 파일>로 연결하세요. 응답 JSON에는 request/question_id/session/selected를 담습니다. 선택은 과거 실행 승인을 복구하지 않습니다. 요청: " + string(memory.Packed(request))}}, nil
		}
		return nil, nil
	}
	if c.Task(memory.Text(binding["task"])) != nil || !memory.Within(memory.Text(binding["cwd"]), cwd) {
		return nil, nil
	}
	if event == "SessionStart" {
		instruction := "선택된 작업 기억 연결됨. " + command + " brief --task " + memory.Text(binding["task"]) + "로 현재 상태와 근거를 조회하세요. 과거 승인은 새 실행 허가가 아닙니다."
		return memory.Object{"systemMessage": instruction, "hookSpecificOutput": memory.Object{"hookEventName": "SessionStart", "additionalContext": instruction}}, nil
	}
	input, _ := payload["tool_input"].(map[string]any)
	command = memory.Text(input["command"])
	if command == "" {
		command = memory.Text(input["cmd"])
	}
	// A proven retrieval response may be huge. Omit it before traversing for
	// secrets; preserve the caller's payload and all ambiguous command output.
	toRedact := payload
	if event == "PostToolUse" && memoryRead.MatchString(command) {
		toRedact = memory.Object{}
		for key, value := range payload {
			toRedact[key] = value
		}
		toRedact["tool_response"] = memory.Object{"omitted": "memory retrieval body; query metadata only"}
	}
	clean := Redact(toRedact).(map[string]any)
	delete(clean, "transcript_path")
	delete(clean, "permission_mode")
	body := memory.Packed(clean)
	occurrence := memory.Text(payload["event_id"])
	if strings.TrimSpace(occurrence) == "" {
		occurrence = memory.Text(payload["tool_use_id"])
	}
	if strings.TrimSpace(occurrence) == "" && event != "PostToolUse" {
		occurrence = memory.Text(payload["turn_id"])
	}
	identity := []byte(memory.ID())
	if strings.TrimSpace(occurrence) != "" {
		identity = memory.Packed([]any{session, event, occurrence})
	}
	authority := "native-event"
	if event == "Stop" {
		authority = "agent-report"
	}
	result, e := s.Ingest(memory.Object{"key": tool + "-" + memory.Hash(identity), "task": binding["task"], "session": session, "kind": "raw", "subject": "native-events", "body": string(body), "source_keys": []any{}, "tool": tool, "event": event, "authority": authority, "canonical_hash": memory.Hash(body)})
	if e != nil {
		return nil, e
	}
	if result["result"] == "queued" {
		return memory.Object{"systemMessage": "작업 기억 DB 대기: 사건을 로컬 대기열에 보존했습니다. 다음 기억 수집 때 소량씩 자동 재처리하며, harness memory replay로도 재처리할 수 있습니다."}, nil
	}
	if result["recovery_issues"] != nil {
		return memory.Object{"systemMessage": "이번 작업 기억은 저장했습니다. 이전 대기열의 처리 불가 항목은 원본을 보존해 분리했거나 그대로 두었습니다. memory replay 결과와 health의 invalid 항목을 확인하세요."}, nil
	}
	return nil, nil
}
func IsOutside(e error) bool {
	return e != nil && strings.Contains(e.Error(), "outside configured scope")
}
