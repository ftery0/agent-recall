package recall

import (
	"context"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Checks run when a memory is read, against git as it is now, and are never written back: the
// ledger keeps what was seen, git says what is true today.

const (
	gitLimit   = 2 * time.Second
	movedLimit = 600 * time.Millisecond
	movedQuiet = 12 * time.Hour // work this recent cannot have moved on much
)

// commitState says whether a commit is in this computer's history and on which remote branches.
func commitState(root, hash, subject string) string {
	if root == "" {
		return "확인 불가(레포 폴더 없음)"
	}
	if hash == "" {
		if hash = sameSubject(root, subject, "--all"); hash == "" {
			return "이 컴퓨터 git 기록에 없음"
		}
	}
	if _, ok := worklog.Git(root, gitLimit, "cat-file", "-e", hash+"^{commit}"); !ok {
		return "이 컴퓨터 git 기록에 없음"
	}
	name := hash[:min(8, len(hash))]
	remote, ok := worklog.Git(root, gitLimit, "branch", "-r", "--contains", hash)
	switch {
	case !ok:
		return "커밋 있음 · 원격 확인 불가"
	case remote == "":
		// A commit no branch holds was reset or rewritten away; it is not one waiting to be pushed.
		// A commit with its subject on a branch is the version that replaced it.
		if local, ok := worklog.Git(root, gitLimit, "branch", "--contains", hash); ok && local == "" {
			if again := sameSubject(root, subject, "--branches", "--remotes"); again != "" {
				return "커밋 " + name + " 은 다시 쓰임 → 새 판 " + strings.TrimPrefix(commitState(root, again, ""), "커밋 ")
			}
			return "커밋 " + name + " 어느 브랜치에도 없음(취소됐거나 다시 쓴 커밋)"
		}
		if remotes, _ := worklog.Git(root, gitLimit, "remote"); remotes == "" {
			return "커밋 " + name + " 있음 · 원격 없는 레포"
		}
		return "커밋 " + name + " 있음 · 원격 브랜치에 없음(안 올림" + fetched(root) + ")"
	}
	// Long-lived branches first: being in main or dev says more than a feature branch does.
	var branches []string
	for _, line := range strings.Split(remote, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.Contains(line, "->") {
			branches = append(branches, line)
		}
	}
	rank := func(b string) int {
		for i, name := range []string{"/main", "/master", "/dev"} {
			if strings.HasSuffix(b, name) {
				return i
			}
		}
		return 3
	}
	sort.SliceStable(branches, func(i, j int) bool { return rank(branches[i]) < rank(branches[j]) })
	shown := strings.Join(branches[:min(2, len(branches))], ", ")
	if len(branches) > 2 {
		shown += fmt.Sprintf(" 외 %d", len(branches)-2)
	}
	return "커밋 " + name + " 있음 · " + shown + " 에 올라감"
}

// sameSubject is the newest commit in refs whose subject is exactly subject; a word of it would
// find other commits.
func sameSubject(root, subject string, refs ...string) string {
	if subject == "" {
		return ""
	}
	args := append(append([]string{"log"}, refs...), "--fixed-strings", "--grep="+subject, "--format=%H%x09%s", "-50")
	out, _ := worklog.Git(root, gitLimit, args...)
	for _, line := range strings.Split(out, "\n") {
		if hash, s, ok := strings.Cut(line, "\t"); ok && s == subject {
			return hash
		}
	}
	return ""
}

// fetched says when the remote branches were last fetched, once that is over a day ago: a commit
// not on them may have been pushed from elsewhere since.
func fetched(root string) string {
	dir, ok := worklog.Git(root, gitLimit, "rev-parse", "--git-common-dir")
	if !ok {
		return ""
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	info, err := os.Stat(filepath.Join(dir, "FETCH_HEAD"))
	if err != nil || time.Since(info.ModTime()) < 24*time.Hour {
		return ""
	}
	return ", 원격 정보는 " + info.ModTime().Format("01-02") + " 에 받아 온 것"
}

// prState is a pull request's merge as the local history knows it; online asks GitHub as well.
func prState(root, owner, repo, n string, online bool) string {
	state := "확인 불가(레포 폴더 없음)"
	if root != "" {
		state = "내 컴퓨터 git 기록에 머지 없음(안 받아 왔을 수 있음)"
		if hash, at, ok := worklog.PRMerged(root, n); ok {
			state = fmt.Sprintf("내 컴퓨터 git 기록: %s 머지됨 (%s)", at.Local().Format("01-02 15:04"), hash)
		}
	}
	if !online {
		return state
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "gh", "pr", "view", n, "--repo", owner+"/"+repo, "--json", "state,mergedAt", "--jq", `.state+" "+.mergedAt`).Output()
	if err != nil {
		return state + " · GitHub 확인 실패"
	}
	return state + " · GitHub: " + strings.TrimSpace(string(raw))
}

// repoFolder is the local clone for a GitHub repository: the folder the fact was seen in, or a git
// folder beside it, whose origin names that repository, else whose folder does (GitHub may have
// renamed it since). None is "": another repository's history would answer for this one.
func repoFolder(cwd, repo string) string {
	root := worklog.RepoRoot(cwd)
	if root == "" || repo == "" {
		return root
	}
	candidates, _ := filepath.Glob(filepath.Join(filepath.Dir(root), "*"))
	candidates = append([]string{root}, candidates...)
	for _, dir := range candidates {
		if worklog.OriginName(dir) == repo {
			return dir
		}
	}
	for _, dir := range candidates {
		if filepath.Base(dir) == repo && worklog.RepoRoot(dir) == dir {
			return dir
		}
	}
	return ""
}

// commitRoot is the repository to check a commit claim in: the folder it was made in, else, when
// that is gone, a clone of the same remote beside the conversation's repository.
func commitRoot(ev factEvidence, cwd string) string {
	if root := worklog.RepoRoot(ev.Cwd); root != "" {
		return root
	}
	if ev.Origin != "" {
		return repoFolder(cwd, originRepo(ev.Origin))
	}
	return ""
}

// verifyFact is a commit's or pull request's state now; other facts have nothing to check.
func verifyFact(f factState, online bool) string {
	cwd := ""
	if len(f.Claims) > 0 {
		cwd = f.Claims[0].Evidence.Cwd
	}
	parts := strings.Split(f.Slot, "/")
	switch {
	case parts[0] == "commit" && len(f.Claims) > 0:
		return commitState(commitRoot(f.Claims[0].Evidence, cwd), f.Ref, f.Value)
	case parts[0] == "pr" && len(parts) == 4:
		return prState(repoFolder(cwd, parts[2]), parts[1], parts[2], parts[3], online)
	}
	return ""
}

// movedFiles counts the files a conversation changed that were committed again after it ended.
// Recent work, a missing folder or a slow git say nothing.
func movedFiles(cwd string, files []string, ended, now time.Time) string {
	root := worklog.RepoRoot(cwd)
	var inside []string
	for _, file := range files {
		// One path outside the repository would fail the whole git call.
		if !filepath.IsAbs(file) && !strings.HasPrefix(file, "..") && len(inside) < 40 {
			inside = append(inside, file)
		}
	}
	if root == "" || len(inside) == 0 || ended.IsZero() || now.Sub(ended) < movedQuiet {
		return ""
	}
	args := append([]string{"log", "--since=" + ended.Format(time.RFC3339), "--name-only", "--format=", "--"}, inside...)
	out, ok := worklog.Git(root, movedLimit, args...)
	if !ok || out == "" {
		return ""
	}
	moved := map[string]bool{}
	for _, file := range strings.Split(out, "\n") {
		if file != "" {
			moved[file] = true
		}
	}
	return fmt.Sprintf("이 대화가 고친 파일 %d개가 그 뒤 다시 커밋됨", len(moved))
}

// sessionCheck is the start card's line under where a conversation stopped: its commits and the
// pull requests it opened as git has them now, and whether its files moved on since.
func sessionCheck(session, cwd string, files []string, ended, now time.Time) string {
	var notes []string
	lines, _ := readFacts()
	commits, prs := 0, 0
	// Folded facts come newest first, and where the conversation stopped is in its last ones.
	for _, f := range foldFacts(lines) {
		parts := strings.Split(f.Slot, "/")
		for _, c := range f.Claims {
			if c.Evidence.Session != session {
				continue
			}
			switch {
			case parts[0] == "commit" && commits < 2 && !(scratchRepo(c.Evidence.Cwd) && c.Evidence.Origin == ""):
				// The commit's own folder: a conversation in one repository can commit in another.
				// When that folder is gone, as a removed worktree or one named by a shell variable,
				// the conversation's repository may share its commits; only a hash found there counts.
				state := commitState(commitRoot(c.Evidence, cwd), f.Ref, f.Value)
				if strings.HasPrefix(state, "확인 불가") && f.Ref != "" {
					if found := commitState(worklog.RepoRoot(cwd), f.Ref, f.Value); strings.HasPrefix(found, "커밋 ") {
						state = found
					}
				}
				// The state names a commit git has; any other is named by its hash or subject.
				if !strings.HasPrefix(state, "커밋 ") {
					name := f.Ref[:min(8, len(f.Ref))]
					if name == "" {
						name = cut(f.Value, 20)
					}
					state = name + " → " + state
				}
				// Another repository's commit says which one.
				if repo := parts[1]; len(parts) > 2 && repo != RepoName(cwd) {
					state = repo + " 레포 " + state
				}
				notes = append(notes, state)
				commits++
			case parts[0] == "pr" && len(parts) == 4 && prs < 2:
				// Merged since, the next step the conversation left may be done.
				notes = append(notes, "PR #"+parts[3]+" → "+prState(repoFolder(c.Evidence.Cwd, parts[2]), parts[1], parts[2], parts[3], false))
				prs++
			}
			break
		}
	}
	if moved := movedFiles(cwd, files, ended, now); moved != "" {
		notes = append(notes, moved)
	}
	if len(notes) == 0 {
		return ""
	}
	return "  확인(git, 지금): " + strings.Join(notes, " · ")
}

// FactsText lists the ledger: standing facts that hold the words, with their state in git now,
// or with review, the AI's proposals and the conflicts waiting on the person.
func FactsText(query string, review, online bool, limit int) (string, error) {
	lines, err := readFacts()
	if err != nil {
		return "", err
	}
	words := terms(query)
	var out []string
	shown, matched := 0, 0
	for _, f := range foldFacts(lines) {
		waiting := f.Status == factProposed || f.Status == factConflict
		if review != waiting || (!review && f.Status != factActive) || !allWords(factKind(f.Slot)+" "+f.Slot+" "+f.Value, words) {
			continue
		}
		matched++
		if shown == limit {
			continue
		}
		shown++
		line := fmt.Sprintf("- %s [%s·%s] %s · %s · %s", shortFact(f.Fact), f.Status, gradeLabel[f.Grade], factKind(f.Slot), cut(f.Value, 120), f.At.Local().Format("2006-01-02 15:04"))
		if len(f.Claims) > 0 && fullSessionID.MatchString(f.Claims[0].Evidence.Session) {
			line += " · 세션 " + short(f.Claims[0].Evidence.Session)
		}
		if check := verifyFact(f, online); check != "" && !review {
			line += "\n    지금: " + check
		}
		out = append(out, line)
	}
	if matched == 0 {
		if review {
			return "확인을 기다리는 사실이 없다", nil
		}
		return "장부에 맞는 확정 사실이 없다 — 후보는 harness recall facts --review", nil
	}
	head := fmt.Sprintf("확정 사실 %d개 — 장부는 본 것, '지금' 줄은 지금 git 이 말하는 것", matched)
	if review {
		head = fmt.Sprintf("확인을 기다리는 사실 %d개 — 채팅에 '확인 f…' 또는 '거절 f…' 를 직접 치면 다음 정리 때 반영된다", matched)
	}
	if matched > shown {
		out = append(out, fmt.Sprintf("… 외 %d개 — --limit 로 늘리거나 말을 붙여 좁힌다", matched-shown))
	}
	return head + "\n" + strings.Join(out, "\n"), nil
}

var gradeLabel = map[string]string{"user": "사용자", "observed": "도구 출력", "ai": "AI 주장"}

func factKind(slot string) string {
	kind, rest, _ := strings.Cut(slot, "/")
	switch kind {
	case "commit":
		repo, _, _ := strings.Cut(rest, "/")
		return "커밋 " + repo
	case "pr":
		parts := strings.Split(rest, "/")
		if len(parts) == 3 {
			return "PR " + parts[1] + "#" + parts[2]
		}
	case "push":
		repo, ref, _ := strings.Cut(rest, "/")
		return "push " + repo + " " + ref
	case "relation":
		from, to, _ := strings.Cut(rest, "/")
		return "레포 관계 " + from + " → " + to
	case "choice":
		return "선택"
	case "rule":
		repo, _, _ := strings.Cut(rest, "/")
		return "규칙 " + repo
	}
	return kind
}

// prMadeIn names the conversation that opened pull request n of the repository at root, from the
// gh output the ledger kept.
func prMadeIn(root, n string) string {
	own := worklog.OriginName(root)
	lines, _ := readFacts()
	for _, f := range foldFacts(lines) {
		if parts := strings.Split(f.Slot, "/"); len(parts) == 4 && parts[0] == "pr" && parts[3] == n && len(f.Claims) > 0 && ownPR(root, own, f) {
			return fmt.Sprintf("만든 대화: %s 세션 %s (gh 출력)", f.At.Local().Format("01-02 15:04"), short(f.Claims[0].Evidence.Session))
		}
	}
	return ""
}

func allWords(text string, words []string) bool {
	lower := strings.ToLower(text)
	for _, w := range words {
		if !strings.Contains(lower, w) {
			return false
		}
	}
	return true
}

// foreignPRHint is a pull request said to be another repository's: checked in that repository
// when the ledger knows which one, never against this one's numbers. When the number is in several
// repositories, the one named before it wins, then a backend one for a backend word, then one
// cloned beside this repository, then the latest.
func foreignPRHint(cwd, n, word string) string {
	root := worklog.RepoRoot(cwd)
	own := worklog.OriginName(root)
	lines, _ := readFacts()
	var pick factState
	best := -1
	for _, f := range foldFacts(lines) {
		parts := strings.Split(f.Slot, "/")
		if len(parts) != 4 || parts[0] != "pr" || parts[3] != n || len(f.Claims) == 0 || ownPR(root, own, f) {
			continue
		}
		dir := repoFolder(cwd, parts[2])
		cloned := worklog.OriginName(dir) == parts[2]
		rank := 0
		switch {
		case strings.EqualFold(parts[2], word) || cloned && strings.EqualFold(filepath.Base(dir), word):
			rank = 3
		case worklog.Backend(word) && worklog.Backend(parts[2]):
			rank = 2
		case cloned:
			rank = 1
		}
		if rank > best || rank == best && f.At.After(pick.At) {
			pick, best = f, rank
		}
	}
	if best >= 0 {
		repo := strings.Split(pick.Slot, "/")[2]
		return fmt.Sprintf("[다른 레포 PR %s#%s] %s\n", repo, n, verifyFact(pick, false)) + findHint("PR #"+n+" 이야기", "#"+n, 2)
	}
	return fmt.Sprintf("[PR #%s] 다른 레포 PR 로 보여 이 레포 git 으로 확인하지 않음\n", n) + findHint("PR #"+n+" 이야기", "#"+n, 2)
}

// ownPR says a pull request fact is the repository at root's: named by its origin or by its
// folder, which keeps the new name when GitHub renamed the repository after it was cloned, or
// opened from a clone or worktree with the same origin.
func ownPR(root, own string, f factState) bool {
	repo := strings.Split(f.Slot, "/")[2]
	if root != "" && (strings.EqualFold(repo, own) || strings.EqualFold(repo, filepath.Base(root))) {
		return true
	}
	return own != "" && worklog.OriginName(worklog.RepoRoot(f.Claims[0].Evidence.Cwd)) == own
}
