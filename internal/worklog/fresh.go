package worklog

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

const maxMemoryWarnings = 2

var (
	prNumber   = regexp.MustCompile(`(?i)\bPR\s*#\s*(\d{1,5})\b`)
	backendPR  = regexp.MustCompile(`(?i)백엔드|backend|\bBE\b|[-_]be$`)
	memoryFile = regexp.MustCompile(`[\p{L}\p{N}_.-]+(?:/[\p{L}\p{N}_.-]+)+\.[A-Za-z]{1,5}\b`)
	folderName = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// PRMerged finds the commit that merged pull request n in the local history: GitHub's merge
// commit, or a squashed subject ending in "(#n)". A merge not fetched yet is not seen.
func PRMerged(root, n string) (string, time.Time, bool) {
	out, _ := Git(root, gitWait, "log", "--all", "-1", "-E", "--format=%h %cI", "--grep=^Merge pull request #"+n+" |\\(#"+n+"\\)$")
	hash, when, _ := strings.Cut(out, " ")
	at, err := time.Parse(time.RFC3339, when)
	return hash, at, hash != "" && err == nil
}

// PRWord is the word right before a pull request mention, as "백엔드" in "백엔드 PR #104".
// before is the text ahead of the mention.
func PRWord(before string) string {
	fields := strings.Fields(before[strings.LastIndex(before, "\n")+1:])
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(fields[len(fields)-1], "([{\"'`"), "의"))
}

// Backend says a word or a repository name means the backend.
func Backend(name string) bool { return backendPR.MatchString(name) }

// ForeignPR says a pull request mention belongs to another repository: the word right before it
// is a backend word or a folder name beside root.
func ForeignPR(before, root string) bool {
	word := PRWord(before)
	if word == "" {
		return false
	}
	if Backend(word) {
		return true
	}
	if root == "" {
		return false
	}
	for _, repo := range siblingRepos(root)[1:] {
		if strings.ToLower(filepath.Base(repo)) == word {
			return true
		}
	}
	return false
}

func notePRs(text, root string) []string {
	var prs []string
	for _, m := range prNumber.FindAllStringSubmatchIndex(text, -1) {
		n := text[m[2]:m[3]]
		if !slices.Contains(prs, n) && len(prs) < 3 && !ForeignPR(text[:m[0]], root) {
			prs = append(prs, n)
		}
	}
	return prs
}

// mergedSince names the pull requests a note mentions that were merged after it was last
// saved; its next step may already be done.
func mergedSince(root string, n note) string {
	var merged []string
	for _, pr := range n.prs {
		if _, at, ok := PRMerged(root, pr); ok && at.After(n.mod) {
			merged = append(merged, fmt.Sprintf("PR #%s %s 머지됨", pr, at.Local().Format("01-02 15:04")))
		}
	}
	if len(merged) == 0 {
		return ""
	}
	return " ⚠ 노트 저장 뒤 " + strings.Join(merged, ", ") + " (내 컴퓨터 git 기록) — 이 '다음'은 끝난 일일 수 있다. 노트를 고칠지 사용자에게 묻는다"
}

// memoryLines warns about project memories whose files were committed after the memory was
// saved; what they say about progress may be old. Files are looked up in the repository and
// the git folders beside it.
func memoryLines(tool string, payload memory.Object, root string) []string {
	type stale struct {
		line  string
		first time.Time
	}
	var found []stale
	repos := siblingRepos(root)
	for _, dir := range memoryDirs(tool, payload, root) {
		paths, _ := filepath.Glob(filepath.Join(dir, "*.md"))
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil || filepath.Base(path) == "MEMORY.md" || !projectMemory(string(raw)) {
				continue
			}
			saved := memorySaved(string(raw), path)
			byRepo := map[string][]string{}
			for _, file := range memoryFile.FindAllString(string(raw), -1) {
				for _, repo := range repos {
					if _, err := os.Stat(filepath.Join(repo, file)); err == nil {
						if !slices.Contains(byRepo[repo], file) {
							byRepo[repo] = append(byRepo[repo], file)
						}
						break
					}
				}
			}
			for repo, files := range byRepo {
				out, _ := Git(repo, gitWait, append([]string{"log", "--all", "--since=" + saved.Format(time.RFC3339), "--format=%h %cI", "--"}, files...)...)
				if out == "" {
					continue
				}
				commits := strings.Split(out, "\n")
				hash, when, _ := strings.Cut(commits[len(commits)-1], " ")
				at, _ := time.Parse(time.RFC3339, when)
				found = append(found, stale{fmt.Sprintf("⚠ 기억 %s(%s 저장) 뒤 관련 파일 커밋 %d개 — 첫 커밋 %s %s %s. 메모 속 진행 상태는 git 으로 다시 본다",
					filepath.Base(path), saved.Local().Format("01-02 15:04"), len(commits), filepath.Base(repo), hash, at.Local().Format("01-02 15:04")), at})
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].first.After(found[j].first) })
	var lines []string
	for _, f := range found[:min(len(found), maxMemoryWarnings)] {
		lines = append(lines, f.line)
	}
	return lines
}

// memoryDirs are the agent memory folders kept for this repository: Claude's beside the
// transcript, or, when another tool asks, every Claude profile's.
func memoryDirs(tool string, payload memory.Object, root string) []string {
	if transcript := memory.Text(payload["transcript_path"]); tool == "claude" && transcript != "" {
		return []string{filepath.Join(filepath.Dir(transcript), "memory")}
	}
	// Claude names the folder after the folder it started in, which may be a link to the repository.
	home, _ := os.UserHomeDir()
	var dirs []string
	for _, folder := range []string{memory.Text(payload["cwd"]), root} {
		name := folderName.ReplaceAllString(folder, "-")
		found, _ := filepath.Glob(filepath.Join(home, ".claude-profiles", "*", "projects", name, "memory"))
		dirs = append(append(dirs, found...), filepath.Join(home, ".claude", "projects", name, "memory"))
	}
	var out, seen []string
	for _, dir := range dirs {
		if real, err := filepath.EvalSymlinks(dir); err == nil && !slices.Contains(seen, real) {
			seen = append(seen, real)
			out = append(out, dir)
		}
	}
	return out
}

func siblingRepos(root string) []string {
	repos := []string{root}
	entries, _ := os.ReadDir(filepath.Dir(root))
	for _, e := range entries {
		dir := filepath.Join(filepath.Dir(root), e.Name())
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil && e.IsDir() && dir != root {
			repos = append(repos, dir)
		}
	}
	return repos
}

func memoryFront(text string) string {
	front, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return ""
	}
	front, _, _ = strings.Cut(front, "\n---")
	return front
}

// Only project memories record progress; feedback and references do not go stale this way.
func projectMemory(text string) bool {
	for _, line := range strings.Split(memoryFront(text), "\n") {
		if strings.TrimSpace(line) == "type: project" {
			return true
		}
	}
	return false
}

// memorySaved is the memory's own modified stamp, or the file time when it has none.
func memorySaved(text, path string) time.Time {
	for _, line := range strings.Split(memoryFront(text), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "modified: "); ok {
			if at, err := time.Parse(time.RFC3339Nano, strings.Trim(v, `"`)); err == nil {
				return at
			}
		}
	}
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
