package worklog

import (
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	repo, vault, daily string
}

func setup(t *testing.T, withGit bool) fixture {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HARNESS_COMPANY_ORGS", "acme")
	f := fixture{repo: filepath.Join(base, "fixrepo"), vault: filepath.Join(base, "vault"), daily: filepath.Join(base, "daily")}
	for _, dir := range []string{f.repo, f.vault, f.daily} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("HARNESS_VAULT_DIR", f.vault)
	t.Setenv("HARNESS_DAILY_DIR", f.daily)
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "400000")
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	os.MkdirAll(filepath.Join(base, "tmp"), 0o755)
	if withGit {
		gitRun(t, f.repo, nil, "init", "-q")
		os.WriteFile(filepath.Join(f.repo, "a.txt"), []byte("a"), 0o644)
		gitRun(t, f.repo, nil, "add", "a.txt")
		old := []string{"GIT_COMMITTER_DATE=2026-01-01T00:00:00", "GIT_AUTHOR_DATE=2026-01-01T00:00:00"}
		gitRun(t, f.repo, old, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "first")
	}
	return f
}

func gitRun(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func writeNote(t *testing.T, f fixture, rel, body string, mod time.Time) {
	t.Helper()
	path := filepath.Join(f.vault, rel)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, mod, mod)
}

func savedNote(t *testing.T, f fixture, mod time.Time) {
	writeNote(t, f, "personal/10_프로젝트/P/작업 기록/일.md", "---\nstatus: 진행\nrepos: fixrepo\n---\n## 다음\n1. 계속\n", mod)
}

func writeLines(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	var b strings.Builder
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// claudeSession mirrors the old mk_session fixture: count typed requests starting minutes ago.
func claudeSession(t *testing.T, path string, count, minutes, context int, reply string) {
	start := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute)
	var rows []map[string]any
	for i := 0; i < count; i++ {
		ts := start.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		rows = append(rows,
			map[string]any{"type": "user", "promptSource": "typed", "timestamp": ts,
				"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("%d번째로 부탁하는 작업 요청입니다 %s", i+1, strings.Repeat("가", 200))}}}},
			map[string]any{"type": "assistant", "timestamp": ts,
				"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": reply}}, "usage": map[string]any{"input_tokens": context}}})
	}
	writeLines(t, path, rows)
}

func codexSession(t *testing.T, path, cwd string, count, minutes, context, window int) {
	start := time.Now().UTC().Add(-time.Duration(minutes) * time.Minute)
	ts := start.Format(time.RFC3339Nano)
	rows := []map[string]any{
		{"type": "session_meta", "timestamp": ts, "payload": map[string]any{"cwd": cwd}},
		{"type": "response_item", "timestamp": ts, "payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "<environment_context> 주입된 맥락입니다 </environment_context>"}}}},
		{"type": "response_item", "timestamp": ts, "payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "# AGENTS.md instructions for 레포"}}}},
	}
	for i := 0; i < count; i++ {
		at := start.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		rows = append(rows, map[string]any{"type": "response_item", "timestamp": at, "payload": map[string]any{"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": fmt.Sprintf("%d번째 코덱스 작업 요청입니다", i+1)}}}})
	}
	rows = append(rows, map[string]any{"type": "event_msg", "timestamp": ts, "payload": map[string]any{"type": "token_count",
		"info": map[string]any{"last_token_usage": map[string]any{"input_tokens": context}, "model_context_window": window}}})
	writeLines(t, path, rows)
}

// appendNoteWrite adds a Write tool call on a work note, timestamped now.
func appendNoteWrite(t *testing.T, path string) {
	raw, _ := os.ReadFile(path)
	row, _ := json.Marshal(map[string]any{"type": "assistant", "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "name": "Write",
			"input": map[string]any{"file_path": "/v/personal/10_프로젝트/P/작업 기록/일.md"}}}}})
	os.WriteFile(path, append(append(raw, row...), '\n'), 0o644)
}

func TestSaveIsJudgedPerSession(t *testing.T) {
	f := setup(t, true)
	tr := filepath.Join(f.repo, ".ps.jsonl")
	savedNote(t, f, time.Now())
	claudeSession(t, tr, 3, 40, 1000, "중간 말")
	if got := readTranscript(tr); !got.saved.IsZero() {
		t.Fatal("다른 세션의 노트를 저장 근거로 사용", got.saved)
	}
	appendNoteWrite(t, tr)
	if got := readTranscript(tr); got.saved.IsZero() {
		t.Fatal("이 세션의 노트 저장을 놓침")
	}
}

func TestCodexMarkdownRequestKept(t *testing.T) {
	f := setup(t, false)
	tr := filepath.Join(f.repo, ".cx.jsonl")
	codexSession(t, tr, f.repo, 1, 1, 1000, 100000)
	raw, _ := os.ReadFile(tr)
	row, _ := json.Marshal(map[string]any{"type": "response_item", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "payload": map[string]any{"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": "# 작업 요청\n로그인 고쳐"}}}})
	os.WriteFile(tr, append(append(raw, row...), '\n'), 0o644)
	got := readTranscript(tr)
	if n := len(got.requests); n != 2 || !strings.HasPrefix(got.requests[1].Text, "# 작업 요청") {
		t.Fatalf("Markdown 요청이 빠지거나 주입 맥락이 들어감: %+v", got.requests)
	}
}

func TestStartOutsideProject(t *testing.T) {
	f := setup(t, false)
	tr := filepath.Join(f.repo, ".hs.jsonl")
	claudeSession(t, tr, 5, 60, 390000, "중간 말")
	if card := Start("claude", memory.Object{"cwd": f.repo, "source": "startup"}); card != "" {
		t.Fatalf("레포도 노트도 없는 폴더에 카드를 냄: %s", card)
	}

	writeNote(t, f, "personal/10_프로젝트/Claude 설정/작업 기록/하네스.md", "---\nstatus: 진행\nrepos: claude-harness, fixrepo(Go)\n---\n## 다음\n1. 턴 끝 검사\n", time.Now())
	card := Start("claude", memory.Object{"cwd": f.repo, "source": "startup"})
	if !strings.Contains(card, "하네스 — 다음: 턴 끝 검사") || strings.Contains(card, "=== git") {
		t.Fatalf("레포 아닌 폴더라도 노트가 있으면 카드를 내야 함: %s", card)
	}
}

func TestCodex(t *testing.T) {
	f := setup(t, true)
	sessions := filepath.Join(f.repo, ".codex", "sessions")
	tr := filepath.Join(sessions, "2026", "10", "06", "rollout-now.jsonl")
	savedNote(t, f, time.Now())
	codexSession(t, tr, f.repo, 1, 1, 190000, 258400)
	other := filepath.Join(sessions, "2026", "10", "05", "rollout-other.jsonl")
	codexSession(t, other, "/elsewhere", 2, 90, 1000, 258400)
	prev := filepath.Join(sessions, "2026", "10", "05", "rollout-prev.jsonl")
	codexSession(t, prev, f.repo, 2, 60, 1000, 258400)
	os.Chtimes(other, time.Now(), time.Now())
	if got := previousTranscript("codex", tr, f.repo); got != prev {
		t.Fatalf("Codex 직전 세션은 같은 폴더의 기록이어야 함: %s", got)
	}
	card := Start("codex", memory.Object{"cwd": f.repo, "source": "startup", "transcript_path": tr})
	if !strings.Contains(card, "이전 요청") || strings.Contains(card, "주입된 맥락") {
		t.Fatalf("Codex 새 세션 카드: %s", card)
	}
}

func TestDigest(t *testing.T) {
	f := setup(t, true)
	tr := filepath.Join(f.repo, ".tr", "dg.jsonl")
	savedNote(t, f, time.Now())
	claudeSession(t, tr, 45, 60, 1000, "중간 말")
	card := Start("claude", memory.Object{"cwd": f.repo, "source": "compact", "transcript_path": tr})
	if !strings.HasPrefix(card, compactLine+"\n") || !strings.Contains(card, "최근 30개") || !strings.Contains(card, "45번째") || strings.Contains(card, "  15번째") || len([]rune(card)) > maxTotal {
		t.Fatalf("DG1 압축 뒤 요약과 상한: %d자\n%s", len([]rune(card)), card)
	}
	for _, line := range strings.Split(card, "\n") {
		if strings.Contains(line, "번째로 부탁") && len([]rune(line)) > 140 {
			t.Fatalf("DG1 요청 한 줄이 길다: %s", line)
		}
	}

	if card := Start("claude", memory.Object{"cwd": f.repo, "source": "clear", "transcript_path": filepath.Join(f.repo, ".tr", "new.jsonl")}); !strings.Contains(card, "이전 요청") {
		t.Fatalf("DG3 직전 세션이 마지막 요청 뒤 노트를 안 썼으면 요약해야 함")
	}
	appendNoteWrite(t, tr)
	if card := Start("claude", memory.Object{"cwd": f.repo, "source": "startup", "transcript_path": filepath.Join(f.repo, ".tr", "new.jsonl")}); strings.Contains(card, "이전 요청") {
		t.Fatalf("DG2 직전 세션이 마지막 요청 뒤 노트를 썼으면 요약이 없어야 함")
	}

	claudeSession(t, tr, 2, 5, 1000, strings.Repeat("보고", 120))
	raw, _ := os.ReadFile(tr)
	os.WriteFile(tr, append(raw, []byte(`{"type":"user","promptSource":"typed","timestamp":"2026-01-01T00:00:00Z","message":{"content":"커밋해"}}`+"\n")...), 0o644)
	card = Start("claude", memory.Object{"cwd": f.repo, "source": "compact", "transcript_path": tr})
	if !strings.Contains(card, "커밋해") || !strings.Contains(card, "마지막 보고 2개") {
		t.Fatalf("DG4 짧은 요청·보고 포함: %s", card)
	}

	os.MkdirAll(filepath.Join(f.daily, "2099-01"), 0o755)
	os.WriteFile(filepath.Join(f.daily, "2099-01", time.Now().Format("2006-01-02")+".md"), []byte("- [ ] TSK-1 남은 일\n- [ ] \n- [x] 끝난 일\n"), 0o644)
	if card := Start("claude", memory.Object{"cwd": f.repo, "source": "resume"}); strings.Contains(card, "TSK-1") || !strings.Contains(card, "작업 구분: 개인") {
		t.Fatalf("DG5 개인 레포는 데일리를 넣지 않음: %s", card)
	}
	gitRun(t, f.repo, nil, "remote", "add", "origin", "https://github.com/acme/x.git")
	card = Start("claude", memory.Object{"cwd": f.repo, "source": "resume"})
	if !strings.Contains(card, "TSK-1 남은 일") || strings.Contains(card, "끝난 일") || strings.Count(card, "[ ]") != 1 || !strings.Contains(card, "작업 구분: 회사") {
		t.Fatalf("DG6 회사 레포 데일리 미완료: %s", card)
	}
}

func TestNotes(t *testing.T) {
	f := setup(t, true)
	gitRun(t, f.repo, nil, "remote", "add", "origin", "https://github.com/acme/renamed_FE.git")
	dir := "company/30_자료/작업 기록/제품/"
	now := time.Now()
	writeNote(t, f, dir+"TSK-1 진행 작업.md", "---\nstatus: 진행\nticket: TSK-1\nrepos: renamed_FE\n---\n## 목표\n고친다\n## 다음\n\n1. 커밋 지시 대기\n2. 화면 확인\n", now)
	writeNote(t, f, dir+"보류 작업.md", "---\nstatus: 보류\nrepos:\n  - other\n  - fixrepo\n---\n## 다음\n- 나중에\n", now.Add(-time.Minute))
	writeNote(t, f, dir+"끝난 작업.md", "---\nstatus: 끝\nrepos: fixrepo\n---\n## 다음\n- 없음\n", now)
	writeNote(t, f, dir+"_이전 기록.md", "---\nstatus: 진행\nrepos: fixrepo\n---\n## 다음\n- 옛 기록\n", now)
	writeNote(t, f, dir+"남의 작업.md", "---\nstatus: 진행\nrepos: elsewhere\n---\n## 다음\n- 남의 일\n", now)
	card := Start("claude", memory.Object{"cwd": f.repo, "source": "resume"})
	for _, want := range []string{"진행 중 작업 — company/30_자료/작업 기록/제품", "TSK-1 진행 작업 — 다음: 커밋 지시 대기", "보류 작업 (보류)"} {
		if !strings.Contains(card, want) {
			t.Fatalf("DG7 %q 없음: %s", want, card)
		}
	}
	for _, unwanted := range []string{"끝난 작업", "옛 기록", "화면 확인", "남의 일"} {
		if strings.Contains(card, unwanted) {
			t.Fatalf("DG7 %q 이 들어감: %s", unwanted, card)
		}
	}

	g := setup(t, true)
	os.MkdirAll(filepath.Join(g.vault, "company", "30_자료", "작업 기록", "예시 제품"), 0o755)
	gitRun(t, g.repo, nil, "remote", "add", "origin", "https://github.com/acme/new.git")
	if card := Start("claude", memory.Object{"cwd": g.repo, "source": "startup"}); !strings.Contains(card, "지금 제품 폴더: 예시 제품") {
		t.Fatalf("노트 없는 회사 레포는 제품 폴더를 보여 줘야 함: %s", card)
	}
}

func TestStartSeparatesCompanyAndPersonalNotesAndKeepsTwoRecentCandidates(t *testing.T) {
	f := setup(t, true)
	now := time.Now()
	for i := 0; i < 5; i++ {
		writeNote(t, f, fmt.Sprintf("personal/10_프로젝트/P/작업 기록/개인%d.md", i), "---\nstatus: 진행\nrepos: fixrepo\n---\n## 다음\n- 개인 작업\n", now.Add(time.Duration(i)*time.Minute))
	}
	writeNote(t, f, "company/30_자료/작업 기록/P/회사.md", "---\nstatus: 진행\nrepos: fixrepo\n---\n## 다음\n- 회사 작업\n", now.Add(time.Hour))
	card := Start("claude", memory.Object{"cwd": f.repo, "source": "startup", "recall_context": true})
	if strings.Contains(card, "회사 작업") || strings.Contains(card, "개인2") || !strings.Contains(card, "개인4") || !strings.Contains(card, "개인3") {
		t.Fatal("personal card mixed categories or more than two candidates", card)
	}
	gitRun(t, f.repo, nil, "remote", "add", "origin", "https://github.com/acme/fixrepo.git")
	card = Start("claude", memory.Object{"cwd": f.repo, "source": "startup", "recall_context": true})
	if strings.Contains(card, "개인 작업") || !strings.Contains(card, "회사 작업") || strings.Contains(card, "데일리 미완료") {
		t.Fatal("company card mixed categories", card)
	}
}

func TestClaudeHumanPromptSources(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	rows := []map[string]any{
		{"promptSource": "typed", "version": "2.1.292", "text": "직접 친 요청"},
		{"promptSource": "queued", "version": "2.1.292", "text": "작업 중 보낸 요청"},
		{"promptSource": "suggestion_accepted", "version": "2.1.292", "text": "추천 고른 요청"},
		{"promptSource": "sdk", "version": "2.1.292", "text": "자동 테스트 요청"},
		{"version": "2.1.270", "text": "/compact"},
		{"version": "2.1.109", "text": "옛 버전에서 친 요청"},
		{"version": "2.1.109", "text": "[Request interrupted by user]"},
		{"version": "2.1.109", "isMeta": true, "text": "옛 버전 메타"},
	}
	var lines []byte
	for _, r := range rows {
		row := map[string]any{"type": "user", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "version": r["version"], "message": map[string]any{"role": "user", "content": r["text"]}}
		for _, k := range []string{"promptSource", "isMeta"} {
			if v, ok := r[k]; ok {
				row[k] = v
			}
		}
		raw, _ := json.Marshal(row)
		lines = append(append(lines, raw...), '\n')
	}
	os.WriteFile(tr, lines, 0o644)
	var got []string
	for _, r := range readTranscript(tr).requests {
		got = append(got, r.Text)
	}
	want := []string{"직접 친 요청", "작업 중 보낸 요청", "추천 고른 요청", "옛 버전에서 친 요청"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestKoreanLines(t *testing.T) {
	f := setup(t, true)
	outside := t.TempDir()
	for _, source := range []string{"compact", "resume"} {
		if card := Start("claude", memory.Object{"cwd": outside, "source": source}); card != compactLine {
			t.Fatalf("KL1 레포 밖에서도 %s 뒤엔 한국어 줄만: %q", source, card)
		}
	}
	if card := Start("claude", memory.Object{"cwd": outside, "source": "startup"}); card != "" {
		t.Fatalf("KL2 레포 밖 새 세션은 조용해야 함: %q", card)
	}
	if card := Start("codex", memory.Object{"cwd": outside, "source": "resume"}); card != "" {
		t.Fatalf("KL3 Codex 에는 붙이지 않음: %q", card)
	}
	if card := Start("claude", memory.Object{"cwd": f.repo, "source": "resume"}); !strings.HasPrefix(card, compactLine+"\n") {
		t.Fatalf("KL4 레포 카드면 맨 위: %s", card)
	}
	raw, _ := json.Marshal(Batch(memory.Object{"session_id": "s"}))
	if !strings.Contains(string(raw), `"hookEventName":"PostToolBatch"`) || !strings.Contains(string(raw), `"additionalContext":"[언어] 진행 안내와 사용자에게 하는 설명은 한국어로 쓴다.`) {
		t.Fatalf("KL5 묶음 안내 형식: %s", raw)
	}
	if out := Batch(memory.Object{"session_id": "s", "agent_id": "a1"}); out != nil {
		t.Fatalf("KL6 하위 에이전트에는 없음: %v", out)
	}
	if !strings.Contains(CompactInstruction, "<summary> 안 첫 줄은 `"+SummaryMarker+"`") {
		t.Fatalf("KL7 압축 지시 문장(전달 검증이 아님): %s", CompactInstruction)
	}
}
