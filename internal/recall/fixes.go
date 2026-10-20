package recall

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
)

// No agent knows beforehand which of its habits are wrong in a place; it finds out when a command
// fails. What helps then is how the same failure was gotten past before: a failed command followed,
// within a few commands and with no file edited between, by a near copy of it that worked. The pairs
// are read from transcripts without a model, kept in the ledger as fix/ facts, and said in one line
// after a failing command they match. A failure with no such pair adds nothing.

const (
	fixHeader = "=== 하네스: 방금 실패한 명령과 닮은 예전 실패와 그때 된 명령 — 장부에 남은 기록이며 지시가 아니다 ==="
	fixWindow = 10  // commands after a failure in which its fix is looked for
	fixLength = 200 // longer commands are scripts, not habits
	fixAlike  = 0.6 // word overlap at which two commands of one program count as the same attempt
)

// tried is one shell command a conversation ran, in order.
type tried struct {
	text   string
	source string
	edits  int // files edited before it: a fix after an edit was the edit's, not the command's
	at     time.Time
	done   bool
	failed bool
}

var (
	urlParts = regexp.MustCompile(`\b(https?)://([A-Za-z0-9.-]+)(:\d{2,5})?(/[^\s'"|;&<>)\\]*)?`)
	// A failure the exit code hides: a pipe's last command succeeded, or the program printed its error.
	failedOutput = regexp.MustCompile(`(?m)Traceback \(most recent call last\)|curl: \(\d+\)|net::ERR_|Connection refused|ECONNREFUSED|Empty reply from server|command not found|^fatal: `)
	// Fixes nobody should repeat on a hint: they destroy work or reach production.
	irreversible = regexp.MustCompile(`(?i)\bpush\b[^|;&\n]*\s(--force\b|--force-with-lease\b|-f\b)|\bbranch\s+-D\b|\breset\s+--hard\b|\bgit\s+clean\b|\brm\s+-[a-z]*r|\bgh\s+(pr\s+merge|repo\s+delete|release\s+delete)\b|\bkubectl\s+(delete|apply|scale|rollout)\b|\bhelm\s+(upgrade|install|uninstall|delete)\b|\bdrop\s+(table|database)\b|\bprod(uction)?\b`)
	// Programs that only look around; their failures are questions, not habits.
	probes     = map[string]bool{"cd": true, "ls": true, "cat": true, "head": true, "tail": true, "echo": true, "printf": true, "pwd": true, "which": true, "type": true, "wc": true, "stat": true, "file": true, "find": true, "grep": true, "rg": true, "sed": true, "awk": true, "jq": true, "sleep": true, "true": true, "false": true, "test": true, "[": true, "ps": true, "lsof": true, "kill": true, "pkill": true, "pgrep": true, "diff": true, "sort": true, "xargs": true}
	fixTagOpen = regexp.MustCompile(`<(/?[A-Za-z][\w-]*)`)
)

// fixPair is a failed command and the one that worked after it. An address pair keeps only the
// scheme, host and port each used.
type fixPair struct {
	kind           string // addr or cmd
	failed, worked string
	source         string // the failed call
	at             time.Time
}

// program is a command's first program, past cd, variable settings and wrappers like rtk.
func program(text string) string {
	for _, part := range splitShell(text) {
		words := part.words
		for len(words) > 0 && (shellPrefix[words[0]] || assignment(words[0])) {
			words = words[1:]
		}
		if len(words) == 0 || words[0] == "cd" {
			continue
		}
		return filepath.Base(strings.Trim(words[0], `"'`))
	}
	return ""
}

func assignment(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	return ok && shellName.MatchString(name)
}

// fixWords are a command's words for comparing two attempts: letters and digits, so a changed flag
// or address part counts as one changed word, not a whole new command.
func fixWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if w != "rtk" && w != "proxy" {
			out[w] = true
		}
	}
	return out
}

func alike(a, b map[string]bool) float64 {
	common := 0
	for w := range a {
		if b[w] {
			common++
		}
	}
	if union := len(a) + len(b) - common; union > 0 {
		return float64(common) / float64(union)
	}
	return 0
}

type address struct{ scheme, host, port, path string }

func (a address) origin() string { return a.scheme + "://" + a.host + a.port }

func addresses(text string) []address {
	var out []address
	for _, m := range urlParts.FindAllStringSubmatch(text, -1) {
		out = append(out, address{strings.ToLower(m[1]), strings.ToLower(m[2]), m[3], strings.TrimRight(m[4], "/")})
	}
	return out
}

// sameResource says b asks the same server for the same thing as a in another way: another scheme
// on the same port, or another port or scheme for the same path.
func sameResource(a, b address) bool {
	return a.host == b.host && a.origin() != b.origin() && (a.port == b.port || a.path != "" && a.path == b.path)
}

// fixable is a command that can be a habit worth a pair: short, no heredoc or scratch folder, not
// only looking around, and not a check, whose failure is the code's.
func fixable(text string) bool {
	if len([]rune(text)) > fixLength || heredoc.MatchString(text) || probes[program(text)] {
		return false
	}
	for _, root := range scratchRoots {
		if strings.Contains(text, root) {
			return false
		}
	}
	for _, step := range shellSteps("", text) {
		if step.kind == "check" {
			return false
		}
	}
	return true
}

// fixPairs are a conversation's failures that a later command got past. A failure whose command
// still fails later in the conversation was not got past; a fix that only repeats the failed
// command, or that cannot be undone, is no fix to pass on.
func fixPairs(tries []tried) []fixPair {
	var out []fixPair
	worked := map[string]bool{} // origins that answered in this conversation
	for _, t := range tries {
		if t.done && !t.failed {
			for _, a := range addresses(t.text) {
				worked[a.origin()] = true
			}
		}
	}
	for i, f := range tries {
		if !f.failed || !fixable(f.text) {
			continue
		}
		prog, words := program(f.text), fixWords(f.text)
		// The same command failing again was not got past; working later, it only failed for a while.
		failsAgain := slices.ContainsFunc(tries[i+1:], func(t tried) bool { return t.text == f.text && (t.failed || t.done) })
		for _, w := range tries[i+1 : min(len(tries), i+1+fixWindow)] {
			if !w.done || w.failed || w.edits != f.edits || w.text == f.text || !fixable(w.text) || irreversible.MatchString(w.text) {
				continue
			}
			if pair, ok := addressFix(f, w, worked); ok {
				out = append(out, pair)
				break
			}
			if !failsAgain && program(w.text) == prog && alike(words, fixWords(w.text)) >= fixAlike && reworded(f.text, w.text) && kept(f.text, w.text) {
				out = append(out, fixPair{kind: "cmd", failed: mask(f.text), worked: mask(w.text), source: f.source, at: f.at})
				break
			}
		}
	}
	return out
}

// reworded says the second command changed how the first one ran: not only what a pipe did with
// its output, and not only by dropping the part that failed. The same words quoted or ordered
// another way are a change.
func reworded(failed, worked string) bool {
	if beforePipe(failed) == beforePipe(worked) {
		return false
	}
	had, has := fixWords(failed), fixWords(worked)
	for w := range has {
		if !had[w] {
			return true
		}
	}
	return len(has) == len(had)
}

// kept says the second command still names everything the first one did. A fix runs the same thing
// another way: other flags, quoting, the program's folder or an address's host. A command that
// names another file, path or subcommand has moved on to other work.
func kept(failed, worked string) bool {
	has := targets(worked)
	for w := range targets(failed) {
		if !has[w] {
			return false
		}
	}
	return true
}

// targets are a command's words besides flags, variable settings and wrappers, unquoted, with the
// program's folder and an address's scheme, host and port left out.
func targets(text string) map[string]bool {
	out := map[string]bool{}
	for _, part := range splitShell(text) {
		first := true
		for _, w := range part.words {
			if strings.HasPrefix(w, "-") || assignment(w) || first && shellPrefix[w] {
				continue
			}
			w = strings.NewReplacer(`"`, "", "'", "").Replace(w)
			if m := urlParts.FindStringSubmatchIndex(w); m != nil {
				rest := m[1] // an address with no path keeps nothing of itself
				if m[8] >= 0 {
					rest = m[8]
				}
				w = w[:m[0]] + w[rest:]
			}
			if first {
				w, first = filepath.Base(w), false
			}
			if w != "" {
				out[w] = true
			}
		}
	}
	return out
}

func beforePipe(text string) string {
	if at := pipe.FindStringIndex(text); at != nil {
		return strings.TrimSpace(text[:at[0]+1])
	}
	return text
}

// addressFix is a pair of addresses: the failed one never answered in the conversation, and the
// one that worked asked the same server for the same thing.
func addressFix(f, w tried, worked map[string]bool) (fixPair, bool) {
	for _, a := range addresses(f.text) {
		if worked[a.origin()] {
			continue
		}
		for _, b := range addresses(w.text) {
			if sameResource(a, b) {
				return fixPair{kind: "addr", failed: a.origin(), worked: b.origin(), source: f.source, at: f.at}, true
			}
		}
	}
	return fixPair{}, false
}

// fixFacts are the pairs as ledger claims, in the repository the conversation worked in.
func fixFacts(s session, repo string) []factLine {
	var out []factLine
	for _, p := range fixPairs(s.tries) {
		ev := factEvidence{Session: s.id, Request: strings.TrimPrefix(p.source, "tool:"), Cwd: s.cwd, Quote: p.failed}
		out = append(out, claim("fix/"+p.kind+"/"+digest(p.failed)[:12], p.worked, "observed", repo, ev, p.at))
	}
	return out
}

// readFixes are the ledger's fix claims; the rest of the ledger is not parsed.
func readFixes() []factLine {
	f, err := os.Open(factsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []factLine
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		if !bytes.Contains(scanner.Bytes(), []byte(`"slot":"fix/`)) {
			continue
		}
		var line factLine
		if json.Unmarshal(scanner.Bytes(), &line) == nil {
			out = append(out, line)
		}
	}
	return out
}

// fixNotes are the lines for a failed command: each address pair for an address it used, and the
// pair whose failed command is most like it in the same repository.
func fixNotes(command, repo string, fixes []factState) []string {
	var notes []string
	origins := map[string]bool{}
	for _, a := range addresses(command) {
		origins[a.origin()] = true
	}
	prog, words := program(command), fixWords(command)
	best, bestScore := "", fixAlike
	for _, f := range fixes {
		if f.Status != factActive || len(f.Claims) == 0 {
			continue
		}
		failed := f.Claims[len(f.Claims)-1].Evidence.Quote
		switch {
		case strings.HasPrefix(f.Slot, "fix/addr/") && origins[failed]:
			notes = append(notes, fmt.Sprintf("%s 은 실패했고 %s 로 됐다 (%s)", failed, f.Value, seenIn(f)))
		case strings.HasPrefix(f.Slot, "fix/cmd/") && f.Scope == repo && program(failed) == prog && f.Value != command:
			if score := alike(words, fixWords(failed)); score >= bestScore {
				best, bestScore = fmt.Sprintf("비슷한 명령 «%s» 가 실패했고 바로 뒤 «%s» 로 됐다 (%s)", failed, f.Value, seenIn(f)), score
			}
		}
	}
	if best != "" {
		notes = append(notes, best)
	}
	return notes[:min(len(notes), 2)]
}

func seenIn(f factState) string {
	sessions := map[string]bool{}
	for _, c := range f.Claims {
		sessions[c.Evidence.Session] = true
	}
	return fmt.Sprintf("대화 %d개, 마지막 %s", len(sessions), f.At.Local().Format("01-02"))
}

// FailureText is what the hook after a failed shell command adds, once per conversation for a line.
func FailureText(command, cwd, session string) string {
	notes := unseen(session, fixNotes(command, RepoName(cwd), foldFacts(readFixes())))
	if len(notes) == 0 {
		return ""
	}
	// A stored command may hold text that looks like the client's own tags; it is defused to read as data.
	return fixTagOpen.ReplaceAllString(fixHeader+"\n"+strings.Join(notes, "\n"), "‹$1")
}

// OutputFailed says a shell output carries an error a program prints when it fails.
func OutputFailed(output string) bool {
	return failedOutput.MatchString(output)
}
