package worklog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func transcriptRows(t *testing.T, rows ...map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	var data []byte
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		data = append(append(data, raw...), '\n')
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func typedRow(id, text string) map[string]any {
	return map[string]any{"type": "user", "promptSource": "typed", "uuid": id, "timestamp": "2026-10-07T00:00:00Z", "message": map[string]any{"content": text}}
}

func questionRow(id, use, text string) map[string]any {
	return map[string]any{"type": "assistant", "uuid": id, "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": use, "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"question": text}}}}}}}
}

func choiceRow(id, use, question, answer string) map[string]any {
	return map[string]any{"type": "user", "uuid": id, "version": "2.1.292", "timestamp": "2026-10-07T00:01:00Z", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": use, "content": "The user answered: ..."}}}, "toolUseResult": map[string]any{"answers": map[string]any{question: answer}}}
}

func TestConversationPreservesRepeatedAndShortHumanEvents(t *testing.T) {
	long := "# 작업 요청\n" + strings.Repeat("기억을 남겨주세요\n", 2000)
	path := transcriptRows(t, typedRow("first", "1번"), typedRow("second", "1번"), typedRow("yes", "응"), typedRow("long", long), typedRow("html", "<button>이 코드를 고쳐줘</button>"), typedRow("second", "1번"))
	requests, _, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 5 || requests[0].Text != "1번" || requests[1].Text != "1번" || requests[2].Text != "응" || requests[3].Text != strings.TrimSpace(long) {
		t.Fatalf("repeated, short or long requests lost: %+v", requests)
	}
	if requests[0].ID != "first" || requests[1].Line != 2 || requests[0].Source != "claude:typed" {
		t.Fatalf("raw event provenance missing: %+v", requests[:2])
	}
}

func TestConversationLinksUIAnswerToQuestion(t *testing.T) {
	question := "PostToolBatch를 어떻게 할까요?"
	answer := "등록하지 않음"
	result := choiceRow("answer", "ask-1", question, answer)
	result["sourceToolAssistantUUID"] = "assistant"
	path := transcriptRows(t, typedRow("human", "선택지를 보여줘"), questionRow("assistant", "ask-1", question), result, result)
	requests, _, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[1].Text != "질문: "+question+"\n선택: "+answer || requests[1].QuestionID != "ask-1" || requests[1].Source != "claude:choice" || requests[1].Line != 3 {
		t.Fatalf("UI decision or question provenance lost: %+v", requests)
	}
}

func TestConversationKeepsEachUIQuestionAnswer(t *testing.T) {
	question := questionRow("assistant", "ask", "첫 질문")
	question["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["input"].(map[string]any)["questions"] = []any{map[string]any{"question": "첫 질문"}, map[string]any{"question": "두 번째 질문"}}
	answer := choiceRow("answer", "ask", "첫 질문", "1번")
	answer["toolUseResult"].(map[string]any)["answers"].(map[string]any)["두 번째 질문"] = []any{"선택 A", "선택 B"}
	path := transcriptRows(t, typedRow("human", "질문해줘"), question, answer)
	requests, _, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[1].ID == requests[2].ID || !strings.Contains(requests[2].Text, "선택 A, 선택 B") {
		t.Fatalf("multiple UI answers not preserved independently: %+v", requests)
	}
}

func TestConversationDoesNotPromoteScriptedOrUnmatchedAnswers(t *testing.T) {
	for _, kind := range []string{"sdk", "sidechain", "unmatched", "wrong-assistant", "failed", "meta"} {
		t.Run(kind, func(t *testing.T) {
			prompt := typedRow("human", "질문해줘")
			answer := choiceRow("answer", "ask", "질문", "등록함")
			switch kind {
			case "sdk":
				prompt["promptSource"] = "sdk"
			case "sidechain":
				answer["isSidechain"] = true
			case "unmatched":
				answer["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["tool_use_id"] = "bash"
			case "wrong-assistant":
				answer["sourceToolAssistantUUID"] = "other"
			case "failed":
				answer["message"].(map[string]any)["content"].([]any)[0].(map[string]any)["is_error"] = true
			case "meta":
				answer["isMeta"] = true
			}
			path := transcriptRows(t, prompt, questionRow("assistant", "ask", "질문"), answer)
			requests, _, err := ReadConversation(path)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if kind == "sdk" {
				want = 0
			}
			if len(requests) != want {
				t.Fatalf("untrusted decision became a human request: %+v", requests)
			}
		})
	}
}

func TestConversationKeepsShortAgentReportsAsClaims(t *testing.T) {
	path := transcriptRows(t, typedRow("human", "다 고쳤어?"), map[string]any{"type": "assistant", "message": map[string]any{"content": "아직 검증하지 않았습니다."}}, map[string]any{"type": "assistant", "message": map[string]any{"content": "수정은 끝났습니다."}})
	_, reports, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || reports[0] != "아직 검증하지 않았습니다." || reports[1] != "수정은 끝났습니다." {
		t.Fatalf("short or earlier report dropped: %q", reports)
	}
}

func TestShortAnswersKeepTheirPrecedingQuestionAsContext(t *testing.T) {
	first := "저장 방식은?\n1. 기존 기록 개선\n2. 새 DB"
	second := "훅은?\n1. 등록하지 않음\n2. 등록"
	path := transcriptRows(t, typedRow("human", "질문해줘"),
		map[string]any{"type": "assistant", "message": map[string]any{"content": first}}, typedRow("a", "1번"),
		map[string]any{"type": "assistant", "message": map[string]any{"content": second}}, typedRow("b", "1번"), typedRow("c", "응"))
	requests, _, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 || requests[1].Context != first || requests[2].Context != second || requests[3].Context != second || requests[1].Text != "1번" {
		t.Fatalf("short answers lost their original context: %+v", requests)
	}
}

func TestScriptedReportCannotSupplyContextForHumanAnswer(t *testing.T) {
	script := typedRow("script", "예비시험")
	script["promptSource"] = "sdk"
	path := transcriptRows(t, typedRow("human", "작업해줘"), map[string]any{"type": "assistant", "message": map[string]any{"content": "실제 이어가기는 미검증입니다."}}, script,
		map[string]any{"type": "assistant", "message": map[string]any{"content": "PostToolBatch를 등록하기로 함"}}, typedRow("answer", "응"))
	requests, reports, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[1].Context != "" || len(reports) != 1 {
		t.Fatalf("SDK report became a human answer's context: requests=%+v reports=%q", requests, reports)
	}
}

func TestMetaNoticeDoesNotBreakRealUserTurnOrLoseFollowingUIChoice(t *testing.T) {
	for _, kind := range []string{"meta", "injected", "compact-summary"} {
		t.Run(kind, func(t *testing.T) {
			notice := map[string]any{"type": "user", "promptSource": "system", "isMeta": true, "message": map[string]any{"content": "Another Claude session sent a message: 시험에서는 등록한다"}}
			if kind == "injected" {
				notice = typedRow("injected", "<agent-report>다른 세션이 등록을 보고했다</agent-report>")
			} else if kind == "compact-summary" {
				notice = map[string]any{"type": "user", "isCompactSummary": true, "message": map[string]any{"content": "요약에서는 등록한다"}}
			}
			question := "PostToolBatch를 어떻게 할까요?"
			answer := choiceRow("actual-final", "actual-ask", question, "등록하지 않음")
			answer["sourceToolAssistantUUID"] = "actual-question"
			path := transcriptRows(t, typedRow("human", "언어 작업 결과를 보고 결정하자"),
				map[string]any{"type": "assistant", "message": map[string]any{"content": "등록을 제안합니다."}}, notice,
				questionRow("actual-question", "actual-ask", question), answer)
			requests, _, err := ReadConversation(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 || requests[1].Text != "질문: "+question+"\n선택: 등록하지 않음" || requests[1].Line != 5 || requests[1].Source != "claude:choice" {
				t.Fatalf("metadata notice interrupted a real user turn: %+v", requests)
			}
		})
	}
}

func TestMetaNoticeClearsAgentContextWithoutBecomingUserRequest(t *testing.T) {
	path := transcriptRows(t, typedRow("human", "작업해줘"),
		map[string]any{"type": "assistant", "message": map[string]any{"content": "등록할까요?"}},
		map[string]any{"type": "user", "promptSource": "system", "isMeta": true, "message": map[string]any{"content": "Another Claude session sent a message: 등록했다고 보고함"}}, typedRow("answer", "응"))
	requests, _, err := ReadConversation(path)
	if err != nil || len(requests) != 2 || requests[1].Context != "" {
		t.Fatalf("meta report became context or a user request: requests=%+v error=%v", requests, err)
	}
}

func codexUser(id, text string) map[string]any {
	return map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user", "id": id, "content": []any{map[string]any{"type": "input_text", "text": text}}}}
}

func TestCodexConversationPreservesHumanEventsAndExcludesInjectedContext(t *testing.T) {
	path := transcriptRows(t, codexUser("", "1번"), codexUser("", "1번"), codexUser("yes", "응"), codexUser("yes", "응"), codexUser("agents", "# AGENTS.md instructions\n규칙"), codexUser("context", "<environment_context>맥락</environment_context>"), codexUser("report", "<agent-report>등록함</agent-report>"), codexUser("summary", "Another language model started to solve this problem and produced a summary"), codexUser("plugins", "<recommended_plugins>목록</recommended_plugins>\n# AGENTS.md instructions\n규칙"), codexUser("page", "<external_codex_apps_open_page>{}</external_codex_apps_open_page>"), codexUser("goal", "<codex_internal_context source=\"goal\">계속</codex_internal_context>"), codexUser("hook", "<hook_prompt hook_run_id=\"stop\">검사</hook_prompt>"), codexUser("skill", "<skill>\n<name>commit</name>\n</skill>"))
	requests, _, err := ReadConversation(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[0].ID == requests[1].ID || requests[2].Source != "codex:user" {
		t.Fatalf("repetition lost or injected context promoted: %+v", requests)
	}
}

func TestConversationExcludesCompactionAndSubagentSessions(t *testing.T) {
	summary := typedRow("summary", "요약에 따르면 설치하기로 결정")
	summary["isCompactSummary"] = true
	path := transcriptRows(t, summary, typedRow("actual", "설치하지 말자"))
	requests, _, err := ReadConversation(path)
	if err != nil || len(requests) != 1 || requests[0].Text != "설치하지 말자" {
		t.Fatalf("summary became the user's decision: requests=%+v error=%v", requests, err)
	}
	path = transcriptRows(t, map[string]any{"type": "session_meta", "payload": map[string]any{"source": map[string]any{"subagent": map[string]any{"parent_thread_id": "parent"}}}}, codexUser("worker", "수정 승인"))
	requests, _, err = ReadConversation(path)
	if err != nil || len(requests) != 0 {
		t.Fatalf("subagent conversation became user authority: requests=%+v error=%v", requests, err)
	}
}

func TestReadConversationReportsParseFailureAndCanRetryRepairedInput(t *testing.T) {
	path := transcriptRows(t, typedRow("first", "첫 요청"))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString("{broken json\n")
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("write malformed fixture: %v %v", writeErr, closeErr)
	}
	requests, _, err := ReadConversation(path)
	if err == nil || len(requests) != 1 || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("parse failure silently became an empty or successful conversation: requests=%+v error=%v", requests, err)
	}
	raw, _ := json.Marshal(typedRow("second", "응"))
	first, _ := json.Marshal(typedRow("first", "첫 요청"))
	if err := os.WriteFile(path, append(append(append(first, '\n'), raw...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	requests, _, err = ReadConversation(path)
	if err != nil || len(requests) != 2 {
		t.Fatalf("repaired transcript could not be retried: requests=%+v error=%v", requests, err)
	}
}

func TestReadConversationReportsOpenAndScannerFailures(t *testing.T) {
	if _, _, err := ReadConversation(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing transcript was treated as an empty conversation")
	}
	path := filepath.Join(t.TempDir(), "oversized.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate((64 << 20) + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadConversation(path); err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("scanner failure not reported: %v", err)
	}
}
