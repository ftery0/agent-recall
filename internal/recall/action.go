package recall

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Before a commit, a push or a copy, the memory that matters is about the repositories it touches:
// whether one can be pushed at all, where its work was copied to, and what the person asked the last
// time it went out. The hook before a shell command, the prompt hint and the start card share it.

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

// repoNames are the repositories the ledger knows by name, long enough to spot in a request.
func repoNames(folded []factState) []string {
	var names []string
	for _, f := range folded {
		parts := strings.Split(f.Slot, "/")
		var found []string
		switch parts[0] {
		case "commit", "push", "rule":
			found = parts[1:2]
		case "relation":
			found = parts[1:3]
		case "pr":
			if len(parts) == 4 {
				found = parts[2:3]
			}
		}
		for _, name := range found {
			if len(name) >= 4 && name != "전체" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

// namedRepos are the known repositories a request names, as a word of its own: "agent-recall레포"
// names agent-recall, "agent-recall-v2" does not.
func namedRepos(prompt string, folded []factState) []string {
	var out []string
	for _, name := range repoNames(folded) {
		if regexp.MustCompile(`(?i)(?:^|[^-\w.])` + regexp.QuoteMeta(name) + `(?:$|[^-\w.])`).MatchString(prompt) {
			out = append(out, name)
		}
	}
	return out
}

// repoFolderNamed is a local folder of the repository called name: one the ledger saw work in, else
// a clone beside the current repository.
func repoFolderNamed(name, cwd string, folded []factState) string {
	for _, f := range folded {
		parts := strings.Split(f.Slot, "/")
		if len(parts) < 2 || parts[1] != name || parts[0] != "commit" && parts[0] != "push" {
			continue
		}
		for _, c := range f.Claims {
			if root := worklog.RepoRoot(c.Evidence.Cwd); root != "" && RepoName(root) == name {
				return root
			}
		}
	}
	return repoFolder(cwd, name)
}

// repoHint is what a request naming a repository gets: the repository's line, then where it was
// talked about.
func repoHint(name, cwd string, folded []factState) string {
	var lines []string
	if note := repoNote(repoFolderNamed(name, cwd, folded), folded, true); note != "" {
		lines = append(lines, "["+note+"]")
	} else {
		lines = append(lines, "[레포 "+name+": 이 컴퓨터에서 폴더를 찾지 못함]")
	}
	return strings.Join(append(lines, findHint(name+" 이야기", name, 2)), "\n")
}

// workedRepos are the repositories a conversation changed files in, read from its transcript
// without parsing the lines that cannot name a file.
func workedRepos(transcript string) []string {
	f, err := os.Open(transcript)
	if err != nil {
		return nil
	}
	defer f.Close()
	var roots []string
	seen := map[string]bool{}
	note := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		if root := worklog.RepoRoot(filepath.Dir(path)); root != "" && !scratchRepo(mainRoot(root)) && !slices.Contains(roots, root) {
			roots = append(roots, root)
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		switch {
		case bytes.Contains(line, []byte(`"file_path"`)) || bytes.Contains(line, []byte(`"notebook_path"`)):
			var row struct {
				Message struct {
					Content []struct {
						Type  string            `json:"type"`
						Input map[string]string `json:"input"`
					} `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &row) == nil {
				for _, block := range row.Message.Content {
					if block.Type == "tool_use" {
						note(block.Input["file_path"])
						note(block.Input["notebook_path"])
					}
				}
			}
		case bytes.Contains(line, []byte("*** Update File: ")) || bytes.Contains(line, []byte("*** Add File: ")):
			var row struct {
				Payload struct {
					Input, Arguments string
				} `json:"payload"`
			}
			if json.Unmarshal(line, &row) == nil {
				for _, m := range patchFile.FindAllStringSubmatch(row.Payload.Input+row.Payload.Arguments, -1) {
					note(m[1])
				}
			}
		}
	}
	return roots
}

// actionNotes are the lines worth saying before work goes to the repositories at targets: each
// unusual one's line, and the conversation's other repositories when it worked in more than one.
// A line already said in this conversation is not said again.
func actionNotes(targets []string, transcript, session string) []string {
	lines, _ := readFacts()
	folded := foldFacts(lines)
	var notes []string
	for _, root := range targets {
		if note := repoNote(root, folded, false); note != "" {
			notes = append(notes, note)
		}
	}
	all := append([]string(nil), targets...)
	for _, root := range workedRepos(transcript) {
		if !slices.Contains(all, root) {
			all = append(all, root)
		}
	}
	if len(all) > 1 {
		var names []string
		for _, root := range all {
			names = append(names, RepoName(root)+" ("+root+")")
		}
		notes = append(notes, "이 대화가 다룬 레포: "+strings.Join(names, ", "))
	}
	return unseen(session, notes)
}

// ActionText is what the hook before a shell command adds when the command commits, pushes, merges,
// copies between repositories or changes GitHub.
func ActionText(command, cwd, session, transcript string) string {
	var targets []string
	acts := false
	for _, step := range shellSteps(cwd, command) {
		if !step.writesHistory() && step.kind != "copy" && !ghWrites[step.kind] {
			continue
		}
		acts = true
		for _, dir := range append([]string{step.dir, step.to}, step.from...) {
			if root := worklog.RepoRoot(nearestFolder(dir)); root != "" && !slices.Contains(targets, root) {
				targets = append(targets, root)
			}
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
	notes := actionNotes(targets, transcript, session)
	if len(notes) == 0 {
		return ""
	}
	return actionHeader + "\n" + strings.Join(notes, "\n")
}

// nearestFolder is the path, or its nearest parent that exists: a copy's destination may not yet.
func nearestFolder(path string) string {
	for path != "" && path != "/" && path != "." {
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path
		}
		path = filepath.Dir(path)
	}
	return ""
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
