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

func vscodeRow(id, origin string, content any) map[string]any {
	return map[string]any{"type": "user", "uuid": id, "promptSource": "sdk", "entrypoint": "claude-vscode", "version": "2.1.233", "origin": map[string]any{"kind": origin}, "message": map[string]any{"content": content}}
}

func TestConversationReadsVSCodePromptsAndSkipsItsNotices(t *testing.T) {
	prompt := vscodeRow("prompt", "human", []any{
		map[string]any{"type": "text", "text": "<ide_opened_file>The user opened the file /repo/src/app.ts in the IDE.</ide_opened_file>"},
		map[string]any{"type": "text", "text": "버튼 색을 바꿔줘"},
	})
	notice := vscodeRow("notice", "task-notification", "작업이 끝났습니다")
	tagged := vscodeRow("tagged", "", "<task-notification>\n<task-id>t1</task-id>\n</task-notification>")
	delete(tagged, "origin")
	answer := choiceRow("answer", "ask", "어떤 색으로 할까요?", "파랑")
	answer["entrypoint"] = "claude-vscode"
	script := typedRow("script", "자동 시험 요청")
	script["promptSource"], script["entrypoint"] = "sdk", "sdk-cli"
	requests, _, err := ReadConversation(transcriptRows(t, prompt, questionRow("question", "ask", "어떤 색으로 할까요?"), notice, tagged, answer, script))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Text != "버튼 색을 바꿔줘" || requests[0].Source != "claude:vscode" || requests[1].Text != "질문: 어떤 색으로 할까요?\n선택: 파랑" {
		t.Fatalf("VS Code prompt or its UI answer lost, or a notice became a request: %+v", requests)
	}
}

func TestConversationKeepsSkillCallsAndSkipsCommandRecords(t *testing.T) {
	row := func(id, version, entrypoint, text string) map[string]any {
		return map[string]any{"type": "user", "uuid": id, "version": version, "entrypoint": entrypoint, "message": map[string]any{"content": text}}
	}
	caveat := row("caveat", "2.1.233", "claude-vscode", "<local-command-caveat>Caveat: run directly</local-command-caveat>")
	caveat["isMeta"] = true
	builtin := row("builtin", "2.1.233", "claude-vscode", "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args>sonnet</command-args>")
	builtin["parentUuid"] = "caveat"
	requests, _, err := ReadConversation(transcriptRows(t,
		row("bare", "2.1.109", "cli", "<command-name>/model</command-name>\n<command-message>model</command-message>\n<command-args></command-args>"),
		row("stdout", "2.1.109", "cli", "<local-command-stdout>Set model</local-command-stdout>"),
		row("stderr", "2.1.109", "cli", "<local-command-stderr>failed</local-command-stderr>"),
		row("skill", "2.1.109", "cli", "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>변경을 다시 확인해</command-args>"),
		row("plan", "2.1.270", "cli", "<command-name>/plan</command-name>\n<command-message>plan</command-message>\n<command-args>계획을 세워</command-args>"),
		row("scripted", "2.1.270", "sdk-cli", "<command-name>/review</command-name>\n<command-args>자동 검사</command-args>"),
		caveat, builtin))
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0].Text != "/review 변경을 다시 확인해" || requests[1].Text != "/plan 계획을 세워" || requests[0].Source != "claude:command" || requests[1].Source != "claude:command" {
		t.Fatalf("skill call lost or a command record became a request: %+v", requests)
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

func TestCodexConversationKeepsTheRequestUnderAppContext(t *testing.T) {
	attachedOnly := "# Files mentioned by the user:\n\n## notes.txt: /tmp/notes.txt\n\nThe attached pasted text file(s) contain the user's request.\n\n## My request for Codex:"
	requests, _, err := ReadConversation(transcriptRows(t,
		codexUser("files", "# Files mentioned by the user:\n\n## shot.png: /tmp/shot.png\n\n## My request for Codex:\n아이콘 색을 고쳐줘\n"),
		codexUser("browser", "<in-app-browser-context source=\"ambient-ui-state\">\nThis block is automatically supplied ambient UI state.\n# In app browser:\n- Current URL: http://localhost:3000/\n</in-app-browser-context>\n## My request:\n로그인 했어"),
		codexUser("ide", "# Context from my IDE setup:\n\n## Open tabs:\n- app.ts: src/app.ts\n\n## My request:\n이 파일을 정리해줘"),
		codexUser("attached", attachedOnly),
		codexUser("ambient", "<in-app-browser-context source=\"ambient-ui-state\">\n# In app browser:\n- Current URL: http://localhost:3000/\n</in-app-browser-context>"),
		codexUser("heartbeat", "<heartbeat>\n  <instructions>남은 작업을 이어간다</instructions>\n</heartbeat>"),
		codexUser("delegation", "<codex_delegation>\n  <input>이 작업을 이어가세요</input>\n</codex_delegation>"),
	))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range requests {
		got = append(got, r.Text)
	}
	if strings.Join(got, "|") != strings.Join([]string{"아이콘 색을 고쳐줘", "로그인 했어", "이 파일을 정리해줘", attachedOnly}, "|") {
		t.Fatalf("app context hid the request or became one: %q", got)
	}
}

func replyItem(call string, index int, question, answer string) map[string]any {
	ref, _ := json.Marshal([]any{"request_user_input_async", call, index})
	return map[string]any{"questionItemId": string(ref), "question": question, "answer": answer}
}

func questionReply(t *testing.T, id string, items ...map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return codexUser(id, "<send_user_message_question_reply>\n"+string(raw)+"\n</send_user_message_question_reply>")
}

func TestCodexQuestionReplyBecomesChoices(t *testing.T) {
	call := map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "request_user_input_async", "call_id": "call-1",
		"arguments": `{"questions":[{"title":"어느 쪽으로 할까요?","options":["왼쪽","오른쪽"]},{"title":"언제 할까요?"}]}`}}
	broken := "<send_user_message_question_reply>\n선택을 읽을 수 없음\n</send_user_message_question_reply>"
	requests, _, err := ReadConversation(transcriptRows(t,
		codexUser("ask", "정할 것을 물어봐"), call,
		questionReply(t, "reply", replyItem("call-1", 0, "어느 쪽?", "왼쪽"), replyItem("call-1", 1, "언제?", "오늘")),
		questionReply(t, "unpaired", replyItem("call-2", 0, "범위를 넓힐까요?", "아니요")),
		codexUser("broken", broken),
	))
	if err != nil {
		t.Fatal(err)
	}
	want := []Request{
		{Text: "정할 것을 물어봐", Source: "codex:user"},
		{Text: "질문: 어느 쪽으로 할까요?\n선택: 왼쪽", Source: "codex:choice", QuestionID: "call-1"},
		{Text: "질문: 언제 할까요?\n선택: 오늘", Source: "codex:choice", QuestionID: "call-1"},
		{Text: "질문: 범위를 넓힐까요?\n선택: 아니요", Source: "codex:choice", QuestionID: "call-2"},
		{Text: broken, Source: "codex:user"},
	}
	if len(requests) != len(want) || requests[1].ID == requests[2].ID {
		t.Fatalf("question reply not split into choices: %+v", requests)
	}
	for i, w := range want {
		if r := requests[i]; r.Text != w.Text || r.Source != w.Source || r.QuestionID != w.QuestionID {
			t.Fatalf("request %d: got %+v, want %+v", i, r, w)
		}
	}
}

func TestCodexBlockingQuestionAnswersBecomeChoices(t *testing.T) {
	call := map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call", "name": "request_user_input", "call_id": "call-9",
		"arguments": `{"questions":[{"header":"소스","id":"design_source","question":"어떤 디자인 소스를 기준으로 할까요?"},{"id":"scope","question":"범위는?"},{"id":"skipped","question":"건너뛴 질문"}]}`}}
	output := map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-9",
		"output": `{"answers":{"scope":{"answers":["home","shared"]},"design_source":{"answers":["피그마 원본"]}}}`}}
	other := map[string]any{"type": "response_item", "payload": map[string]any{"type": "function_call_output", "call_id": "call-1", "output": `{"answers":{"scope":{"answers":["x"]}}}`}}
	requests, _, err := ReadConversation(transcriptRows(t, codexUser("ask", "물어보고 정해"), call, output, other))
	if err != nil || len(requests) != 3 {
		t.Fatalf("blocking question answers not read: %+v %v", requests, err)
	}
	for i, want := range []string{"질문: 어떤 디자인 소스를 기준으로 할까요?\n선택: 피그마 원본", "질문: 범위는?\n선택: home, shared"} {
		if r := requests[i+1]; r.Text != want || r.Source != "codex:choice" || r.QuestionID != "call-9" {
			t.Fatalf("choice %d: got %+v", i, r)
		}
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

func TestRepoRootFindsTheGitFolderOrAWorktreesGitFile(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, worktree, plain := filepath.Join(parent, "repo"), filepath.Join(parent, "wt"), filepath.Join(parent, "plain")
	for _, dir := range []string{filepath.Join(repo, ".git"), filepath.Join(repo, "a", "b"), worktree, plain} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+repo+"/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{filepath.Join(repo, "a", "b"): repo, worktree: worktree, plain: "", filepath.Join(parent, "gone"): "", "": ""} {
		if got := RepoRoot(dir); got != want {
			t.Errorf("%q: got %q, want %q", dir, got, want)
		}
	}
}
