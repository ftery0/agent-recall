package recall

import (
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gitAt runs git in dir with its commits dated at.
func gitAt(t *testing.T, dir string, at time.Time, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+at.Format(time.RFC3339), "GIT_AUTHOR_DATE="+at.Format(time.RFC3339))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// clone is a repository folder named name whose origin is GitHub's me/origin, with one commit.
func clone(t *testing.T, parent, name, origin string, at time.Time) string {
	dir := filepath.Join(parent, name)
	gitAt(t, parent, at, "init", "-q", dir)
	gitAt(t, dir, at, "remote", "add", "origin", "https://github.com/me/"+origin+".git")
	gitAt(t, dir, at, "commit", "-q", "--allow-empty", "-m", "first")
	return dir
}

var verifyAt = time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC)

func TestCommitStateSaysWhereACommitIsNow(t *testing.T) {
	fixture(t)
	web := clone(t, t.TempDir(), "web", "web", verifyAt)
	gitAt(t, web, verifyAt, "commit", "-q", "--allow-empty", "-m", "fix: 올림")
	pushed := gitAt(t, web, verifyAt, "rev-parse", "--short=8", "HEAD")
	for _, ref := range []string{"origin/feature/x", "origin/dev", "origin/main"} {
		gitAt(t, web, verifyAt, "update-ref", "refs/remotes/"+ref, "HEAD")
	}
	gitAt(t, web, verifyAt, "commit", "-q", "--allow-empty", "-m", "fix: 안 올림")
	local := gitAt(t, web, verifyAt, "rev-parse", "--short=8", "HEAD")
	found := gitAt(t, web, verifyAt, "rev-parse", "--short", "HEAD")
	gitAt(t, web, verifyAt, "commit", "-q", "--allow-empty", "-m", "fix: 취소함")
	dropped := gitAt(t, web, verifyAt, "rev-parse", "--short=8", "HEAD")
	gitAt(t, web, verifyAt, "reset", "-q", "--hard", "HEAD~1")
	for _, c := range []struct{ root, hash, subject, want string }{
		{web, dropped, "", "커밋 " + dropped + " 어느 브랜치에도 없음(취소됐거나 다시 쓴 커밋)"},
		{"", pushed, "", "확인 불가(레포 폴더 없음)"},
		{web, pushed, "", "커밋 " + pushed + " 있음 · origin/main, origin/dev 외 1 에 올라감"},
		{web, local, "", "커밋 " + local + " 있음 · 원격 브랜치에 없음(안 올림)"},
		{web, "", "fix: 안 올림", "커밋 " + found + " 있음 · 원격 브랜치에 없음(안 올림)"},
		{web, "deadbeef", "", "이 컴퓨터 git 기록에 없음"},
		{web, "", "fix: 없는 제목", "이 컴퓨터 git 기록에 없음"},
	} {
		if got := commitState(c.root, c.hash, c.subject); got != c.want {
			t.Errorf("%q %q: got %q, want %q", c.hash, c.subject, got, c.want)
		}
	}
}

func TestSessionCheckLooksInTheFolderTheCommitWasMadeIn(t *testing.T) {
	fixture(t)
	parent := t.TempDir()
	web, api := clone(t, parent, "web", "web", verifyAt), clone(t, parent, "api", "api", verifyAt)
	gitAt(t, api, verifyAt, "commit", "-q", "--allow-empty", "-m", "fix: 백엔드")
	hash := gitAt(t, api, verifyAt, "rev-parse", "--short=8", "HEAD")
	line := claim("commit/api/"+hash, "fix: 백엔드", "observed", "api", factEvidence{Session: "s1", Cwd: api}, verifyAt)
	line.Ref = hash
	if err := appendFacts(map[string]bool{}, []factLine{line}, verifyAt); err != nil {
		t.Fatal(err)
	}
	// The conversation ran in web and committed in api.
	if got := sessionCheck("s1", web, nil, verifyAt, verifyAt); got != "  확인(git, 지금): 커밋 "+hash+" 있음 · 원격 브랜치에 없음(안 올림)" {
		t.Fatal("커밋한 폴더 대신 대화 폴더에서 찾음", got)
	}
	// A worktree removed since keeps its commits in the conversation's repository; a subject alone
	// is not taken from there.
	wt := filepath.Join(parent, "wt")
	gitAt(t, web, verifyAt, "worktree", "add", "-q", wt)
	gitAt(t, wt, verifyAt, "commit", "-q", "--allow-empty", "-m", "fix: 지운 worktree")
	gone := gitAt(t, wt, verifyAt, "rev-parse", "--short=8", "HEAD")
	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	byHash := claim("commit/wt/"+gone, "fix: 지운 worktree", "observed", "wt", factEvidence{Session: "s2", Cwd: wt}, verifyAt)
	byHash.Ref = gone
	bySubject := claim("commit/wt/x", "fix: 지운 worktree", "observed", "wt", factEvidence{Session: "s3", Cwd: wt}, verifyAt)
	if err := appendFacts(map[string]bool{}, []factLine{byHash, bySubject}, verifyAt); err != nil {
		t.Fatal(err)
	}
	if got := sessionCheck("s2", web, nil, verifyAt, verifyAt); got != "  확인(git, 지금): 커밋 "+gone+" 있음 · 원격 브랜치에 없음(안 올림)" {
		t.Fatal("지운 worktree 의 커밋을 대화 레포에서 찾지 못함", got)
	}
	if got := sessionCheck("s3", web, nil, verifyAt, verifyAt); got != "  확인(git, 지금): fix: 지운 worktree → 확인 불가(레포 폴더 없음)" {
		t.Fatal("제목만으로 다른 폴더의 커밋을 짚음", got)
	}
	gitAt(t, web, verifyAt, "commit", "-q", "--allow-empty", "-m", "Merge pull request #5 from me/x")
	merge := gitAt(t, web, verifyAt, "rev-parse", "--short", "HEAD")
	if err := appendFacts(map[string]bool{}, []factLine{claim("pr/me/web/5", "만듦", "observed", "web", factEvidence{Session: "s4", Cwd: web}, verifyAt)}, verifyAt); err != nil {
		t.Fatal(err)
	}
	if got := sessionCheck("s4", web, nil, verifyAt, verifyAt); got != "  확인(git, 지금): PR #5 → 내 컴퓨터 git 기록: "+verifyAt.Local().Format("01-02 15:04")+" 머지됨 ("+merge+")" {
		t.Fatal("대화가 만든 PR 의 머지를 붙이지 못함", got)
	}
	var three []factLine
	for i, subject := range []string{"첫째", "둘째", "셋째"} {
		at := verifyAt.Add(time.Duration(i) * time.Minute)
		three = append(three, claim("commit/api/"+subject, "fix: "+subject, "observed", "api", factEvidence{Session: "s5", Cwd: api}, at))
	}
	if err := appendFacts(map[string]bool{}, three, verifyAt); err != nil {
		t.Fatal(err)
	}
	if got := sessionCheck("s5", web, nil, verifyAt, verifyAt); got != "  확인(git, 지금): fix: 셋째 → 이 컴퓨터 git 기록에 없음 · fix: 둘째 → 이 컴퓨터 git 기록에 없음" {
		t.Fatal("대화가 멈춘 곳인 마지막 커밋 대신 처음 커밋을 보임", got)
	}
	if got := verifyFact(factState{Slot: "commit/api/x", Ref: hash, Claims: []factLine{{}}}, false); got != "확인 불가(레포 폴더 없음)" {
		t.Fatal("폴더를 모르는 커밋을 이 프로세스의 레포에서 찾음", got)
	}
}

func TestTheCardKeepsRoomForTheGitCheck(t *testing.T) {
	fixture(t)
	web := clone(t, t.TempDir(), "web", "web", verifyAt)
	hash := gitAt(t, web, verifyAt, "rev-parse", "--short=8", "HEAD")
	line := claim("commit/web/"+hash, "first", "observed", "web", factEvidence{Session: "s1", Cwd: web}, verifyAt)
	line.Ref = hash
	if err := appendFacts(map[string]bool{}, []factLine{line}, verifyAt); err != nil {
		t.Fatal(err)
	}
	var requests []worklog.Request
	for i, id := range []string{"a", "b", "c", "d"} {
		requests = append(requests, worklog.Request{ID: id, Line: i + 1, Text: strings.Repeat(id+" 요청 ", 40)})
	}
	// The budget a record gets on a card with two of them.
	view := recordView(resumeRecord{session: "s1", cwd: web, m: summary{Title: "카드"}, requests: requests, last: strings.Repeat("끝 ", 80)}, 600)
	for _, want := range []string{"[d 줄4]", "끝난 지점(AI 주장)", "확인(git, 지금): 커밋 " + hash + " 있음"} {
		if !strings.Contains(view, want) {
			t.Fatalf("긴 요청이 %q 자리를 다 씀\n%s", want, view)
		}
	}
}

func TestMovedFilesCountsLaterCommitsOfTheConversationsFiles(t *testing.T) {
	fixture(t)
	web := clone(t, t.TempDir(), "web", "web", verifyAt.Add(-time.Hour))
	write(t, filepath.Join(web, "a.go"), "package a\n")
	gitAt(t, web, verifyAt.Add(time.Hour), "add", "a.go")
	gitAt(t, web, verifyAt.Add(time.Hour), "commit", "-q", "-m", "다시 고침")
	files := []string{"a.go", "b.go", "/abs/c.go", "../d.go"}
	if got := movedFiles(web, files, verifyAt, verifyAt.Add(24*time.Hour)); got != "이 대화가 고친 파일 1개가 그 뒤 다시 커밋됨" {
		t.Fatal("뒤에 다시 커밋된 파일을 세지 못함", got)
	}
	if got := movedFiles(web, files, verifyAt, verifyAt.Add(2*time.Hour)) + movedFiles(web, files[2:], verifyAt, verifyAt.Add(24*time.Hour)) + movedFiles("", files, verifyAt, verifyAt.Add(24*time.Hour)); got != "" {
		t.Fatal("방금 끝난 대화·레포 밖 파일·폴더 없는 기록에 말을 붙임", got)
	}
}

func TestPRStateReadsTheLocalMerge(t *testing.T) {
	fixture(t)
	merged := time.Date(2026, 10, 14, 8, 53, 0, 0, time.UTC)
	web := clone(t, t.TempDir(), "web", "web", merged)
	gitAt(t, web, merged, "commit", "-q", "--allow-empty", "-m", "Merge pull request #12 from me/x")
	hash := gitAt(t, web, merged, "rev-parse", "--short", "HEAD")
	for n, want := range map[string]string{
		"12": "내 컴퓨터 git 기록: " + merged.Local().Format("01-02 15:04") + " 머지됨 (" + hash + ")",
		"13": "내 컴퓨터 git 기록에 머지 없음(안 받아 왔을 수 있음)",
	} {
		if got := prState(web, "me", "web", n, false); got != want {
			t.Errorf("#%s: got %q, want %q", n, got, want)
		}
	}
	if got := prState("", "me", "web", "12", false); got != "확인 불가(레포 폴더 없음)" {
		t.Fatal(got)
	}
}

func TestForeignPRHintPicksTheRepositoryTheWordNames(t *testing.T) {
	recordFixture(t)
	parent := t.TempDir()
	// GitHub renamed web_old to web after it was cloned; web-copy is another clone of it.
	web, copied := clone(t, parent, "web", "web_old", verifyAt), clone(t, parent, "web-copy", "web_old", verifyAt)
	api := clone(t, parent, "api-be", "api-be", verifyAt)
	merged := verifyAt.Add(time.Hour)
	for _, dir := range []string{web, api} {
		gitAt(t, dir, merged, "commit", "-q", "--allow-empty", "-m", "Merge pull request #7 from me/x")
	}
	pr := func(slot, cwd string, at time.Time) factLine {
		return claim(slot, "만듦", "observed", strings.Split(slot, "/")[2], factEvidence{Session: "s-" + filepath.Base(cwd), Cwd: cwd}, at)
	}
	lines := []factLine{
		pr("pr/me/api-be/7", api, verifyAt), pr("pr/me/design/7", "/gone/design", verifyAt.Add(time.Minute)),
		pr("pr/me/web/7", web, verifyAt.Add(2*time.Minute)), pr("pr/me/web/8", web, verifyAt), pr("pr/me/storyboard/8", copied, verifyAt),
	}
	if err := appendFacts(map[string]bool{}, lines, verifyAt); err != nil {
		t.Fatal(err)
	}
	inAPI := "[다른 레포 PR api-be#7] 내 컴퓨터 git 기록: " + merged.Local().Format("01-02 15:04") + " 머지됨 ("
	for word, want := range map[string]string{
		"백엔드": inAPI, "x": inAPI, // a backend word, or no word naming any: the clone beside this one
		"design": "[다른 레포 PR design#7] 확인 불가(레포 폴더 없음)",
	} {
		if got := foreignPRHint(web, "7", word); !strings.HasPrefix(got, want) {
			t.Errorf("%s: got %q, want %q…", word, got, want)
		}
	}
	// #8 is this repository's own, under its new name and from another clone of it.
	for _, n := range []string{"8", "9"} {
		if got := foreignPRHint(web, n, "백엔드"); !strings.HasPrefix(got, "[PR #"+n+"] 다른 레포 PR 로 보여 이 레포 git 으로 확인하지 않음\n") {
			t.Errorf("#%s: got %q", n, got)
		}
	}
	if got := prMadeIn(web, "8"); !strings.HasPrefix(got, "만든 대화: ") {
		t.Fatal("새 이름으로 남은 이 레포 PR 의 대화를 찾지 못함", got)
	}
}

func TestCommitsInScratchRepositoriesAreNotWork(t *testing.T) {
	fixture(t)
	parent := t.TempDir()
	scratchRoots = []string{parent + "/"}
	web := clone(t, parent, "web", "web", verifyAt)
	hash := gitAt(t, web, verifyAt, "rev-parse", "--short=8", "HEAD")
	line := claim("commit/web/"+hash, "first", "observed", "web", factEvidence{Session: "s1", Cwd: web}, verifyAt)
	line.Ref = hash
	if err := appendFacts(map[string]bool{}, []factLine{line}, verifyAt); err != nil {
		t.Fatal(err)
	}
	if got := sessionCheck("s1", web, nil, verifyAt, verifyAt); got != "" {
		t.Fatal("임시 폴더 커밋을 확인 줄에 보임", got)
	}
	s := session{id: "s2", cwd: web, commands: []command{{text: "git commit -m init", source: "tool:t1", result: "성공", output: "[main (root-commit) " + hash + "] init", at: verifyAt}}}
	if got := sessionFacts(s, nil); len(got) != 0 {
		t.Fatal("임시 폴더 커밋을 장부에 남김", got)
	}
}
