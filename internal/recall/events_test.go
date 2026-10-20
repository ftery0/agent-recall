package recall

import (
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoAt is a repository at dir with one commit on main; with a remote, a bare repository beside it
// that it has pushed to.
func repoAt(t *testing.T, dir string, remote bool, at time.Time) {
	t.Helper()
	gitAt(t, filepath.Dir(dir), at, "init", "-q", "-b", "main", dir)
	gitAt(t, dir, at, "commit", "-q", "--allow-empty", "-m", "first")
	if remote {
		bare := filepath.Join(filepath.Dir(dir), "remote", filepath.Base(dir)+".git")
		gitAt(t, filepath.Dir(dir), at, "init", "-q", "--bare", bare)
		gitAt(t, dir, at, "remote", "add", "origin", bare)
		gitAt(t, dir, at, "push", "-q", "-u", "origin", "main")
	}
}

func bash(id, command string, at time.Time) map[string]any {
	return map[string]any{"type": "assistant", "timestamp": at.Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}}
}

func bashResult(id, output string, at time.Time) map[string]any {
	return map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": output}}}}
}

// The case that started this: work made in a repository with no remote was copied to another and
// pushed there, quietly. The next conversation about to push must hear where it went.
func TestAQuietPushFromACopyIsRememberedBeforeTheNextPush(t *testing.T) {
	_, raw := fixture(t)
	parent := t.TempDir()
	harness, published := filepath.Join(parent, "harness"), filepath.Join(parent, "agent-recall")
	t0 := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	repoAt(t, harness, false, t0.Add(-time.Hour))
	repoAt(t, published, true, t0.Add(-time.Hour))
	write(t, filepath.Join(harness, "notes.md"), "기억\n")
	// What the agent ran, as git saw it: rtk prints "ok" for all of it.
	write(t, filepath.Join(published, "notes.md"), "기억\n")
	gitAt(t, published, t0.Add(2*time.Minute), "add", "-A")
	gitAt(t, published, t0.Add(2*time.Minute+time.Second), "commit", "-q", "-m", "feat: 기억 개선")
	gitAt(t, published, t0.Add(2*time.Minute+2*time.Second), "push", "-q")
	hash := gitAt(t, published, t0, "rev-parse", "HEAD")
	path := filepath.Join(raw, "claude", "2026-10-07", "11111111-2222-3333-4444-555555555555.jsonl")
	human := recordHuman("ask", "agent-recall 레포에도 올려줘", t0)
	human["sessionId"], human["cwd"] = "11111111-2222-3333-4444-555555555555", harness
	write(t, path, jsonLines(t, human,
		bash("b1", "cp "+filepath.Join(harness, "notes.md")+" "+published+"/", t0.Add(time.Minute)), bashResult("b1", "", t0.Add(time.Minute+time.Second)),
		bash("b2", `cd `+published+` && rtk git add -A && rtk git commit -qm "feat: 기억 개선" && rtk git push -q`, t0.Add(2*time.Minute)), bashResult("b2", "ok\nok\nok", t0.Add(2*time.Minute+5*time.Second)),
	))
	now := t0.Add(3 * time.Hour)
	if _, err := Build(now.AddDate(0, 0, -1), false, now); err != nil {
		t.Fatal(err)
	}
	records, _ := loadRecords()
	if len(records) != 1 {
		t.Fatal("기록이 하나여야 함", len(records))
	}
	text := string(records[0].raw)
	for _, want := range []string{"### 커밋\n- " + hash[:8] + " feat: 기억 개선 (agent-recall main) [tool:b2]", "### push\n- agent-recall origin/main ← " + hash[:8] + " [tool:b2]", "### 레포 사이 복사\n- harness → agent-recall"} {
		if !strings.Contains(text, want) {
			t.Fatalf("기록에 %q 없음\n%s", want, text)
		}
	}
	lines, _ := readFacts()
	slots := map[string]factLine{}
	for _, line := range lines {
		slots[line.Slot] = line
	}
	commit, push := slots["commit/agent-recall/"+hash], slots["push/agent-recall/origin/main"]
	if commit.Value != "feat: 기억 개선" || commit.Evidence.Asked != "agent-recall 레포에도 올려줘" || !strings.HasSuffix(commit.Evidence.Origin, "agent-recall.git") || push.Value != hash || slots["relation/harness/agent-recall"].Value != "복사" {
		t.Fatal("장부에 커밋·push·관계가 틀리게 남음", commit, push, slots)
	}
	// Next conversation, in harness, about to push: where the work went, and what was asked then.
	note := ActionText("git push", harness, "next-session", "")
	for _, want := range []string{"레포 harness", "원격 없음", "복사해 간 레포 agent-recall (대화 1개", "마지막 push: agent-recall origin/main ← " + hash[:8], "그때 사용자 원문: «agent-recall 레포에도 올려줘»"} {
		if !strings.Contains(note, want) {
			t.Fatalf("push 전에 %q 를 말하지 않음\n%s", want, note)
		}
	}
	if again := ActionText("git commit -m x && git push", harness, "next-session", ""); again != "" {
		t.Fatal("같은 대화에 같은 말을 다시 함", again)
	}
	ForgetActions("next-session")
	if again := ActionText("git push", harness, "next-session", ""); again == "" {
		t.Fatal("압축 뒤에 다시 말하지 않음")
	}
	if hint := HintText("agent-recall레포에 올렸던 거 기억 안나?", harness, "other-session", "", now); !strings.Contains(hint, "[레포 agent-recall (") || !strings.Contains(hint, "복사해 온 레포 harness") {
		t.Fatal("레포 이름을 말한 질문에 그 레포 기억을 붙이지 않음", hint)
	}
	// A repository with a remote and no relation says nothing before its push.
	plain := filepath.Join(parent, "plain")
	repoAt(t, plain, true, t0)
	if note := ActionText("git commit -qm x && git push", plain, "third-session", ""); note != "" {
		t.Fatal("특별할 것 없는 레포에 말을 붙임", note)
	}
	if hint := HintText("커밋하고 푸시해", plain, "third-session", "", now); hint != "" {
		t.Fatal("특별할 것 없는 레포의 행동 요청에 말을 붙임", hint)
	}
}

func TestCodexScriptsLongRunsAndHiddenFoldersAreRead(t *testing.T) {
	_, raw := fixture(t)
	parent := t.TempDir()
	api := filepath.Join(parent, "api")
	t0 := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	repoAt(t, api, true, t0.Add(-time.Hour))
	gitAt(t, api, t0.Add(time.Minute+time.Second), "commit", "-q", "--allow-empty", "-m", "fix: 스크립트")
	gitAt(t, api, t0.Add(2*time.Minute+4*time.Second), "commit", "-q", "--allow-empty", "-m", "fix: 오래 걸림")
	long := gitAt(t, api, t0, "rev-parse", "--short=7", "HEAD")
	// Another session's commit, while a command here committed in a folder a variable hid.
	gitAt(t, api, t0.Add(3*time.Minute+time.Second), "commit", "-q", "--allow-empty", "-m", "fix: 다른 세션")
	row := func(kind string, at time.Time, payload map[string]any) map[string]any {
		return map[string]any{"type": kind, "timestamp": at.Format(time.RFC3339), "payload": payload}
	}
	id := "01a0bbbb-0000-7000-8000-00000000c0de"
	path := filepath.Join(raw, "codex", "2026-10-07", "rollout-2026-10-07T11-00-00-"+id+".jsonl")
	script := "const r = await Promise.allSettled([tools.exec_command({cmd:\"git commit -qm 'fix: 스크립트'\",workdir:\"" + api + "\"})]);"
	write(t, path, jsonLines(t,
		row("session_meta", t0, map[string]any{"id": id, "cwd": parent}),
		row("response_item", t0, map[string]any{"type": "message", "role": "user", "id": "u1", "content": []any{map[string]any{"type": "input_text", "text": "커밋해"}}}),
		row("response_item", t0.Add(time.Minute), map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "s1", "input": script}),
		row("response_item", t0.Add(time.Minute+2*time.Second), map[string]any{"type": "custom_tool_call_output", "call_id": "s1", "output": []any{
			map[string]any{"type": "input_text", "text": `{"index":0,"result":{"status":"fulfilled","value":{"exit_code":0,"output":""}}}`}}}),
		row("response_item", t0.Add(2*time.Minute), map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c2", "arguments": `{"cmd":"git commit -m 'fix: 오래 걸림'","workdir":"` + api + `"}`}),
		row("response_item", t0.Add(2*time.Minute+time.Second), map[string]any{"type": "function_call_output", "call_id": "c2", "output": "Process running with session ID 77\nhusky > pre-commit"}),
		row("response_item", t0.Add(2*time.Minute+5*time.Second), map[string]any{"type": "function_call", "name": "write_stdin", "call_id": "c3", "arguments": `{"session_id":77,"chars":""}`}),
		row("response_item", t0.Add(2*time.Minute+6*time.Second), map[string]any{"type": "function_call_output", "call_id": "c3", "output": "Process exited with code 0\nOutput:\n[main " + long + "] fix: 오래 걸림"}),
		row("response_item", t0.Add(3*time.Minute), map[string]any{"type": "function_call", "name": "exec_command", "call_id": "c4", "arguments": `{"cmd":"W=$(mktemp -d); cd $W && git init -q && git commit -qm 'fix: 다른 세션' --allow-empty","workdir":"` + api + `"}`}),
		row("response_item", t0.Add(3*time.Minute+2*time.Second), map[string]any{"type": "function_call_output", "call_id": "c4", "output": "Process exited with code 0\nOutput:\n"}),
	))
	s, ok := readSession("codex", path)
	if !ok || len(s.commands) != 3 || s.commands[1].result != "성공" || !strings.Contains(s.commands[1].output, "husky") || !strings.Contains(s.commands[1].output, "fix: 오래 걸림") {
		t.Fatal("스크립트 명령이나 write_stdin 으로 끝난 명령을 읽지 못함", ok, s.commands)
	}
	newGitCache().readGit(&s)
	var subjects []string
	for _, e := range s.events {
		subjects = append(subjects, e.subject)
	}
	if strings.Join(subjects, ",") != "fix: 스크립트,fix: 오래 걸림" {
		t.Fatal("스크립트·긴 실행 커밋을 놓쳤거나, 변수로 가린 폴더의 커밋을 이 레포 커밋으로 봄", subjects)
	}
}

func TestSubAgentsPullRequestLinksAndShellWritesAreRecorded(t *testing.T) {
	_, raw := fixture(t)
	id := "22222222-3333-4444-5555-666666666666"
	path := filepath.Join(raw, "claude", "2026-10-07", id+".jsonl")
	t0 := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	human := recordHuman("ask", "PR 올려줘", t0)
	human["sessionId"], human["cwd"] = id, "/work/web"
	write(t, path, jsonLines(t, human,
		bash("w1", "cat > /work/web/notes.md <<'EOF'\n본문 > 안 씀.md\nEOF", t0.Add(time.Minute)),
		map[string]any{"type": "pr-link", "prNumber": 377, "prUrl": "https://github.com/me/web/pull/377", "prRepository": "me/web", "timestamp": t0.Add(2 * time.Minute).Format(time.RFC3339)},
	))
	write(t, filepath.Join(raw, "claude", "2026-10-07", id, "subagents", "agent-a1.jsonl"), jsonLines(t,
		map[string]any{"type": "assistant", "isSidechain": true, "timestamp": t0.Add(time.Minute).Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "e1", "name": "Edit", "input": map[string]any{"file_path": "/work/web/src/a.ts"}}}}},
		map[string]any{"type": "assistant", "isSidechain": true, "timestamp": t0.Add(time.Minute).Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "e2", "name": "NotebookEdit", "input": map[string]any{"notebook_path": "/work/web/b.ipynb"}}}}},
	)+`{"type":"assistant","message":`) // the agent still writing its last line
	s, ok := readSession("claude", path)
	if !ok || strings.Join(s.files, ",") != "notes.md,src/a.ts,b.ipynb" || len(s.prLinks) != 1 {
		t.Fatal("셸로 쓴 파일·서브에이전트 편집·PR 링크를 놓침", ok, s.files, s.prLinks)
	}
	now := t0.Add(time.Hour)
	if _, err := Build(now.AddDate(0, 0, -1), false, now); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"#377", "PR377", "pr #377"} {
		if hits := Find(query, 3); len(hits) != 1 || !strings.Contains(hits[0].text, "PR #377 https://github.com/me/web/pull/377 (대화에 연결된 PR)") {
			t.Fatal(query+" 로 PR 을 연 대화를 찾지 못함", hits)
		}
	}
	lines, _ := readFacts()
	if len(lines) != 1 || lines[0].Slot != "pr/me/web/377" || lines[0].Value != "만듦" {
		t.Fatal("대화에 연결된 PR 을 장부에 남기지 못함", lines)
	}
}

func TestALongOutputKeepsWhatGitMade(t *testing.T) {
	output := strings.Repeat("husky 검사 줄\n", 600) + "[main 1a2b3c4] fix: 끝\n 1 file changed\nTo github.com:me/web.git\n   0a0a0a0..1a2b3c4  main -> main\n"
	kept := keepOutput(output)
	if len([]rune(kept)) > 4300 || !strings.Contains(kept, "[main 1a2b3c4] fix: 끝") || !strings.Contains(kept, "0a0a0a0..1a2b3c4  main -> main") {
		t.Fatal("긴 출력 뒤의 커밋·push 줄을 잃음", len([]rune(kept)), kept[len(kept)-200:])
	}
}

func TestAWorktreeIsItsRepository(t *testing.T) {
	fixture(t)
	parent := t.TempDir()
	web := filepath.Join(parent, "web")
	repoAt(t, web, true, verifyAt)
	wt := filepath.Join(parent, "web-fix")
	gitAt(t, web, verifyAt, "worktree", "add", "-q", wt)
	os.MkdirAll(filepath.Join(wt, "src"), 0o700)
	if RepoName(wt) != "web" || RepoName(filepath.Join(wt, "src")) != "web" || mainRoot("") != "" {
		t.Fatal("worktree 를 폴더 이름으로 부름", RepoName(wt))
	}
}

func TestRebuildKeepsDecisionsAndUnreadableConversations(t *testing.T) {
	_, _, path := recordFixture(t)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if _, err := Build(now.AddDate(0, 0, -14), false, now); err != nil {
		t.Fatal(err)
	}
	decided := claim("choice/gone/q", "가", "user", "repo", factEvidence{Session: "gone-session"}, now)
	lost := claim("commit/old/abc", "fix: 원문 없음", "observed", "old", factEvidence{Session: "gone-session"}, now)
	decision := factLine{Kind: "decision", ID: "d", Fact: decided.Fact, Verdict: "확인", ObservedAt: now.Format(time.RFC3339)}
	if err := appendFacts(map[string]bool{}, []factLine{decided, lost, decision}, now); err != nil {
		t.Fatal(err)
	}
	before, _ := readFacts()
	out, err := BackfillFacts(now, true)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := readFacts()
	if _, err := os.Stat(out["backup"].(string)); err != nil || len(after) != len(before) {
		t.Fatal("옛 장부를 남기지 않았거나 줄 수가 달라짐", out, len(before), len(after))
	}
	kept := map[string]bool{}
	for _, line := range after {
		kept[line.ID] = true
	}
	if !kept[decided.ID] || !kept[lost.ID] || !kept[decision.ID] {
		t.Fatal("사용자 결정이나 원문을 못 읽는 대화의 사실을 잃음", after)
	}
	_ = path
}

func TestFindReadsNamesWrittenAgainstKoreanAndUnderscores(t *testing.T) {
	recordFixture(t)
	write(t, filepath.Join(RecordDir(), "2026-10-07", "cccccccc.md"), "---\nsession: cccccccc-0000-4000-8000-000000000000\nrepo: harness\ndate: 2026-10-07\n---\n\n# agent-recall 레포에 올려줘\n\n## 내용\n\n### 요청\n- 10-07 10:00 [req-c 줄3 claude] agent-recall 레포에 올려줘\n  → AI(주장): harness-recall 폴더를 옮겼습니다\n")
	for _, query := range []string{"agent-recall레포", "harness_recall", "Agent-Recall"} {
		if out := FindText(query, 5, FindFilter{}); !strings.Contains(out, "agent-recall 레포에 올려줘") {
			t.Fatal(query+" 로 찾지 못함", out)
		}
	}
}

func TestSourceOpensAFullIDsNewestFileOrTheConversationOfARequest(t *testing.T) {
	_, raw := fixture(t)
	id := "01a0cccc-0000-7000-8000-00000000beef"
	t0 := time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC)
	rollout := func(path, text string, at time.Time) {
		write(t, path, jsonLines(t,
			map[string]any{"type": "session_meta", "timestamp": at.Format(time.RFC3339), "payload": map[string]any{"id": id, "cwd": "/work/repo"}},
			map[string]any{"type": "response_item", "timestamp": at.Format(time.RFC3339), "payload": map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}},
		))
	}
	rollout(filepath.Join(raw, "codex", "2026-10-07", "rollout-2026-10-07T11-00-00-"+id+".jsonl"), "처음 연 날", t0)
	resumed := filepath.Join(raw, "codex", "2026-10-08", "rollout-2026-10-08T09-00-00-"+id+".jsonl")
	rollout(resumed, "다음 날 이어서", t0.Add(24*time.Hour))
	if out, err := Source(id, ""); err != nil || !strings.HasPrefix(out, resumed+"\n") || !strings.Contains(out, "다음 날 이어서") {
		t.Fatal("이어 연 대화의 최신 파일을 고르지 못함", out, err)
	}
	if _, err := Source("01a0cccc", ""); err == nil {
		t.Fatal("짧은 ID 로 두 파일 중 하나를 임의로 고름")
	}
	path := filepath.Join(raw, "claude", "2026-10-07", "44444444-5555-6666-7777-888888888888.jsonl")
	first, later := recordHuman("req-first-0001", "처음 요청", t0), recordHuman("req-later-0002", "나중 요청", t0.Add(time.Hour))
	for _, row := range []map[string]any{first, later} {
		row["sessionId"], row["cwd"] = "44444444-5555-6666-7777-888888888888", "/work/repo"
	}
	write(t, path, jsonLines(t, first, said(t0.Add(time.Minute).Format(time.RFC3339), "처음 답", false), later, said(t0.Add(time.Hour+time.Minute).Format(time.RFC3339), "나중 답", false)))
	now := t0.Add(2 * time.Hour)
	if _, err := Build(now.AddDate(0, 0, -1), false, now); err != nil {
		t.Fatal(err)
	}
	out, err := SourcePage("req-later-0002", "", "", 0, 20)
	if err != nil || !strings.HasPrefix(out, path+"\n") || !strings.Contains(out, "나중 요청") || strings.Contains(out, "처음 요청") {
		t.Fatal("요청 ID 로 그 요청부터 원문을 보이지 못함", out, err)
	}
}

func TestResumeShowsAnotherFoldersConversationThatCommittedHere(t *testing.T) {
	fixture(t)
	parent := t.TempDir()
	web, notes := filepath.Join(parent, "web"), filepath.Join(parent, "notes")
	repoAt(t, web, false, verifyAt)
	os.MkdirAll(notes, 0o700)
	web, _ = filepath.EvalSymlinks(web)
	texts := []string{"web 레포에 커밋하고 푸시해", "다음 일 하나", "다음 일 둘", "다음 일 셋", "다음 일 넷째를 마저 해줘", "다음 일 넷째를 마저 해줘!!"}
	s := session{tool: "claude", id: "elsewhere-session", path: filepath.Join(t.TempDir(), "raw.jsonl"), cwd: notes, start: verifyAt, end: verifyAt.Add(time.Hour), summaryState: "pending",
		events: []gitEvent{{kind: "commit", root: web, repo: "web", hash: "abcdef1234567890", subject: "fix: 다른 폴더에서", ref: "main", at: verifyAt, source: "tool:b1"}}}
	var rows []map[string]any
	for i, text := range texts {
		id := []string{"act", "r2", "r3", "r4", "r5", "r6"}[i]
		s.requests = append(s.requests, worklog.Request{Text: text, ID: id, Line: i + 1, Source: "claude:typed"})
		row := resumeHuman(id, text)
		row["cwd"] = notes
		rows = append(rows, row)
	}
	write(t, s.path, jsonLines(t, rows...))
	info, _ := os.Stat(s.path)
	s.size = info.Size()
	write(t, filepath.Join(RecordDir(), "2026-10-07", "1000-notes-elsewhere.md"), render(s, summary{Title: "다른 폴더 작업", Todo: []string{"남은 일"}}, true))
	card, err := ResumeText(web, "")
	for _, want := range []string{"레포 web (" + web + "): 원격 없음", "- 목표/제목: 다른 폴더 작업 (다른 폴더 " + notes + " 의 대화가 이 레포를 고침)", "  앞선 행동 요청: [act 줄1] web 레포에 커밋하고 푸시해"} {
		if err != nil || !strings.Contains(card, want) {
			t.Fatalf("카드에 %q 없음\n%s %v", want, card, err)
		}
	}
	if strings.Contains(card, "[r5 줄5]") || !strings.Contains(card, "[r6 줄6]") {
		t.Fatal("다시 보낸 같은 요청을 두 번 보임", card)
	}
	if card, _ := ResumeText(notes, ""); !strings.Contains(card, "다른 폴더 작업") || strings.Contains(card, "다른 폴더 "+notes) {
		t.Fatal("자기 폴더의 대화를 다른 폴더 대화로 보임", card)
	}
}

func TestEarlierActionSkipsChoicesAndTheShownRequests(t *testing.T) {
	requests := []worklog.Request{{Text: "푸시해", ID: "a"}, {Text: "커밋할까요? → 예", ID: "q", QuestionID: "ask"}, {Text: "다음"}, {Text: "커밋해"}}
	if r, ok := earlierAction(requests, 2); !ok || r.ID != "a" {
		t.Fatal("질문창 답이나 이미 보인 요청을 앞선 행동 요청으로 고름", r)
	}
	if _, ok := earlierAction(requests[2:], 2); ok {
		t.Fatal("보인 요청뿐인데 앞선 요청을 만듦")
	}
	if squash("다음 일 넷을 해줘!!") != squash("다음 일, 넷을 해줘") {
		t.Fatal("구두점만 다른 말을 다르게 봄")
	}
}

func TestBuildConversationRecordsOnlyTheOneArchived(t *testing.T) {
	raw, _, path := recordFixture(t)
	row := recordHuman("o", "다른 대화", time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC))
	row["sessionId"], row["cwd"] = "fedcba98-0000-0000-0000-000000000000", "/work/repo"
	write(t, filepath.Join(raw, "claude", "2026-10-07", "fedcba98-0000-0000-0000-000000000000.jsonl"), jsonLines(t, row))
	out, err := BuildConversation(path, time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC))
	records, _ := loadRecords()
	if err != nil || len(records) != 1 || records[0].field("raw") != path {
		t.Fatal("끝난 대화 하나만 기록하지 않음", out, err, len(records))
	}
}

func TestARecordOfWhatIsNoLongerAConversationIsRetired(t *testing.T) {
	_, raw := fixture(t)
	path := filepath.Join(raw, "claude", "2026-10-07", "55555555-6666-7777-8888-999999999999.jsonl")
	write(t, path, jsonLines(t, map[string]any{"type": "user", "uuid": "c", "version": "2.1.100", "cwd": "/work/repo", "timestamp": "2026-10-07T01:00:00Z",
		"message": map[string]any{"content": "<command-name>/clear</command-name>\n<command-message>clear</command-message>\n<command-args></command-args>"}}))
	record := func(name, extra string) string {
		p := filepath.Join(RecordDir(), "2026-10-07", name)
		write(t, p, "---\nsession: 55555555-6666-7777-8888-999999999999\nraw: "+path+"\nformat: 9\n"+extra+"---\n\n# /clear\n")
		return p
	}
	stale := record("1000-repo-55555555.md", "")
	now := time.Date(2026, 10, 7, 3, 0, 0, 0, time.UTC)
	out, err := Build(now.AddDate(0, 0, -1), false, now)
	if _, statErr := os.Stat(stale); err != nil || !os.IsNotExist(statErr) || len(out["retired"].([]string)) != 1 {
		t.Fatal("대화가 아니게 된 원문의 옛 기록을 남김", out, err)
	}
	summarized := record("1000-repo-55555555.md", "summary_data: {\"title\": \"모델 요약\"}\n")
	os.Remove(filepath.Join(Dir(), "not-sessions-10.json"))
	out, err = Build(now.AddDate(0, 0, -1), false, now)
	if _, statErr := os.Stat(summarized); err != nil || statErr != nil || len(out["failed"].([]string)) != 0 {
		t.Fatal("모델 요약이 든 기록까지 지움", out, err, statErr)
	}
}

// A branch deleted after its work was merged takes its reflog with it; the commit is still found by
// its words and time, and a quiet command that committed nothing stays nothing.
func TestAQuietCommitIsFoundAfterItsBranchAndWorktreeAreGone(t *testing.T) {
	_, raw := fixture(t)
	parent := t.TempDir()
	web, wt := filepath.Join(parent, "web"), filepath.Join(parent, "web-topic")
	t0 := time.Date(2026, 8, 1, 1, 0, 0, 0, time.UTC)
	repoAt(t, web, false, t0.Add(-time.Hour))
	gitAt(t, web, t0, "worktree", "add", "-q", "-b", "topic", wt)
	gitAt(t, wt, t0.Add(time.Minute+time.Second), "commit", "-q", "--allow-empty", "-m", "feat: 조용한 커밋")
	gitAt(t, web, t0.Add(2*time.Hour), "merge", "-q", "--ff-only", "topic")
	gitAt(t, web, t0.Add(2*time.Hour), "worktree", "remove", wt)
	gitAt(t, web, t0.Add(2*time.Hour), "branch", "-D", "topic")
	hash := gitAt(t, web, t0, "rev-parse", "HEAD")
	// In a repository whose reflog is older than the command, a branch deleted with its reflog.
	api := filepath.Join(parent, "api")
	repoAt(t, api, false, t0.Add(-time.Hour))
	gitAt(t, api, t0, "checkout", "-q", "-b", "topic")
	gitAt(t, api, t0.Add(5*time.Minute+time.Second), "commit", "-q", "--allow-empty", "-m", "feat: 지운 가지")
	gitAt(t, api, t0.Add(2*time.Hour), "checkout", "-q", "main")
	gitAt(t, api, t0.Add(2*time.Hour), "merge", "-q", "--ff-only", "topic")
	gitAt(t, api, t0.Add(2*time.Hour), "branch", "-D", "topic")
	gitAt(t, api, t0.Add(2*time.Hour), "reflog", "expire", "--expire=now", "HEAD")
	merged := gitAt(t, api, t0, "rev-parse", "HEAD")
	path := filepath.Join(raw, "claude", "2026-08-01", "66666666-7777-8888-9999-000000000000.jsonl")
	human := recordHuman("ask", "커밋해", t0)
	human["sessionId"], human["cwd"] = "66666666-7777-8888-9999-000000000000", web
	write(t, path, jsonLines(t, human,
		bash("q1", "cd "+wt+" && git commit -qm 'feat: 조용한 커밋'", t0.Add(time.Minute)), bashResult("q1", "", t0.Add(time.Minute+2*time.Second)),
		bash("q2", "git commit -qm 'fix: 없던 커밋' | tail -1", t0.Add(3*time.Minute)), bashResult("q2", "", t0.Add(3*time.Minute+2*time.Second)),
		bash("q3", "cd "+api+" && git commit -qm 'feat: 지운 가지' 2>&1 | tail -1", t0.Add(5*time.Minute)), bashResult("q3", "", t0.Add(5*time.Minute+2*time.Second)),
	))
	s, _ := readSession("claude", path)
	newGitCache().readGit(&s)
	if len(s.events) != 2 || s.events[0].hash != hash || RepoName(s.events[0].root) != "web" || s.events[1].hash != merged || RepoName(s.events[1].root) != "api" {
		t.Fatal("지운 브랜치의 커밋을 git 에서 찾지 못했거나, 하지 않은 커밋을 만듦", s.events)
	}
}

func TestScriptCommandsReadBareAndQuotedKeys(t *testing.T) {
	script := `const a = await tools.exec_command({"cmd":"git commit -m \"docs :: 정비\"","workdir":"/w/docs"});
const b = await tools.exec_command({ cmd: 'git push', workdir: '/w/api' });
await tools.view_image({path: "x"});`
	got := scriptCommands(script)
	if len(got) != 3 || got[0].cmd != `git commit -m "docs :: 정비"` || got[0].workdir != "/w/docs" || got[1].cmd != "git push" || got[1].workdir != "/w/api" || got[2].cmd != "" {
		t.Fatalf("스크립트 명령을 잘못 읽음 %+v", got)
	}
}

func TestCopyStepsSkipTheValuesOfRsyncOptions(t *testing.T) {
	steps := shellSteps("/work/repo", `P=/tmp/port; R=/w/agent-recall; rsync -a --exclude .git --exclude 'README*' --delete $P/ $R/`)
	if len(steps) != 1 || steps[0].kind != "copy" || strings.Join(steps[0].from, ",") != "/tmp/port" || steps[0].to != "/w/agent-recall" {
		t.Fatalf("옵션 값을 복사 경로로 읽음 %+v", steps)
	}
}

// The copy that really happened went through a folder git does not track: an archive of the
// repository unpacked in a scratch folder, then copied into the published one.
func TestACopyThroughAStagingFolderRelatesTheRepositoryItCarried(t *testing.T) {
	_, raw := fixture(t)
	parent := t.TempDir()
	web, harness, published, stage := filepath.Join(parent, "web"), filepath.Join(parent, "harness"), filepath.Join(parent, "agent-recall"), filepath.Join(parent, "stage")
	t0 := time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)
	repoAt(t, web, true, t0.Add(-time.Hour))
	repoAt(t, harness, false, t0.Add(-time.Hour))
	repoAt(t, published, true, t0.Add(-time.Hour))
	for _, name := range []string{"main.go", "a.go", "b.go", "c.go", "d.go"} {
		write(t, filepath.Join(harness, name), "package main\n")
		write(t, filepath.Join(published, name), "package main\n")
	}
	write(t, filepath.Join(web, "page.ts"), "x\n")
	for _, dir := range []string{harness, published, web} {
		gitAt(t, dir, t0.Add(-time.Minute), "add", "-A")
		gitAt(t, dir, t0.Add(-time.Minute), "commit", "-q", "-m", "files")
	}
	write(t, filepath.Join(published, "main.go"), "package main\n\nfunc main() {}\n")
	gitAt(t, published, t0.Add(3*time.Minute), "commit", "-q", "-am", "feat: 옮김")
	gitAt(t, published, t0.Add(3*time.Minute+time.Second), "push", "-q")
	path := filepath.Join(raw, "claude", "2026-10-07", "22222222-2222-3333-4444-555555555555.jsonl")
	human := recordHuman("ask", "공개 레포에도 옮겨 줘", t0)
	human["sessionId"], human["cwd"] = "22222222-2222-3333-4444-555555555555", web
	edit := func(id, file string, at time.Time) map[string]any {
		return map[string]any{"type": "assistant", "timestamp": at.Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Edit", "input": map[string]any{"file_path": file}}}}}
	}
	write(t, path, jsonLines(t, human, edit("e1", filepath.Join(harness, "main.go"), t0.Add(time.Minute)), edit("e2", filepath.Join(published, "README.md"), t0.Add(time.Minute)),
		bash("b1", "cd "+harness+" && git archive HEAD | tar -x -C "+stage, t0.Add(2*time.Minute)), bashResult("b1", "", t0.Add(2*time.Minute+time.Second)),
		bash("b2", "rsync -a --exclude .git "+stage+"/ "+published+"/ && cd "+published+` && git commit -qam "feat: 옮김" && git push -q`, t0.Add(3*time.Minute)), bashResult("b2", "", t0.Add(3*time.Minute+5*time.Second)),
		// Filling a repository from a folder git does not track, without pushing it, relates nothing:
		// here a copy goes back into harness, and an unrelated file into web.
		bash("b3", "cp -R "+stage+"/. "+harness+"/ && cp "+filepath.Join(parent, "logo.png")+" "+web+"/", t0.Add(4*time.Minute)), bashResult("b3", "", t0.Add(4*time.Minute+time.Second)),
	))
	now := t0.Add(3 * time.Hour)
	if _, err := Build(now.AddDate(0, 0, -1), false, now); err != nil {
		t.Fatal(err)
	}
	lines, _ := readFacts()
	var relations []string
	for _, line := range lines {
		if strings.HasPrefix(line.Slot, "relation/") {
			relations = append(relations, line.Slot)
		}
	}
	if strings.Join(relations, ",") != "relation/harness/agent-recall" {
		t.Fatal("거쳐 간 복사의 관계가 틀림", relations)
	}
	if note := ActionText("git push", harness, "next-session", ""); !strings.Contains(note, "복사해 간 레포 agent-recall") {
		t.Fatal("push 전에 옮겨 간 레포를 말하지 않음", note)
	}
}

func TestNamedReposNeedTheWholeName(t *testing.T) {
	folded := []factState{{Slot: "push/agent-recall/origin/main"}, {Slot: "commit/harness/1a2b3c4d"}}
	for prompt, want := range map[string]string{
		"agent-recall레포에 올렸던 거 기억 안나?":  "agent-recall",
		"Agent-Recall 에 올려":             "agent-recall",
		"agent-recall-v2 만들자":           "",
		"harness_old 정리하고 (harness) 커밋": "harness",
	} {
		if got := strings.Join(namedRepos(prompt, folded), ","); got != want {
			t.Errorf("%q 에서 %q, 기대 %q", prompt, got, want)
		}
	}
}
