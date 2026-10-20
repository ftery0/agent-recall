package recall

import (
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A conversation's git work is read back from git itself. A commit or a push leaves an entry in the
// repository's reflog whatever the command printed: -q, rtk's "ok", a pipe or a shell function hide
// git's own lines, and a chain that failed only at its push still made its commit.

// shellStep is one call in a shell command that matters to memory, with the folder it ran in. dir
// is "" when a variable the command did not set, or a substitution, hides it.
type shellStep struct {
	kind string // a git subcommand (commit, push, …), a gh one ("pr create"), "check", "copy" or "write"
	dir  string
	args []string // the words after the subcommand, variables expanded where known
	from []string // a copy's sources
	to   string   // a copy's destination, a written file, or the folder init and clone make
}

// writesHistory says a step can make commits or move remote branches, so git's reflog has its trace.
func (s shellStep) writesHistory() bool {
	switch s.kind {
	case "commit", "merge", "pull", "cherry-pick", "revert", "rebase", "am", "push":
		return true
	}
	return false
}

var (
	shellName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// Words before the program itself: wrappers that run what follows, and shell keywords.
	shellPrefix = map[string]bool{"rtk": true, "proxy": true, "command": true, "env": true, "time": true, "nohup": true, "sudo": true, "exec": true,
		"if": true, "then": true, "else": true, "elif": true, "while": true, "until": true, "do": true, "!": true}
	// gh calls that change GitHub, as opposed to reading it.
	ghWrites = map[string]bool{"pr create": true, "pr merge": true, "pr close": true, "pr edit": true, "pr comment": true, "pr review": true, "pr ready": true,
		"repo create": true, "repo delete": true, "repo rename": true, "repo edit": true, "repo fork": true}
	checkScript = regexp.MustCompile(`^(test|lint|typecheck|type-check|tsc|build|vet|eslint|jest|vitest|check)(:\S+)?$`)
	inPlace     = regexp.MustCompile(`^(-i|--in-place)|^-\w*i$`) // sed -i '' …, perl -pi -e …
	// Package manager calls that only change what is installed, whatever package they name.
	installs = map[string]bool{"add": true, "install": true, "i": true, "remove": true, "rm": true, "uninstall": true, "update": true, "up": true, "upgrade": true, "info": true, "view": true, "why": true, "outdated": true, "create": true, "init": true}
	// rsync options whose value is the next word.
	rsyncValue = map[string]bool{"--exclude": true, "--include": true, "--exclude-from": true, "--include-from": true, "--filter": true, "-f": true,
		"--files-from": true, "--rsh": true, "-e": true, "--chmod": true, "--chown": true, "--backup-dir": true, "--suffix": true, "--compare-dest": true,
		"--copy-dest": true, "--link-dest": true, "--partial-dir": true, "--temp-dir": true, "-T": true, "--max-size": true, "--min-size": true,
		"--timeout": true, "--bwlimit": true, "--log-file": true, "--password-file": true, "--port": true, "--out-format": true, "--rsync-path": true}
)

// shellPart is a simple command of a shell line, or the start or end of a subshell.
type shellPart struct {
	open, close bool
	words       []string // raw, quotes kept: they decide what expands
}

// splitShell cuts a shell line into simple commands at ;, &, |, newlines, braces and parentheses,
// outside quotes and command substitutions.
func splitShell(shell string) []shellPart {
	var parts []shellPart
	var words []string
	var word strings.Builder
	inWord := false
	endWord := func() {
		if inWord {
			words, inWord = append(words, word.String()), false
			word.Reset()
		}
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			parts, words = append(parts, shellPart{words: words}), nil
		}
	}
	runes := []rune(shell)
	var quote rune
	depth := 0 // inside $( ) or ` `
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case quote != 0:
			word.WriteRune(r)
			if r == '\\' && quote == '"' && next != 0 {
				word.WriteRune(next)
				i++
			} else if r == quote {
				quote = 0
			}
		case r == '\\' && next == '\n':
			i++ // a line continuation joins the lines into one command
		case r == '\\' && next != 0:
			word.WriteRune(r)
			word.WriteRune(next)
			i++
			inWord = true
		case r == '\'' || r == '"':
			quote, inWord = r, true
			word.WriteRune(r)
		case r == '$' && (next == '(' || next == '{'):
			word.WriteRune(r)
			word.WriteRune(next)
			i++
			inWord = true
			if next == '{' {
				for i+1 < len(runes) && runes[i] != '}' {
					i++
					word.WriteRune(runes[i])
				}
			} else {
				depth++
			}
		case r == '`':
			word.WriteRune(r)
			inWord = true
			if depth > 0 && strings.Count(word.String(), "`")%2 == 0 {
				depth--
			} else if depth == 0 {
				depth++
			}
		case depth > 0:
			word.WriteRune(r)
			if r == '(' {
				depth++
			} else if r == ')' {
				depth--
			}
		case r == '&' && (next == '>' || strings.HasSuffix(word.String(), ">") || strings.HasSuffix(word.String(), "<")):
			// 2>&1 and &>file are redirections, not a command sent to the background.
			word.WriteRune(r)
			inWord = true
		case r == '(':
			endCommand()
			parts = append(parts, shellPart{open: true})
		case r == ')':
			endCommand()
			parts = append(parts, shellPart{close: true})
		case r == ';' || r == '&' || r == '|' || r == '\n' || r == '{' || r == '}':
			endCommand()
		case r == ' ' || r == '\t' || r == '\r':
			endWord()
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	endCommand()
	return parts
}

// expand is a word as the shell would pass it, with the variables the command set, $HOME, ~, $PWD
// and $(pwd). known is false when anything else is left to the shell.
func expand(word string, vars map[string]string, dir string) (out string, known bool) {
	var b strings.Builder
	runes := []rune(word)
	var quote rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				b.WriteRune(r)
			}
		case r == '\\' && i+1 < len(runes):
			i++
			b.WriteRune(runes[i])
		case r == '\'' && quote == 0:
			quote = '\''
		case r == '"':
			quote = map[bool]rune{true: 0, false: '"'}[quote == '"']
		case r == '~' && i == 0 && (len(runes) == 1 || runes[1] == '/'):
			b.WriteString(vars["HOME"])
		case r == '`' || (r == '$' && i+1 < len(runes) && runes[i+1] == '('):
			inner, end := substitution(runes, i)
			switch strings.TrimSpace(inner) {
			case "pwd":
				if dir == "" {
					return "", false
				}
				b.WriteString(dir)
			case "git rev-parse --show-toplevel":
				root := worklog.RepoRoot(dir)
				if root == "" {
					return "", false
				}
				b.WriteString(root)
			default:
				return "", false
			}
			i = end
		case r == '$' && i+1 < len(runes):
			name, end := "", i+1
			if runes[end] == '{' {
				close := slices.Index(runes[end:], '}')
				if close < 0 {
					return "", false
				}
				name, end = string(runes[end+1:end+close]), end+close+1
			} else {
				for end < len(runes) && (runes[end] == '_' || runes[end] < 0x80 && (runes[end] >= 'A' && runes[end] <= 'Z' || runes[end] >= 'a' && runes[end] <= 'z' || runes[end] >= '0' && runes[end] <= '9')) {
					end++
				}
				name = string(runes[i+1 : end])
			}
			value, ok := vars[name]
			if name == "PWD" && dir != "" {
				value, ok = dir, true
			}
			if !ok {
				return "", false
			}
			b.WriteString(value)
			i = end - 1
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), true
}

// substitution is the text inside $( ) or ` ` starting at i, and where it ends.
func substitution(runes []rune, i int) (string, int) {
	if runes[i] == '`' {
		end := slices.Index(runes[i+1:], '`')
		if end < 0 {
			return string(runes[i+1:]), len(runes) - 1
		}
		return string(runes[i+1 : i+1+end]), i + 1 + end
	}
	depth := 0
	for j := i + 1; j < len(runes); j++ {
		switch runes[j] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return string(runes[i+2 : j]), j
			}
		}
	}
	return string(runes[i+2:]), len(runes) - 1
}

// shellSteps follows a shell line's folder changes, cd, pushd, popd and subshells, and the variables
// it sets, and returns its git, gh, check, copy and file-writing calls in order.
func shellSteps(cwd, text string) []shellStep {
	home, _ := os.UserHomeDir()
	vars := map[string]string{"HOME": home}
	dir := cwd
	var subshells, pushed []string
	var steps []shellStep
	known := func(word string) (string, bool) { return expand(word, vars, dir) }
	at := func(path string) string {
		switch {
		case filepath.IsAbs(path):
			return filepath.Clean(path)
		case dir == "":
			return ""
		}
		return filepath.Join(dir, path)
	}
	wrote := func(word string) {
		if path, ok := known(word); ok && path != "" && !strings.HasPrefix(path, "&") {
			steps = append(steps, shellStep{kind: "write", dir: dir, to: at(path)})
		}
	}
	for _, part := range splitShell(shellOnly(text)) {
		switch {
		case part.open:
			subshells = append(subshells, dir)
			continue
		case part.close:
			if n := len(subshells); n > 0 {
				dir, subshells = subshells[n-1], subshells[:n-1]
			}
			continue
		}
		// Redirections: standard output sent to a file is a file written; the program sees neither word.
		var words []string
		for i := 0; i < len(part.words); i++ {
			word := part.words[i]
			op := strings.TrimLeft(word, "0123456789&")
			if !strings.HasPrefix(op, ">") && !strings.HasPrefix(op, "<") {
				words = append(words, word)
				continue
			}
			target := strings.TrimLeft(op, "<>|")
			if target == "" && i+1 < len(part.words) {
				i++
				target = part.words[i]
			}
			if fd := word[:len(word)-len(op)]; op[0] == '>' && (fd == "" || fd == "1") {
				wrote(target)
			}
		}
		// Wrappers, keywords and assignments before the program. On their own, assignments set the
		// variable for what follows ("W=…; cd $W").
		for len(words) > 0 {
			if shellPrefix[words[0]] || words[0] == "export" || words[0] == "local" || words[0] == "readonly" {
				words = words[1:]
				continue
			}
			name, value, ok := strings.Cut(words[0], "=")
			if !ok || !shellName.MatchString(name) {
				break
			}
			if v, ok := known(value); ok {
				vars[name] = v
			} else {
				delete(vars, name)
			}
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		program := filepath.Base(words[0])
		switch {
		case program == "cd" || program == "pushd":
			if program == "pushd" {
				pushed = append(pushed, dir)
			}
			target := home
			if len(words) > 1 {
				if path, ok := known(words[1]); ok && words[1] != "-" {
					target = at(path)
				} else {
					target = ""
				}
			}
			dir = target
		case program == "popd":
			if n := len(pushed); n > 0 {
				dir, pushed = pushed[n-1], pushed[:n-1]
			}
		case program == "git":
			if step, ok := gitStep(words[1:], dir, known, at); ok {
				steps = append(steps, step)
			}
		case program == "gh":
			if len(words) >= 3 {
				steps = append(steps, shellStep{kind: words[1] + " " + words[2], dir: dir})
			}
		case program == "cp" || program == "rsync" || program == "ditto":
			var paths []string
			for i := 1; i < len(words); i++ {
				w := words[i]
				if strings.HasPrefix(w, "-") {
					if program == "rsync" && rsyncValue[w] {
						i++ // a pattern or a program, not a path
					}
					continue
				}
				if v, ok := known(w); ok && !strings.Contains(v, "*") {
					paths = append(paths, at(v))
				} else {
					paths = append(paths, "")
				}
			}
			if len(paths) >= 2 {
				steps = append(steps, shellStep{kind: "copy", dir: dir, from: paths[:len(paths)-1], to: paths[len(paths)-1]})
			}
		case program == "tee":
			for _, w := range words[1:] {
				if !strings.HasPrefix(w, "-") {
					wrote(w)
				}
			}
		case (program == "sed" || program == "perl") && slices.ContainsFunc(words[1:], inPlace.MatchString) && !strings.HasPrefix(words[len(words)-1], "-"):
			wrote(words[len(words)-1])
		case (program == "bash" || program == "sh" || program == "zsh") && len(words) >= 3 && (words[1] == "-c" || words[1] == "-lc"):
			if inner, ok := known(words[2]); ok {
				steps = append(steps, shellSteps(dir, inner)...)
			}
		case checkRun(program, words[1:]):
			steps = append(steps, shellStep{kind: "check", dir: dir})
		}
	}
	return steps
}

// checkRun says a program runs a check — a test, a linter, a type check or a build — by its name
// and its first words, never by a word anywhere in the line: "rg test" searches, it does not test.
func checkRun(program string, args []string) bool {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch program {
	case "go", "cargo", "swift":
		return sub == "test" || sub == "vet" || sub == "build" || sub == "check" || sub == "clippy"
	case "pnpm", "npm", "yarn", "bun", "npx", "pnpx", "bunx":
		if installs[sub] {
			return false
		}
		return slices.ContainsFunc(args, checkScript.MatchString)
	case "make", "just":
		return checkScript.MatchString(sub)
	case "python", "python3":
		return sub == "-m" && len(args) > 1 && (args[1] == "pytest" || args[1] == "unittest" || args[1] == "mypy")
	case "pytest", "jest", "vitest", "eslint", "tsc", "golangci-lint", "staticcheck", "ruff", "mypy", "shellcheck", "playwright":
		return true
	}
	return strings.HasSuffix(program, ".sh") && checkLike.MatchString(program)
}

// gitStep reads one git call: its global options, -C among them, then the subcommand.
func gitStep(words []string, dir string, known func(string) (string, bool), at func(string) string) (shellStep, bool) {
	for len(words) > 0 && strings.HasPrefix(words[0], "-") {
		switch {
		case words[0] == "-C" && len(words) > 1:
			if path, ok := known(words[1]); ok {
				dir = at(path)
			} else {
				dir = ""
			}
			words = words[2:]
		case words[0] == "-c" && len(words) > 1:
			words = words[2:]
		default:
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return shellStep{}, false
	}
	step := shellStep{kind: words[0], dir: dir}
	for _, w := range words[1:] {
		v, ok := known(w)
		if !ok {
			v = "$?"
		}
		step.args = append(step.args, v)
	}
	return step, true
}

// reflogEntry is one move of a ref: a commit, an amend, a merge, a push, a reset.
type reflogEntry struct {
	hash, ref, action, subject string
	at                         time.Time
}

type reflog struct {
	entries  []reflogEntry
	oldest   time.Time       // before it, git no longer remembers
	branched map[string]bool // commits a branch entry holds; their HEAD entries repeat them
}

// gitCache keeps each repository's reflog and remote for one build: many conversations share one.
type gitCache struct {
	reflogs map[string]*reflog
	origins map[string]string
	roots   map[string]string
	tracked map[string]map[string]bool
}

func newGitCache() *gitCache {
	return &gitCache{reflogs: map[string]*reflog{}, origins: map[string]string{}, roots: map[string]string{}, tracked: map[string]map[string]bool{}}
}

// root is the repository holding a path, through its nearest folder that still exists.
func (g *gitCache) root(path string) string {
	if path == "" {
		return ""
	}
	if root, ok := g.roots[path]; ok {
		return root
	}
	root := worklog.RepoRoot(nearestFolder(path))
	g.roots[path] = root
	return root
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

// reflog is every ref's history, newest first within each ref, from git's own record.
func (g *gitCache) reflog(root string) *reflog {
	if log, ok := g.reflogs[root]; ok {
		return log
	}
	log := &reflog{branched: map[string]bool{}}
	out, _ := worklog.Git(root, 5*time.Second, "reflog", "show", "--all", "--date=unix", "--format=%H%x09%gD%x09%gs%x09%s")
	for _, line := range strings.Split(out, "\n") {
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) < 4 {
			continue
		}
		ref, stamp, ok := strings.Cut(strings.TrimSuffix(fields[1], "}"), "@{")
		seconds, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil {
			continue
		}
		e := reflogEntry{hash: fields[0], ref: ref, action: fields[2], subject: fields[3], at: time.Unix(seconds, 0)}
		log.entries = append(log.entries, e)
		if log.oldest.IsZero() || e.at.Before(log.oldest) {
			log.oldest = e.at
		}
		if strings.HasPrefix(ref, "refs/heads/") {
			log.branched[e.hash] = true
		}
	}
	g.reflogs[root] = log
	return log
}

// origin is the repository's origin URL, or "" when it has no remote.
func (g *gitCache) origin(root string) string {
	if url, ok := g.origins[root]; ok {
		return url
	}
	url, _ := worklog.Git(root, gitLimit, "remote", "get-url", "origin")
	g.origins[root] = url
	return url
}

// newCommit is an action that left a new commit: commit and amend, a merge that was not a
// fast-forward, cherry-pick and revert. A rebase only copies commits made before.
var newCommit = regexp.MustCompile(`^(commit( \([a-z]+\))?|cherry-pick|revert|am): |Merge made by`)

// headRef is a HEAD entry, of the main folder or of a linked worktree.
func headRef(ref string) bool {
	return ref == "HEAD" || strings.HasPrefix(ref, "worktrees/") && strings.HasSuffix(ref, "/HEAD")
}

// gitEvent is a commit made or a branch pushed, as git recorded it, or as the output showed when git
// no longer can.
type gitEvent struct {
	kind    string // commit or push
	root    string // "" when the folder is gone
	repo    string // the repository's name
	origin  string
	hash    string
	subject string
	ref     string // the branch, or remote/branch for a push
	at      time.Time
	source  string
	asked   string // the person's request the step followed
}

var (
	pushTo    = regexp.MustCompile(`(?m)^To (\S+)$`)
	pushMoved = regexp.MustCompile(`(?m)^\s*[ +*]\s*(?:([0-9a-f]{7,40})\.\.\.?([0-9a-f]{7,40})|\[new branch\]|\[new tag\])\s+(\S+)\s+->\s+(\S+)`)
)

// readGit fills in the session's git work: for each command that could make commits or move remote
// branches, its repositories' reflog entries while it ran, and the repositories it copied files
// between. Where git cannot say, the folder gone or its reflog expired, what the output showed stands.
func (g *gitCache) readGit(s *session) {
	s.events, s.copies = nil, nil
	// A step whose folder is hidden or gone may have run in a repository the conversation worked in.
	var worked []string
	for _, path := range append([]string{s.cwd}, s.files...) {
		if root := g.root(resolveDir(s.cwd, path)); root != "" && !slices.Contains(worked, root) {
			worked = append(worked, root)
		}
	}
	var carried [][2]string // copies through a folder git does not track, by the repository they carried
	seen := map[string]bool{}
	add := func(e gitEvent) {
		if e.root != "" {
			e.repo, e.origin = RepoName(e.root), g.origin(e.root)
		}
		key := strings.Join([]string{e.kind, e.repo, e.hash, e.ref}, "\x00")
		if e.hash == "" {
			key += "\x00" + e.subject
		}
		if !seen[key] {
			seen[key] = true
			s.events = append(s.events, e)
		}
	}
	for _, c := range s.commands {
		var roots, gone []string
		commits, pushes, hidden := false, false, false
		remote := "origin"
		for _, step := range shellSteps(resolveDir(s.cwd, c.dir), c.text) {
			if step.kind == "copy" {
				to := g.root(step.to)
				for _, from := range step.from {
					src := g.root(from)
					if src == "" && to != "" && !scratchRepo(mainRoot(to)) {
						if src = g.carried(worked, to); src != "" {
							carried = append(carried, [2]string{RepoName(src), RepoName(to)})
						}
						continue
					}
					if src == "" || to == "" || scratchRepo(mainRoot(src)) || scratchRepo(mainRoot(to)) {
						continue
					}
					if pair := [2]string{RepoName(src), RepoName(to)}; pair[0] != pair[1] && !slices.Contains(s.copies, pair) {
						s.copies = append(s.copies, pair)
					}
				}
				continue
			}
			if !step.writesHistory() {
				continue
			}
			commits = commits || step.kind != "push"
			if step.kind == "push" {
				pushes = true
				if len(step.args) > 0 && !strings.HasPrefix(step.args[0], "-") && !strings.Contains(step.args[0], "/") {
					remote = step.args[0]
				}
			}
			switch root := g.root(step.dir); {
			case step.dir == "":
				hidden = true
			case root == "":
				gone = append(gone, step.dir)
			case !slices.Contains(roots, root):
				roots = append(roots, root)
			}
		}
		if !commits && !pushes {
			continue
		}
		from, to := c.at.Add(-3*time.Second), c.done.Add(3*time.Second)
		if c.done.IsZero() {
			to = c.at.Add(5 * time.Minute)
		}
		subjects := literalSubjects(c.text)
		asked := askedBefore(s.requests, c.at)
		named := commitsMade(c.text, c.output)
		candidates := roots
		if hidden || len(gone) > 0 {
			for _, root := range worked {
				if !slices.Contains(candidates, root) {
					candidates = append(candidates, root)
				}
			}
		}
		committed, pushed, covered := false, false, false // git answered for this command
		for _, root := range candidates {
			// In a repository the command did not name, only a commit its output names counts.
			strict := !slices.Contains(roots, root)
			log := g.reflog(root)
			for _, e := range log.entries {
				if e.at.Before(from) || e.at.After(to) {
					continue
				}
				switch {
				case commits && newCommit.MatchString(e.action) && (strings.HasPrefix(e.ref, "refs/heads/") || headRef(e.ref) && !log.branched[e.hash]):
					// A literal -m subject guards against another session's commit in the same seconds.
					if len(subjects) > 0 && strings.HasPrefix(e.action, "commit") && !slices.Contains(subjects, e.subject) || strict && !namesCommit(named, e) {
						continue
					}
					add(gitEvent{kind: "commit", root: root, hash: e.hash, subject: e.subject, ref: strings.TrimPrefix(e.ref, "refs/heads/"), at: e.at, source: c.source, asked: asked})
					committed = true
				case pushes && !strict && e.action == "update by push" && strings.HasPrefix(e.ref, "refs/remotes/"):
					add(gitEvent{kind: "push", root: root, hash: e.hash, ref: strings.TrimPrefix(e.ref, "refs/remotes/"), at: e.at, source: c.source, asked: asked})
					pushed = true
				}
			}
			// A reflog older than the command kept what it committed here, unless the branch it was
			// on has been deleted since, its reflog with it.
			if !strict && !log.oldest.IsZero() && !log.oldest.After(from) {
				covered = true
			}
		}
		if commits && !committed {
			// Then what git's output named, or else the commits the command spelled out: history holds
			// one by its words and time, or the command succeeded whole in a folder git kept no record
			// of then, and git did not say there was nothing to commit.
			unsure := c.result != "성공" || hidden || covered || strings.Contains(c.output, "nothing to commit") || strings.Contains(c.output, "nothing added to commit") || strings.Contains(c.output, "no changes added to commit")
			if len(named) == 0 {
				for _, subject := range subjects {
					named = append(named, madeCommit{subject: subject})
				}
			}
			for _, made := range named {
				e := gitEvent{kind: "commit", hash: made.hash, subject: made.subject, at: c.at, source: c.source, asked: asked}
				if made.hash != "" {
					e.root = g.holding(candidates, made.hash)
				} else if e.root, e.hash = g.authored(candidates, made.subject, from, to); e.root == "" && unsure {
					continue
				}
				if e.root == "" && len(roots) > 0 {
					e.root = roots[0]
				}
				if e.root == "" {
					// The folder is gone and no repository here has the commit: its folder names it.
					// A hidden folder names nothing, and is often a scratch repository.
					if hidden || len(gone) == 0 || scratchRepo(gone[0]) {
						continue
					}
					e.repo = filepath.Base(gone[0])
				}
				add(e)
			}
		}
		if pushes && !pushed {
			if url := pushTo.FindStringSubmatch(c.output); url != nil {
				for _, m := range pushMoved.FindAllStringSubmatch(c.output, -1) {
					e := gitEvent{kind: "push", origin: url[1], hash: m[2], ref: remote + "/" + m[4], at: c.at, source: c.source, asked: asked}
					if e.root = g.withOrigin(append(roots, worked...), url[1]); e.root == "" {
						e.repo = originRepo(url[1])
					}
					add(e)
				}
			}
		}
	}
	// A scratch repository, a test fixture under a temporary folder with no remote, is not work.
	s.events = slices.DeleteFunc(s.events, func(e gitEvent) bool {
		return e.root != "" && scratchRepo(mainRoot(e.root)) && e.origin == ""
	})
	// Such a copy relates the two when the conversation then pushed the one it filled: that publishes
	// the other's work, where filling one never pushed may only put a copy back.
	for _, pair := range carried {
		pushed := slices.ContainsFunc(s.events, func(e gitEvent) bool { return e.kind == "push" && e.repo == pair[1] })
		if pushed && !slices.Contains(s.copies, pair) {
			s.copies = append(s.copies, pair)
		}
	}
}

// holding is the first repository that has a commit.
func (g *gitCache) holding(roots []string, hash string) string {
	for _, root := range roots {
		if _, ok := worklog.Git(root, gitLimit, "cat-file", "-e", hash+"^{commit}"); ok {
			return root
		}
	}
	return ""
}

// authored is the commit in one of roots with this subject, written while the command ran: the
// commit a quiet command made, in its current form if a rebase or an amend has rewritten it since.
func (g *gitCache) authored(roots []string, subject string, from, to time.Time) (string, string) {
	for _, root := range roots {
		out, _ := worklog.Git(root, gitLimit, "log", "--all", "--fixed-strings", "--grep="+subject, "--format=%H%x09%at%x09%s", "-50")
		for _, line := range strings.Split(out, "\n") {
			fields := strings.SplitN(line, "\t", 3)
			if len(fields) != 3 || fields[2] != subject {
				continue
			}
			seconds, _ := strconv.ParseInt(fields[1], 10, 64)
			if at := time.Unix(seconds, 0); !at.Before(from.Add(-time.Minute)) && !at.After(to.Add(time.Minute)) {
				return root, fields[0]
			}
		}
	}
	return "", ""
}

// carried is the repository among worked that a copy into to from a folder git does not track, a
// staging or a backup folder, carried over: the two keep most of the same files.
func (g *gitCache) carried(worked []string, to string) string {
	for _, root := range worked {
		if RepoName(root) == RepoName(to) || scratchRepo(mainRoot(root)) {
			continue
		}
		a, b := g.files(root), g.files(to)
		common := 0
		for path := range a {
			if b[path] {
				common++
			}
		}
		if common >= 5 && 2*common >= min(len(a), len(b)) {
			return root
		}
	}
	return ""
}

// files are the paths git tracks in a repository.
func (g *gitCache) files(root string) map[string]bool {
	if paths, ok := g.tracked[root]; ok {
		return paths
	}
	out, _ := worklog.Git(root, gitLimit, "ls-files")
	paths := map[string]bool{}
	for _, path := range strings.Split(out, "\n") {
		if path != "" {
			paths[path] = true
		}
	}
	g.tracked[root] = paths
	return paths
}

// withOrigin is the first repository whose origin is url.
func (g *gitCache) withOrigin(roots []string, url string) string {
	for _, root := range roots {
		if g.origin(root) == url {
			return root
		}
	}
	return ""
}

// namesCommit says the output or the command named this commit, by hash or by subject.
func namesCommit(named []madeCommit, e reflogEntry) bool {
	for _, n := range named {
		if n.hash != "" && strings.HasPrefix(e.hash, n.hash) || n.hash == "" && n.subject == e.subject {
			return true
		}
	}
	return false
}

// originRepo is the repository name in a remote URL.
func originRepo(url string) string {
	return strings.TrimSuffix(filepath.Base(strings.TrimRight(url, "/")), ".git")
}

// literalSubjects are the commit subjects a command spells out; one built from a variable says nothing.
func literalSubjects(command string) []string {
	var out []string
	for _, s := range commitSubjects(command) {
		if s != "" && !strings.ContainsAny(s, "$`") {
			out = append(out, s)
		}
	}
	return out
}

// askedBefore is the person's latest request before a step ran: the words that led to it.
func askedBefore(requests []worklog.Request, at time.Time) string {
	for i := len(requests) - 1; i >= 0; i-- {
		if !requests[i].At.After(at) {
			return cut(strings.Join(strings.Fields(mask(requests[i].Text)), " "), 160)
		}
	}
	return ""
}
