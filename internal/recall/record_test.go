package recall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const claudeSession = `{"type":"user","promptSource":"typed","sessionId":"abcdef12-0000-0000-0000-000000000000","cwd":"/work/repo","gitBranch":"main","timestamp":"2026-10-07T01:00:00Z","message":{"role":"user","content":"헤더 숨김 빼줘 token=abcdef123456"}}
{"type":"assistant","timestamp":"2026-10-07T01:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"e1","name":"Edit","input":{"file_path":"/work/repo/src/Header.tsx"}},{"type":"tool_use","id":"b1","name":"Bash","input":{"command":"pnpm test"}}]}}
{"type":"user","timestamp":"2026-10-07T01:02:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b1","is_error":true}]}}
{"type":"assistant","timestamp":"2026-10-07T01:03:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"b2","name":"Bash","input":{"command":"git commit -m 'remove hide'"}}]}}
{"type":"user","timestamp":"2026-10-07T01:04:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"b2","is_error":false}]}}
`

func recordFixture(t *testing.T) (raw, vault, path string) {
	home, raw := fixture(t)
	vault = filepath.Join(home, "vault")
	t.Setenv("HARNESS_VAULT_DIR", vault)
	os.MkdirAll(filepath.Join(vault, "personal", "10_프로젝트"), 0o700)
	path = filepath.Join(raw, "claude", "2026-10-07", "abcdef12-0000-0000-0000-000000000000.jsonl")
	write(t, path, claudeSession)
	return raw, vault, path
}

func TestReadSessionFixedFields(t *testing.T) {
	_, _, path := recordFixture(t)
	s, ok := readSession("claude", path)
	if !ok || s.cwd != "/work/repo" || s.branch != "main" || len(s.requests) != 1 {
		t.Fatal(ok, s)
	}
	if len(s.files) != 1 || s.files[0] != "src/Header.tsx" {
		t.Fatal(s.files)
	}
	if len(s.commands) != 2 || s.commands[0].result != "실패" || s.commands[1].result != "성공" {
		t.Fatal(s.commands)
	}
	// The folder is gone, so git cannot say; a commit run that succeeded whole still made it.
	newGitCache().readGit(&s)
	text := render(s, summary{}, false)
	for _, want := range []string{"## 내용", "## 자료", "## 소스", "summary: 없음", "`pnpm test` → 실패", "### 커밋\n- remove hide (repo, 해시 모름) [tool:b2]", "token=[가림]"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing", want, "\n", text)
		}
	}
	if strings.Contains(text, "abcdef123456") || strings.Contains(text, "\nstatus:") {
		t.Fatal("secret kept or status key written", text)
	}
}

func TestRecordKeepsAnswersEndingCompactionAndCommitSubject(t *testing.T) {
	_, _, path := recordFixture(t)
	at := func(minute int) string { return time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC).Format(time.RFC3339) }
	human := func(id, text string, minute int) map[string]any {
		row := recordHuman(id, text, time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC))
		row["sessionId"], row["cwd"] = "abcdef12-0000-0000-0000-000000000000", "/work/repo"
		return row
	}
	compaction := "This session is being continued from a previous conversation.\n\nSummary:\n요약 언어: 한국어\n### 남은 일\n- 로그인 다시 확인\n\nIf you need specific details from before compaction, read the full transcript."
	commit := "git commit -m \"$(cat <<'EOF'\nfix: 헤더 숨김 제거\n\n본문\nEOF\n)\""
	write(t, path, jsonLines(t,
		human("first", "<pasted_content id=\"p\"><div>붙여넣은 긴 글</div></pasted_content id=\"p\"> [Image #1] 헤더 숨김 빼줘", 0),
		said(at(1), "## 결론\n헤더 숨김을 뺐습니다.\n| 칸 | 값 |\n|---|---|", false),
		human("second", "왜 요약이 안 돼?", 2),
		map[string]any{"type": "user", "isCompactSummary": true, "timestamp": at(3), "message": map[string]any{"role": "user", "content": compaction}},
		map[string]any{"type": "assistant", "timestamp": at(4), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "c1", "name": "Bash", "input": map[string]any{"command": commit}}}}},
		map[string]any{"type": "user", "timestamp": at(5), "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "is_error": false}}}},
		said(at(6), "## 원인\n**원인은 OAuth 만료입니다.**\n- 다시 로그인하면 됩니다.", false),
	))
	s, ok := readSession("claude", path)
	if !ok {
		t.Fatal("session not read", s)
	}
	newGitCache().readGit(&s)
	text := render(s, summary{}, false)
	for _, want := range []string{
		"title: \"헤더 숨김 빼줘\"",
		"헤더 숨김 빼줘\n  → AI(주장): 헤더 숨김을 뺐습니다.",
		"### 끝난 지점\n- 원인은 OAuth 만료입니다. 다시 로그인하면 됩니다. (AI 주장)",
		"### 압축 요약 (AI 작성·주장)\n> 요약 언어: 한국어\n> ### 남은 일\n> - 로그인 다시 확인\n",
		"### 커밋\n- fix: 헤더 숨김 제거 (repo, 해시 모름) [tool:c1]",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "continued from a previous") || strings.Contains(text, "If you need specific") {
		t.Fatal("compaction framing kept", text)
	}
	if _, ok := summaryOf([]byte(text)); ok {
		t.Fatal("quoted compaction headings read back as a model summary", text)
	}
	if out, err := Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)); err != nil || out["written"] != 1 {
		t.Fatal(out, err)
	}
	if hits := Find("OAuth 만료", 5); len(hits) != 1 || !strings.Contains(hits[0].Line, "OAuth") {
		t.Fatal("agent conclusion not searchable", hits)
	}
}

func TestBuildWritesOnceAndRewritesWhenGrown(t *testing.T) {
	_, vault, path := recordFixture(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	out, err := Build(since, now)
	if err != nil || out["written"] != 1 {
		t.Fatal(out, err)
	}
	records, _ := filepath.Glob(filepath.Join(RecordDir(), "2026-10-07", "*-abcdef12.md"))
	if len(records) != 1 {
		t.Fatal(records)
	}
	if entries, _ := os.ReadDir(filepath.Join(vault, "personal", "10_프로젝트")); len(entries) != 0 {
		t.Fatal("record written into the vault", entries)
	}
	if out, _ = Build(since, now); out["written"] != 0 {
		t.Fatal("unchanged session rewritten", out)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","promptSource":"typed","timestamp":"2026-10-07T02:00:00Z","message":{"role":"user","content":"하나 더"}}` + "\n")
	f.Close()
	if out, _ = Build(since, now); out["written"] != 1 {
		t.Fatal("grown session not rewritten", out)
	}
	raw, _ := os.ReadFile(records[0])
	if !strings.Contains(string(raw), "하나 더") {
		t.Fatal(string(raw))
	}
	if out, _ = Build(since, now.Add(-time.Hour*10)); out["written"] != 0 {
		t.Fatal("unchanged active record rewritten", out)
	}
}

func appendRecordRows(t *testing.T, path string, rows ...map[string]any) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(jsonLines(t, rows...))
	closed := f.Close()
	if err != nil || closed != nil {
		t.Fatal(err, closed)
	}
}

func recordHuman(id, text string, at time.Time) map[string]any {
	return map[string]any{"type": "user", "uuid": id, "promptSource": "typed", "timestamp": at.Format(time.RFC3339Nano), "message": map[string]any{"content": text}}
}

func TestBuildNoModelStoresRecentChoiceAndRepeatedConfirmation(t *testing.T) {
	_, _, path := recordFixture(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	question := "묶음 안내를 어떻게 할까요?"
	appendRecordRows(t, path,
		map[string]any{"type": "assistant", "uuid": "asked", "timestamp": now.Add(-2 * time.Minute).Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "ask", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"question": question}}}}}}},
		map[string]any{"type": "user", "uuid": "latest-choice", "version": "2.1.292", "timestamp": now.Add(-time.Minute).Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "ask"}}}, "toolUseResult": map[string]any{"answers": map[string]any{question: "1번"}}},
		recordHuman("latest-confirmation", "1번", now.Add(-time.Minute)),
	)
	out, err := Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 1 || out["pending"] != 0 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	raw, err := os.ReadFile(out["files"].([]string)[0])
	if err != nil || header(raw, "summary_state") != "off" || header(raw, "summary_cursor") != "0" || strings.Contains(string(raw), "요약 갱신:") {
		t.Fatal(string(raw), err)
	}
	for _, want := range []string{question, "선택: 1번", "latest-choice:ask:0", "claude:choice", "latest-confirmation", "claude:typed] 1번"} {
		if !strings.Contains(string(raw), want) {
			t.Fatal("recent user source omitted", want, string(raw))
		}
	}
	if _, err := os.Stat(filepath.Join(Dir(), "model-calls-2026-10-08")); !os.IsNotExist(err) {
		t.Fatal("no-model build touched model budget", err)
	}
}

func TestBuildUpdatesFixedFieldsAndKeepsStoredSummary(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	old := summary{Title: "기존 작업", Decisions: []decision{{What: "묶음 안내 미등록", Why: "효과 미확인"}}, Todo: []string{"실제 새 세션 검증"}}
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	write(t, dest, render(s, old, true))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	appendRecordRows(t, path, recordHuman("recent", "최근 추가 결정도 보존해줘", now.Add(-time.Minute)))
	out, err := Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 1 || out["pending"] != 0 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	kept, ok := summaryOf(raw)
	if !ok || kept.Title != old.Title || len(kept.Decisions) != 1 || kept.Decisions[0].Why != old.Decisions[0].Why || kept.Todo[0] != old.Todo[0] || !strings.Contains(string(raw), "최근 추가 결정도 보존해줘") || header(raw, "summary_state") != "off" {
		t.Fatal("fixed update lost the stored summary", string(raw))
	}
}

func oldRecordSource(t *testing.T) string {
	t.Helper()
	raw, _, path := recordFixture(t)
	older := filepath.Join(raw, "claude", "2026-09-01", filepath.Base(path))
	write(t, older, strings.ReplaceAll(claudeSession, "2026-10-07", "2026-09-01"))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	return older
}

func TestBuildDiscoversRecentlyActiveSessionFromOldStartDate(t *testing.T) {
	path := oldRecordSource(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	appendRecordRows(t, path, recordHuman("recent", "오래된 세션에서 새 작업을 이어가자", now.Add(-time.Minute)))
	out, err := Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 1 || out["pending"] != 0 {
		t.Fatal(out, err)
	}
	dest := out["files"].([]string)[0]
	raw, _ := os.ReadFile(dest)
	if !strings.Contains(string(raw), "오래된 세션에서 새 작업을 이어가자") || header(raw, "raw") != path || !strings.Contains(dest, "2026-09-01") {
		t.Fatal("new activity was filtered by start date or original source was moved", dest, string(raw))
	}
}

func TestOldReadyRecordStaysUntouchedThenCapturesNewActivity(t *testing.T) {
	path := oldRecordSource(t)
	s, _ := readSession("claude", path)
	old := summary{Title: "오래된 목표", Todo: []string{"남은 확인"}}
	dest := filepath.Join(RecordDir(), "2026-09-01", "1000-repo-abcdef12.md")
	before := render(s, old, true)
	write(t, dest, before)
	oldTime := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, p := range []string{path, dest} {
		if err := os.Chtimes(p, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	out, err := Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 0 {
		t.Fatal("unchanged ready record reprocessed", out, err)
	}
	unchanged, _ := os.ReadFile(dest)
	if string(unchanged) != before {
		t.Fatal("unchanged historical record rewritten", string(unchanged))
	}
	appendRecordRows(t, path, recordHuman("resumed", "한 달 전 작업의 새 요청", now.Add(-2*time.Hour)))
	out, err = Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 1 || out["pending"] != 0 {
		t.Fatal("old ready record did not discover its new content", out, err)
	}
	after, _ := os.ReadFile(dest)
	kept, ok := summaryOf(after)
	if !ok || kept.Title != old.Title || kept.Todo[0] != old.Todo[0] || !strings.Contains(string(after), "한 달 전 작업의 새 요청") || header(after, "summary_state") != "off" {
		t.Fatal(string(after))
	}
}

func TestOldReadyRecordDetectsSameSizeRewriteWithoutNewTimestamp(t *testing.T) {
	path := oldRecordSource(t)
	s, _ := readSession("claude", path)
	dest := filepath.Join(RecordDir(), "2026-09-01", "1000-repo-abcdef12.md")
	write(t, dest, render(s, summary{Title: "기존 목표", Todo: []string{"보류한 확인"}}, true))
	oldTime := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(dest, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(path)
	write(t, path, strings.ReplaceAll(string(text), "헤더", "푸터"))
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	out, err := Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 1 || out["pending"] != 0 {
		t.Fatal("historical same-size source rewrite not discovered", out, err)
	}
	after, _ := os.ReadFile(dest)
	kept, ok := summaryOf(after)
	if !ok || kept.Todo[0] != "보류한 확인" || !strings.Contains(string(after), "푸터 숨김") || header(after, "raw_sha256") == s.hash {
		t.Fatal("source rewrite or previous state lost", string(after))
	}
}

func TestBuildDiscoveryReadFailureStaysRetryable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 fixture")
	}
	_, _, path := recordFixture(t)
	folder := filepath.Dir(path)
	if err := os.Chmod(folder, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(folder, 0o700) })
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	out, err := Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 0 || len(out["failed"].([]string)) != 1 {
		t.Fatal("source discovery failure was silently excluded", out, err)
	}
	if err := os.Chmod(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	out, err = Build(now.AddDate(0, 0, -14), now)
	if err != nil || out["written"] != 1 || len(out["failed"].([]string)) != 0 {
		t.Fatal("read failure permanently excluded source", out, err)
	}
}

func TestBuildSkipsWhenNothingTyped(t *testing.T) {
	_, _, path := recordFixture(t)
	write(t, path, `{"type":"user","timestamp":"2026-10-07T01:00:00Z","message":{"role":"user","content":"<command-name>x</command-name>"}}`+"\n")
	out, _ := Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if out["written"] != 0 {
		t.Fatal(out)
	}
	raw, _ := os.ReadFile(filepath.Join(Dir(), fmt.Sprintf("not-sessions-%d.json", recordFormat)))
	if !strings.Contains(string(raw), "abcdef12") {
		t.Fatal("not remembered", string(raw))
	}
}

func TestFindRanksRecordAndEvalCounts(t *testing.T) {
	_, vault, _ := recordFixture(t)
	Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	write(t, filepath.Join(vault, "personal", "10_프로젝트", "_미분류", "작업 기록", "다른.md"), "# 다른 일\n결제 화면\n")
	hits := Find("헤더를 숨김", 5)
	// The only request is the title, so no line repeats it.
	if len(hits) != 1 || !strings.Contains(hits[0].Path, "abcdef12") || !strings.Contains(hits[0].Title, "헤더") || hits[0].Line != "" {
		t.Fatal(hits)
	}
	questions := filepath.Join(t.TempDir(), "q.json")
	write(t, questions, `[{"id":"q1","question":"헤더 숨김 어디까지 했지","expect_sessions":["abcdef12"],"keywords":["헤더","숨김"]},{"id":"q2","question":"로그인 고쳤나","expect_sessions":["zzzzzzzz"],"keywords":["로그인"]}]`)
	out, err := Eval(questions)
	if err != nil || out["keyword_hits"] != 1 || out["question_hits"] != 1 {
		t.Fatal(out, err)
	}
	results := out["results"].([]evalResult)
	if len(results) != 2 || !results[0].KeywordHit || !results[0].QuestionHit || results[1].KeywordHit || results[1].QuestionHit {
		t.Fatal("Per-question results disagree with totals", results)
	}
}

func TestFormatRebuildKeepsModelSummary(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	newGitCache().readGit(&s)
	m := summary{Title: "헤더 숨김 제거", Decisions: []decision{{What: "훅 삭제", Why: "항상 보이게"}}, Todo: []string{"화면 확인"}, Lessons: []string{"주석도 같이 고친다"}}
	old := strings.Replace(render(s, m, true), fmt.Sprintf("format: %d\n", recordFormat), "", 1)
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	write(t, dest, old)
	out, err := Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	if err != nil || out["written"] != 1 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	if string(raw) != render(s, m, true) {
		t.Fatalf("summary lost on rebuild:\n%s", raw)
	}
}

func jsonLines(t *testing.T, rows ...map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(append(raw, '\n'))
	}
	return b.String()
}

func said(at, text string, tool bool) map[string]any {
	content := []any{map[string]any{"type": "text", "text": text}}
	if tool {
		content = append(content, map[string]any{"type": "tool_use", "id": "t", "name": "Read", "input": map[string]any{}})
	}
	return map[string]any{"type": "assistant", "timestamp": at, "message": map[string]any{"role": "assistant", "model": "claude-opus-5-5", "content": content}}
}

func toolResult(at string) map[string]any {
	return map[string]any{"type": "user", "timestamp": at, "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "ok"}}}}
}

func lookupSessions(t *testing.T) (claude, codex string) {
	t.Helper()
	raw, _, claude := recordFixture(t)
	at := func(minute int) string { return time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC).Format(time.RFC3339) }
	use := func(id, command string, minute int) map[string]any {
		return map[string]any{"type": "assistant", "timestamp": at(minute), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}}
	}
	result := func(id string, failed bool, text string, minute int) map[string]any {
		return map[string]any{"type": "user", "timestamp": at(minute), "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": failed, "content": []any{map[string]any{"type": "text", "text": text}}}}}}
	}
	human := recordHuman("first", "어제 한 거 보여줘", time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC))
	human["sessionId"], human["cwd"] = "abcdef12-0000-0000-0000-000000000000", "/work/repo"
	write(t, claude, jsonLines(t, human,
		use("r1", "harness recall day 어제", 1), result("r1", false, "그날 기록된 요청이 없다: 2026-10-06", 2),
		use("r2", "rtk harness recall find test 결과", 3), result("r2", false, "기록/2026-10-06/x.md · 제목 · 줄", 4),
		use("r3", "harness recall source abcdef12 PDF", 5), result("r3", true, "zsh: command not found: harness", 6),
		use("r4", "python3 - <<'PY'\nprint('예: harness recall find 말')\nPY", 7), result("r4", false, "예: harness recall find 말", 8),
		use("r5", "cd /work/repo && rtk proxy /Users/me/.local/share/harness/bin/harness-0.1.0 recall find TSK-1344", 9), result("r5", false, "기록/2026-10-06/y.md · TSK-1344", 10),
	))
	codex = filepath.Join(raw, "codex", "2026-10-07", "rollout-2026-10-07T10-00-00-01a0aaaa-0000-7000-8000-00000000c0de.jsonl")
	row := func(kind string, minute int, payload map[string]any) map[string]any {
		return map[string]any{"type": kind, "timestamp": at(minute), "payload": payload}
	}
	script := "const results = await Promise.allSettled([\n  tools.exec_command({cmd:\"rtk cat a.md\",workdir:\"/work/repo\"}),\n  tools.exec_command({cmd:\"rtk harness recall find 'TSK-1344'\",workdir:\"/work/repo\"}),\n]);"
	write(t, codex, jsonLines(t,
		row("session_meta", 0, map[string]any{"id": "01a0aaaa-0000-7000-8000-00000000c0de", "cwd": "/work/repo"}),
		row("response_item", 0, map[string]any{"type": "message", "role": "user", "id": "u1", "content": []any{map[string]any{"type": "input_text", "text": "어제 한 거 보여줘"}}}),
		row("response_item", 1, map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c1", "arguments": `{"cmd":"rtk harness recall day 2026-10-15 web_app"}`}),
		row("response_item", 2, map[string]any{"type": "function_call_output", "call_id": "c1", "output": "Process exited with code 0\nOutput:\n그날 찾을 말에 맞는 요청이 없다: 2026-10-15 (찾을 말: web_app) — 말을 빼면 대화 9개"}),
		row("response_item", 3, map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c2", "input": script}),
		row("response_item", 4, map[string]any{"type": "custom_tool_call_output", "call_id": "c2", "output": []any{
			map[string]any{"type": "input_text", "text": "Script completed\nWall time 0.1 seconds\nOutput:\n"},
			map[string]any{"type": "input_text", "text": `{"index":0,"result":{"status":"fulfilled","value":{"exit_code":0,"output":"# a"}}}`},
			map[string]any{"type": "input_text", "text": `{"index":1,"result":{"status":"fulfilled","value":{"exit_code":0,"output":"찾은 기록 없음 — 다른 말로 다시 찾거나"}}}`},
		}}),
		// The agent writes each script, so results also come as one array, or numbered "i" without the wrapper.
		row("response_item", 5, map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c3", "input": "const r = await Promise.allSettled([tools.exec_command({cmd:'harness recall day 10/15'})]); text(JSON.stringify(r))"}),
		row("response_item", 6, map[string]any{"type": "custom_tool_call_output", "call_id": "c3", "output": []any{
			map[string]any{"type": "input_text", "text": `[{"status":"fulfilled","value":{"exit_code":1,"output":"날짜를 읽지 못했다"}}]`},
		}}),
		row("response_item", 7, map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c4", "input": "await Promise.all([tools.exec_command({cmd:\"rtk proxy harness recall find rsi\"}), tools.exec_command({cmd:\"rtk cat b.md\"})])"}),
		row("response_item", 8, map[string]any{"type": "custom_tool_call_output", "call_id": "c4", "output": []any{
			map[string]any{"type": "input_text", "text": `{"i":0,"result":{"exit_code":0,"output":"기록/2026-08-31/x.md · 제목"}}`},
			map[string]any{"type": "input_text", "text": `{"i":1,"result":{"exit_code":0,"output":"# b"}}`},
		}}),
		// Places count every tool the script calls, and one block may hold several numbered lines.
		row("response_item", 9, map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c5", "input": "const results = await Promise.allSettled([\n  tools.mcp__codex_app__read_thread({threadId:\"x\"}),\n  tools.exec_command({cmd:\"rtk harness recall source 01a0 --limit 10\"}),\n]);"}),
		row("response_item", 10, map[string]any{"type": "custom_tool_call_output", "call_id": "c5", "output": []any{
			map[string]any{"type": "input_text", "text": "Script completed\nWall time 0.2 seconds\nOutput:\n"},
			map[string]any{"type": "input_text", "text": "Warning: truncated output (original token count: 171795)\nTotal output lines: 2\n\n" + `{"i":0,"status":"fulfilled","value":{"content":[]}}` + "\n" + `{"i":1,"status":"fulfilled","value":{"exit_code":0,"output":"구간 0:0 / 0"}}`},
		}}),
		// A script that prints each result in turn, the first as plain text.
		row("response_item", 11, map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "c6", "input": "const r = await tools.exec_command({cmd:\"rtk proxy harness recall source 01a11bf3 --limit 4\"});text(r.output);\ntext(await tools.exec_command({cmd:\"rtk proxy sed -n '1,2p' a.md\"}))"}),
		row("response_item", 12, map[string]any{"type": "custom_tool_call_output", "call_id": "c6", "output": []any{
			map[string]any{"type": "input_text", "text": "Script completed\nWall time 0.5 seconds\nOutput:\n"},
			map[string]any{"type": "input_text", "text": "rtk: Failed to execute command: harness: No such file or directory (os error 2)\n"},
			map[string]any{"type": "input_text", "text": `{"exit_code":0,"output":"# a"}`},
		}}),
	))
	return claude, codex
}

func TestALookupWrittenInsideAHeredocIsNotRun(t *testing.T) {
	inside := "cd /x && python3 - <<'EOF'\nwant = \"`cd /work/repo && rtk harness recall find TSK-1` → 결과 있음\"\nEOF"
	if recallCommand.MatchString(shellOnly(inside)) {
		t.Fatal("heredoc 안의 글을 기억 명령으로 봄")
	}
	if !recallCommand.MatchString(shellOnly(inside+"\nharness recall day 어제")) || !recallCommand.MatchString(shellOnly("echo a <<b; harness recall find x")) {
		t.Fatal("heredoc 뒤나 같은 줄의 기억 명령을 놓침")
	}
}

func TestCommitSubjectsReadQuotedAndHeredocMessages(t *testing.T) {
	for command, want := range map[string]string{
		`git -C "/a b" commit -m "fix: 제목"`:                                                                        "fix: 제목",
		`git -c user.name=t -c user.email=t@t commit -qm init && cd /x`:                                            "init",
		"git commit -F - -m \"$(cat <<'EOF'\nfix: 여러 줄\n\n본문\nEOF\n)\"":                                            "fix: 여러 줄",
		"git add a && git commit -q -F - <<'MSG'\nrefactor: 입력으로 준 제목\n\n본문\nMSG\ngit log -1":                      "refactor: 입력으로 준 제목",
		"cat > /tmp/msg.txt <<'EOF'\n\nfix: 파일로 준 제목\nEOF\ngit commit -qF /tmp/msg.txt":                            "fix: 파일로 준 제목",
		"cat <<EOF > \"$T/m\"\nfix: 다른 파일\nEOF\ncat <<EOF >/tmp/msg\nfeat: 맞는 파일\nEOF\ngit commit --file=/tmp/msg": "feat: 맞는 파일",
	} {
		if got := commitSubjects(command); len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %q, want %q", command, got, want)
		}
	}
}

func TestALongRunOfChecksKeepsTheCommit(t *testing.T) {
	s := session{id: "s", cwd: "/w", events: []gitEvent{{kind: "commit", repo: "w", hash: "1a2b3c4d5e", subject: "fix: 먼저", ref: "dev", source: "tool:c"}}}
	for i := range 20 {
		s.commands = append(s.commands, command{text: fmt.Sprintf("pnpm test %d", i), source: "tool:t", result: "성공", check: true})
	}
	text := render(s, summary{}, false)
	if !strings.Contains(text, "### 커밋\n- 1a2b3c4d fix: 먼저 (w dev) [tool:c]") || strings.Contains(text, "pnpm test 4`") || !strings.Contains(text, "pnpm test 19`") {
		t.Fatal("검사가 많으면 앞선 커밋이 기록에서 밀려남", text)
	}
}

func TestRecordKeepsHowEachMemoryLookupCameBack(t *testing.T) {
	claude, codex := lookupSessions(t)
	at := func(minute int) string {
		return time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC).Local().Format("01-02 15:04")
	}
	s, ok := readSession("claude", claude)
	if !ok {
		t.Fatal("session not read", s)
	}
	text := render(s, summary{}, false)
	want := "### 기억 명령\n- " + at(1) + " `harness recall day 어제` → 빈 결과 [tool:r1]\n- " + at(3) + " `rtk harness recall find test 결과` → 결과 있음 [tool:r2]\n- " + at(5) + " `harness recall source abcdef12 PDF` → 오류 [tool:r3]\n- " + at(9) + " `cd /work/repo && rtk proxy /Users/me/.local/share/harness/bin/harness-0.1.0 recall find TSK-1344` → 결과 있음 [tool:r5]\n"
	if !strings.Contains(text, want) || strings.Count(text, "recall find test") != 1 || strings.Contains(text, "print(") {
		t.Fatalf("missing %q, or a lookup was also listed as a check, or quoted words counted as one\n%s", want, text)
	}
	s, ok = readSession("codex", codex)
	if !ok {
		t.Fatal("codex session not read", s)
	}
	text = render(s, summary{}, false)
	want = "### 기억 명령\n- " + at(1) + " `rtk harness recall day 2026-10-15 web_app` → 빈 결과 [tool:c1]\n- " + at(3) + " `rtk harness recall find 'TSK-1344'` → 빈 결과 [tool:c2#1]\n- " + at(5) + " `harness recall day 10/15` → 오류 [tool:c3#0]\n- " + at(7) + " `rtk proxy harness recall find rsi` → 결과 있음 [tool:c4#0]\n- " + at(9) + " `rtk harness recall source 01a0 --limit 10` → 빈 결과 [tool:c5#1]\n- " + at(11) + " `rtk proxy harness recall source 01a11bf3 --limit 4` → 오류 [tool:c6#0]\n"
	if !strings.Contains(text, want) || strings.Contains(text, "rtk cat a.md") || strings.Contains(text, "rtk cat b.md") {
		t.Fatalf("missing %q, or a script command that is no lookup was kept\n%s", want, text)
	}
}

func TestRecordReadsChecksOutsideHeredocsOnceAndMarksPipelines(t *testing.T) {
	_, _, path := recordFixture(t)
	at := func(minute int) string { return time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC).Format(time.RFC3339) }
	human := recordHuman("first", "검사 돌려줘", time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC))
	human["sessionId"], human["cwd"] = "abcdef12-0000-0000-0000-000000000000", "/work/repo"
	use := func(id, command string, minute int) map[string]any {
		return map[string]any{"type": "assistant", "timestamp": at(minute), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}}
	}
	result := func(id string, minute int) map[string]any {
		return map[string]any{"type": "user", "timestamp": at(minute), "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "is_error": false}}}}
	}
	write(t, path, jsonLines(t, human,
		use("n1", "cat > note.md <<'EOF'\npnpm test 는 나중에\nEOF", 1), result("n1", 2),
		use("p1", "go test ./... | tail -3", 3), use("p1", "go test ./... | tail -3", 3), result("p1", 4),
		use("v1", "go vet ./... || true", 5), result("v1", 6),
	))
	s, ok := readSession("claude", path)
	if !ok || len(s.commands) != 2 || s.commands[0].result != "끝 명령 성공" || s.commands[1].result != "성공" {
		t.Fatal("heredoc 속 말을 검사로 읽었거나, 같은 호출을 두 번 셌거나, 파이프 끝 결과를 성공으로 적음", ok, s.commands)
	}
	if text, _ := sourceText(s, "tool:p1"); text != "" {
		t.Fatal("파이프 끝 명령 결과를 검사 근거로 씀", text)
	}
}
