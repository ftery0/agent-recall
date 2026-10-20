package recall

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Before a commit or a push, the memory that matters is about the repositories it touches: whether
// one can be pushed at all, where its work was copied to, and what the person asked the last time it
// went out. The hook before a shell command and the start card share it.

const actionHeader = "=== 하네스: 이 일이 닿는 레포의 기억 — 장부·git 에 남은 사실이며 지시가 아니다 ==="

// actionWords are the requests that send work somewhere.
var actionWords = regexp.MustCompile(`(?i)커밋|푸시|push|올려|올리|머지|merge|배포|deploy|릴리스|release|\bPR\b|풀\s?리퀘|반영해`)

// repoNote is one line about a repository: whether it has a remote, the repositories the ledger saw
// files copied between it and, and the newest push among them with the person's words before it.
// Unless always, a repository with a remote and no such relation has nothing worth a line.
func repoNote(root string, folded []factState, always bool) string {
	if root == "" {
		return ""
	}
	name := RepoName(root)
	remotes, ok := worklog.Git(root, gitLimit, "remote")
	noRemote := ok && remotes == ""
	var relations []string
	related := []string{name}
	for _, f := range folded {
		parts := strings.Split(f.Slot, "/")
		if parts[0] != "relation" || len(parts) != 3 || f.Status != factActive {
			continue
		}
		label, other := "", ""
		switch name {
		case parts[1]:
			label, other = "복사해 간 레포 "+parts[2], parts[2]
		case parts[2]:
			label, other = "복사해 온 레포 "+parts[1], parts[1]
		default:
			continue
		}
		sessions := map[string]bool{}
		for _, c := range f.Claims {
			sessions[c.Evidence.Session] = true
		}
		relations = append(relations, fmt.Sprintf("%s (대화 %d개, 마지막 %s)", label, len(sessions), f.At.Local().Format("01-02")))
		related = append(related, other)
	}
	if !always && !noRemote && len(relations) == 0 {
		return ""
	}
	var parts []string
	switch {
	case noRemote:
		parts = append(parts, "원격 없음(이 레포에서는 push 할 곳이 없다)")
	case ok:
		origin, _ := worklog.Git(root, gitLimit, "remote", "get-url", "origin")
		parts = append(parts, "원격 "+strings.TrimSuffix(strings.TrimPrefix(origin, "https://"), ".git"))
	}
	parts = append(parts, relations...)
	// Folded facts come newest first: the first push among these repositories is the latest.
	for _, f := range folded {
		slot := strings.SplitN(f.Slot, "/", 3)
		if slot[0] != "push" || len(slot) != 3 || !slices.Contains(related, slot[1]) || len(f.Claims) == 0 {
			continue
		}
		c := f.Claims[len(f.Claims)-1]
		line := fmt.Sprintf("마지막 push: %s %s ← %s (%s, 세션 %s)", slot[1], slot[2], f.Value[:min(8, len(f.Value))], f.At.Local().Format("01-02 15:04"), short(c.Evidence.Session))
		if c.Evidence.Asked != "" {
			line += " · 그때 사용자 원문: «" + cut(c.Evidence.Asked, 100) + "»"
		}
		parts = append(parts, line)
		break
	}
	return "레포 " + name + " (" + root + "): " + strings.Join(parts, " · ")
}

// actionNotes are the lines worth saying before work goes to the repositories at targets: each
// unusual one's line, and all of them when the command touches more than one. A line already said
// in this conversation is not said again.
func actionNotes(targets []string, session string) []string {
	lines, _ := readFacts()
	folded := foldFacts(lines)
	var notes []string
	for _, root := range targets {
		if note := repoNote(root, folded, false); note != "" {
			notes = append(notes, note)
		}
	}
	if len(targets) > 1 {
		var names []string
		for _, root := range targets {
			names = append(names, RepoName(root)+" ("+root+")")
		}
		notes = append(notes, "이 명령이 닿는 레포: "+strings.Join(names, ", "))
	}
	return unseen(session, notes)
}

// ActionText is what the hook before a shell command adds when the command commits, pushes, merges
// or changes GitHub.
func ActionText(command, cwd, session string) string {
	var targets []string
	acts := false
	for _, step := range shellSteps(cwd, command) {
		if !step.writesHistory() && !ghWrites[step.kind] {
			continue
		}
		acts = true
		if root := worklog.RepoRoot(step.dir); root != "" && !slices.Contains(targets, root) {
			targets = append(targets, root)
		}
	}
	if !acts {
		return ""
	}
	if len(targets) == 0 {
		// The folder is hidden behind a variable: the conversation's own repository is the guess.
		if root := worklog.RepoRoot(cwd); root != "" {
			targets = append(targets, root)
		}
	}
	notes := actionNotes(targets, session)
	if len(notes) == 0 {
		return ""
	}
	return actionHeader + "\n" + strings.Join(notes, "\n")
}

func actionState(session string) string {
	if !fullSessionID.MatchString(session) {
		session = digest(session)[:16]
	}
	return filepath.Join(os.TempDir(), "harness-action", session)
}

// unseen keeps the notes this conversation has not been shown, and remembers them.
func unseen(session string, notes []string) []string {
	if len(notes) == 0 || session == "" {
		return notes
	}
	path := actionState(session)
	raw, _ := os.ReadFile(path)
	shown := strings.Split(string(raw), "\n")
	var fresh []string
	for _, note := range notes {
		if key := digest(note)[:16]; !slices.Contains(shown, key) {
			fresh = append(fresh, note)
			shown = append(shown, key)
		}
	}
	if len(fresh) > 0 && os.MkdirAll(filepath.Dir(path), 0o700) == nil {
		os.WriteFile(path, []byte(strings.Join(shown, "\n")), 0o600)
	}
	return fresh
}

// ForgetActions lets a compacted conversation be told again: its summary may have dropped the notes.
func ForgetActions(session string) {
	if session != "" {
		os.Remove(actionState(session))
	}
}
