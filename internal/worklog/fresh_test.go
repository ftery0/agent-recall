package worklog

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func commitAt(t *testing.T, dir string, at time.Time, message string, files ...string) string {
	t.Helper()
	for _, file := range files {
		path := filepath.Join(dir, file)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(message), 0o644)
		gitRun(t, dir, nil, "add", file)
	}
	date := at.Format(time.RFC3339)
	gitRun(t, dir, []string{"GIT_COMMITTER_DATE=" + date, "GIT_AUTHOR_DATE=" + date}, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", message)
	out, err := exec.Command("git", "-C", dir, "log", "-1", "--format=%h").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestStartMarksPullRequestsMergedAfterTheNoteWasSaved(t *testing.T) {
	f := setup(t, true)
	t.Setenv("HOME", t.TempDir())
	saved := time.Date(2026, 10, 14, 8, 47, 0, 0, time.UTC)
	merged := time.Date(2026, 10, 14, 8, 53, 45, 0, time.UTC)
	commitAt(t, f.repo, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "Merge pull request #7 from me/old")
	commitAt(t, f.repo, merged, "Merge pull request #12 from me/feature")
	writeNote(t, f, "personal/10_프로젝트/P/작업 기록/일.md", "---\nstatus: 진행\nrepos: fixrepo\n---\n## 현재 상태\n- PR #12 → dev (머지 안 함), 예전 PR #7 은 머지됨\n## 다음\n1. PR #12 리뷰·머지\n", saved)
	card := Start("codex", memory.Object{"cwd": f.repo})
	want := "다음: PR #12 리뷰·머지 (" + saved.Local().Format("01-02 15:04") + ") ⚠ 노트 저장 뒤 PR #12 " + merged.Local().Format("01-02 15:04") + " 머지됨 (내 컴퓨터 git 기록) — 이 '다음'은 끝난 일일 수 있다. 노트를 고칠지 사용자에게 묻는다"
	if !strings.Contains(card, want) || strings.Contains(card, "PR #7 ") {
		t.Fatalf("missing %q or marked a merge older than the note\n%s", want, card)
	}
	squash := commitAt(t, f.repo, merged.Add(time.Hour), "fix: 버튼 겹침 (#15)")
	if hash, at, ok := PRMerged(f.repo, "15"); !ok || hash != squash || !at.Equal(merged.Add(time.Hour)) {
		t.Fatal("합쳐 넣은 PR 을 못 찾음", hash, at, ok)
	}
	if _, _, ok := PRMerged(f.repo, "1"); ok {
		t.Fatal("다른 번호의 PR 을 머지로 봄")
	}
}

func TestStartWarnsAboutProjectMemoriesWhoseFilesWereCommittedAfterThem(t *testing.T) {
	f := setup(t, true)
	t.Setenv("HOME", t.TempDir())
	backend := filepath.Join(filepath.Dir(f.repo), "backend")
	gitRun(t, filepath.Dir(f.repo), nil, "init", "-q", backend)
	file := "app/api/비디오/services.py"
	commitAt(t, backend, time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC), "before", file)
	after := time.Date(2026, 10, 15, 8, 36, 0, 0, time.UTC)
	hash := commitAt(t, backend, after, "fix: 컷 상태", file)
	commitAt(t, backend, after.Add(time.Hour), "fix: 다시", file)
	projects := filepath.Join(t.TempDir(), "projects", "fixrepo")
	memories := filepath.Join(projects, "memory")
	body := "\n---\n5. 백엔드 커밋·PR 전. 이 작업 파일: `" + file + "`, `apps/user/없는파일.ts`\n"
	write := func(name, kind string) {
		os.MkdirAll(memories, 0o755)
		text := "---\nname: " + name + "\nmetadata:\n  type: " + kind + "\n  modified: 2026-10-15T06:38:37.608Z" + body
		if err := os.WriteFile(filepath.Join(memories, name+".md"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("project_tsk", "project")
	write("feedback_style", "feedback")
	os.WriteFile(filepath.Join(memories, "MEMORY.md"), []byte("- 파일 "+file+"\n"), 0o644)
	payload := memory.Object{"cwd": f.repo, "transcript_path": filepath.Join(projects, "session.jsonl")}
	card := Start("claude", payload)
	saved := time.Date(2026, 10, 15, 6, 38, 37, 0, time.UTC)
	want := "⚠ 기억 project_tsk.md(" + saved.Local().Format("01-02 15:04") + " 저장) 뒤 관련 파일 커밋 2개 — 첫 커밋 backend " + hash + " " + after.Local().Format("01-02 15:04") + ". 메모 속 진행 상태는 git 으로 다시 본다"
	if !strings.Contains(card, want) || strings.Contains(card, "feedback_style") || strings.Contains(card, "MEMORY.md") {
		t.Fatalf("missing %q or warned about a memory that records no progress\n%s", want, card)
	}
	// Codex has no transcript beside the memories, so it looks in every Claude profile.
	home := os.Getenv("HOME")
	profile := filepath.Join(home, ".claude-profiles", "work", "projects", folderName.ReplaceAllString(f.repo, "-"))
	if err := os.MkdirAll(filepath.Dir(profile), 0o755); err != nil || os.Rename(projects, profile) != nil {
		t.Fatal("move memories", err)
	}
	if card := Start("codex", memory.Object{"cwd": f.repo}); !strings.Contains(card, want) {
		t.Fatal("Codex 시작 안내가 Claude 기억을 못 찾음", card)
	}
}
