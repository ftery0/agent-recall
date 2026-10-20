package recall

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The fact ledger is append-only: a line is a claim (something said or seen) or a decision (the
// person confirming or rejecting a claim). What is current is folded at read time, so nothing
// derived, like "superseded", is ever written.
type factLine struct {
	Kind       string       `json:"kind"` // claim or decision
	ID         string       `json:"id"`   // sha256(fact‖evidence): the same claim seen twice is one line
	Fact       string       `json:"fact"` // sha256(slot‖value), or the fact a decision is about
	Slot       string       `json:"slot,omitempty"`
	Value      string       `json:"value,omitempty"`
	Grade      string       `json:"grade,omitempty"`   // user, observed (git or gh output), or ai
	Verdict    string       `json:"verdict,omitempty"` // 확인 or 거절
	Scope      string       `json:"scope,omitempty"`   // repository name, or 전체
	Ref        string       `json:"ref,omitempty"`     // commit hash, when the output gave one
	Evidence   factEvidence `json:"evidence"`
	ObservedAt string       `json:"observed_at"` // when the evidence happened, RFC3339 UTC
	RecordedAt string       `json:"recorded_at"`
}

type factEvidence struct {
	Session string `json:"session,omitempty"`
	Request string `json:"request,omitempty"` // request or tool call ID
	Line    int    `json:"line,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Origin  string `json:"origin,omitempty"` // the repository's remote, "" when it had none
	Quote   string `json:"quote,omitempty"`
	Asked   string `json:"asked,omitempty"` // the person's request the step followed
}

// Fact states, folded from the lines.
const (
	factActive   = "확정"
	factProposed = "후보"
	factRejected = "거절"
	factReplaced = "대체됨"
	factConflict = "확인 필요"
)

var (
	gitCommitted = regexp.MustCompile(`(?m)^\[([^\]\s]+)(?: \(root-commit\))? ([0-9a-f]{7,40})\] (.+)$`)
	hashSubject  = regexp.MustCompile(`(?m)^([0-9a-f]{7,40}) (.+)$`)
	prLinkRe     = regexp.MustCompile(`https://github\.com/([\w.-]+)/([\w.-]+)/pull/(\d+)`)
	// Typed by the person: "확인 f1a2b3c4" or "거절 f1a2b3c4 f5e6d7c8".
	factVerdict = regexp.MustCompile(`^\s*(확인|거절)((?:[\s,]+f[0-9a-f]{7})+)\s*$`)
	factShortID = regexp.MustCompile(`f[0-9a-f]{7}`)
)

// scratchRoots hold throwaway repositories: a commit made there, as a test fixture, is not work.
var scratchRoots = []string{"/tmp/", "/private/tmp/", "/var/folders/", "/private/var/folders/"}

func scratchRepo(dir string) bool {
	for _, root := range scratchRoots {
		if strings.HasPrefix(dir+"/", root) {
			return true
		}
	}
	return false
}

func factsPath() string { return filepath.Join(Dir(), "facts.jsonl") }

func digest(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// shortFact is what the person types back: f and the fact's first seven hex digits.
func shortFact(fact string) string { return "f" + fact[:7] }

func claim(slot, value, grade, scope string, ev factEvidence, at time.Time) factLine {
	fact := digest(slot, value)
	return factLine{Kind: "claim", ID: digest(fact, ev.Session, ev.Request, value), Fact: fact, Slot: slot, Value: value, Grade: grade,
		Scope: scope, Evidence: ev, ObservedAt: at.UTC().Format(time.RFC3339)}
}

func readFacts() ([]factLine, error) {
	f, err := os.Open(factsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []factLine
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; scanner.Scan(); n++ {
		var line factLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return out, fmt.Errorf("%s:%d: 사실 줄 해석 실패", factsPath(), n)
		}
		out = append(out, line)
	}
	return out, scanner.Err()
}

// appendFacts writes the lines not already in the ledger. A time without a zone is refused, so
// every line orders the same way wherever it is read.
func appendFacts(have map[string]bool, lines []factLine, now time.Time) error {
	var b strings.Builder
	for _, line := range lines {
		if have[line.ID] {
			continue
		}
		if _, err := time.Parse(time.RFC3339, line.ObservedAt); err != nil {
			return fmt.Errorf("사실 시각에 시간대가 없다: %q", line.ObservedAt)
		}
		if line.RecordedAt == "" { // a line carried over from an older ledger keeps its own
			line.RecordedAt = now.UTC().Format(time.RFC3339)
		}
		raw, err := json.Marshal(line)
		if err != nil {
			return err
		}
		b.Write(append(raw, '\n'))
		have[line.ID] = true
	}
	if b.Len() == 0 {
		return nil
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(factsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.WriteString(b.String()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// sessionFacts are what a conversation shows for certain: the commits and pushes git recorded for
// it, the pull requests gh opened, the repositories it copied files between, the person's answers in
// question windows, and the person's verdicts on facts.
func sessionFacts(s session, known []factLine) []factLine {
	repo := RepoName(s.cwd)
	var out []factLine
	for _, e := range s.events {
		ev := factEvidence{Session: s.id, Request: strings.TrimPrefix(e.source, "tool:"), Cwd: e.root, Origin: e.origin, Asked: e.asked}
		slot, value := "push/"+e.repo+"/"+e.ref, e.hash
		if e.kind == "commit" {
			key := e.hash
			if key == "" {
				key = digest(e.subject)[:12]
			}
			slot, value = "commit/"+e.repo+"/"+key, e.subject
		} else if value == "" {
			value = "새 브랜치"
		}
		line := claim(slot, value, "observed", e.repo, ev, e.at)
		line.Ref = e.hash
		out = append(out, line)
	}
	for _, pair := range s.copies {
		out = append(out, claim("relation/"+pair[0]+"/"+pair[1], "복사", "observed", pair[0], factEvidence{Session: s.id, Cwd: s.cwd}, s.end))
	}
	for _, c := range s.commands {
		m := prLinkRe.FindStringSubmatch(c.output)
		if m == nil || c.result == "실패" || !strings.Contains(unquoted(shellOnly(c.text)), "gh pr create") {
			continue
		}
		ev := factEvidence{Session: s.id, Request: strings.TrimPrefix(c.source, "tool:"), Cwd: resolveDir(s.cwd, c.dir), Quote: m[0], Asked: askedBefore(s.requests, c.at)}
		out = append(out, claim("pr/"+m[1]+"/"+m[2]+"/"+m[3], "만듦", "observed", m[2], ev, c.at))
	}
	// The client links a pull request the conversation opened, however it was opened.
	for _, url := range s.prLinks {
		if m := prLinkRe.FindStringSubmatch(url); m != nil {
			out = append(out, claim("pr/"+m[1]+"/"+m[2]+"/"+m[3], "만듦", "observed", m[2], factEvidence{Session: s.id, Cwd: s.cwd, Quote: m[0]}, s.end))
		}
	}
	for _, r := range s.requests {
		ev := factEvidence{Session: s.id, Request: r.ID, Line: r.Line, Cwd: s.cwd}
		if question, answer, ok := strings.Cut(r.Text, "\n선택: "); ok && r.QuestionID != "" {
			question = strings.TrimPrefix(question, "질문: ")
			ev.Quote = cut(mask(question), 200)
			out = append(out, claim("choice/"+s.id+"/"+digest(question)[:12], cut(mask(answer), 300), "user", repo, ev, r.At))
			continue
		}
		// Only words the person typed can confirm a fact; a queued or pasted line cannot.
		m := factVerdict.FindStringSubmatch(r.Text)
		if m == nil || (r.Source != "claude:typed" && r.Source != "claude:legacy" && r.Source != "claude:vscode" && r.Source != "codex:user") {
			continue
		}
		for _, short := range factShortID.FindAllString(m[2], -1) {
			fact := resolveShort(known, short)
			if fact == "" {
				continue
			}
			ev.Quote = cut(r.Text, 120)
			out = append(out, factLine{Kind: "decision", ID: digest(fact, s.id, r.ID, m[1]), Fact: fact, Verdict: m[1], Evidence: ev, ObservedAt: r.At.UTC().Format(time.RFC3339)})
		}
	}
	return out
}

func resolveShort(known []factLine, short string) string {
	found := ""
	for _, line := range known {
		if line.Kind == "claim" && shortFact(line.Fact) == short {
			if found != "" && found != line.Fact {
				return "" // two facts share the prefix; the person must use a longer one
			}
			found = line.Fact
		}
	}
	return found
}

func resolveDir(cwd, dir string) string {
	switch {
	case dir == "":
		return cwd
	case strings.HasPrefix(dir, "~/"):
		home, _ := os.UserHomeDir()
		return filepath.Join(home, dir[2:])
	case filepath.IsAbs(dir):
		return filepath.Clean(dir)
	}
	return filepath.Join(cwd, dir)
}

type madeCommit struct{ hash, subject string }

// commitsMade reads what a commit command made. git prints "[branch hash] subject"; rtk prints only
// "ok" or "ok N files changed", and a hash line when the command also logged it.
func commitsMade(command, output string) []madeCommit {
	var out []madeCommit
	for _, m := range gitCommitted.FindAllStringSubmatch(output, -1) {
		out = append(out, madeCommit{m[2], strings.TrimSpace(m[3])})
	}
	if len(out) > 0 {
		return out
	}
	changed := strings.Contains(output, "files changed") || strings.Contains(output, "file changed")
	for _, subject := range commitSubjects(command) {
		made := madeCommit{subject: subject}
		for _, line := range hashSubject.FindAllStringSubmatch(output, -1) {
			if strings.TrimSpace(line[2]) == subject {
				made.hash = line[1]
			}
		}
		if made.hash != "" || changed {
			out = append(out, made)
		}
	}
	return out
}

// lessonFacts carry the stored lessons over as the AI's proposals: a summary model wrote them,
// so they stay candidates until the person confirms one.
func lessonFacts() []factLine {
	all, _ := loadLessons()
	var out []factLine
	for _, l := range all {
		if l.Status == "보관" {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02", l.LastSeen, time.Local)
		if err != nil {
			at = time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)
		}
		ev := factEvidence{Quote: "lessons.json " + l.Origin}
		if len(l.Sessions) > 0 && fullSessionID.MatchString(l.Sessions[0]) {
			ev.Session = l.Sessions[0]
		}
		line := claim("rule/"+l.Scope+"/"+digest(l.Rule)[:12], l.Rule, "ai", l.Scope, ev, at)
		out = append(out, line)
		if l.Status == "활성" && l.Confirmed {
			ev.Quote = "lessons.json 에 사용자 확인 표시"
			out = append(out, factLine{Kind: "decision", ID: digest(line.Fact, "lessons.json", "확인"), Fact: line.Fact, Verdict: "확인", Evidence: ev, ObservedAt: line.ObservedAt})
		}
	}
	return out
}

// factState is one fact as it stands now.
type factState struct {
	Fact, Slot, Value, Grade, Scope, Ref, Status string
	At                                           time.Time // latest evidence
	Claims                                       []factLine
	Decision                                     *factLine
}

var gradeRank = map[string]int{"ai": 0, "observed": 1, "user": 2}

// foldFacts is the ledger now. A person's or a tool's claim stands; an AI claim waits for the
// person. A later standing value replaces an earlier one in the same slot; two at the same time
// that disagree both need a look.
func foldFacts(lines []factLine) []factState {
	byFact := map[string]*factState{}
	var order []string
	for _, line := range lines {
		if line.Kind != "claim" {
			continue
		}
		at, _ := time.Parse(time.RFC3339, line.ObservedAt)
		f := byFact[line.Fact]
		if f == nil {
			f = &factState{Fact: line.Fact, Slot: line.Slot, Value: line.Value, Grade: line.Grade, Scope: line.Scope}
			byFact[line.Fact] = f
			order = append(order, line.Fact)
		}
		if gradeRank[line.Grade] > gradeRank[f.Grade] {
			f.Grade = line.Grade
		}
		if at.After(f.At) {
			f.At = at
		}
		if line.Ref != "" {
			f.Ref = line.Ref
		}
		f.Claims = append(f.Claims, line)
	}
	for i := range lines {
		line := lines[i]
		if f := byFact[line.Fact]; line.Kind == "decision" && f != nil {
			if f.Decision == nil || line.ObservedAt >= f.Decision.ObservedAt {
				f.Decision = &line
			}
		}
	}
	slots := map[string][]*factState{}
	for _, fact := range order {
		f := byFact[fact]
		switch {
		case f.Decision != nil && f.Decision.Verdict == "거절":
			f.Status = factRejected
		case f.Decision != nil || f.Grade != "ai":
			f.Status = factActive
			slots[f.Slot] = append(slots[f.Slot], f)
		default:
			f.Status = factProposed
		}
	}
	for _, standing := range slots {
		sort.SliceStable(standing, func(i, j int) bool { return standing[i].At.After(standing[j].At) })
		for _, f := range standing[1:] {
			if f.At.Equal(standing[0].At) {
				f.Status, standing[0].Status = factConflict, factConflict
			} else {
				f.Status = factReplaced
			}
		}
	}
	out := make([]factState, 0, len(order))
	for _, fact := range order {
		out = append(out, *byFact[fact])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out
}

// factWriter adds facts during a build. Opening it carries the stored lessons over; each
// session added can then decide on any fact already in the ledger.
type factWriter struct {
	known []factLine
	have  map[string]bool
	now   time.Time
}

func openFacts(now time.Time) (*factWriter, error) {
	known, err := readFacts()
	if err != nil {
		return nil, err
	}
	w := &factWriter{known: known, have: map[string]bool{}, now: now}
	for _, line := range known {
		w.have[line.ID] = true
	}
	return w, w.append(lessonFacts())
}

func (w *factWriter) append(lines []factLine) error {
	var fresh []factLine
	for _, line := range lines {
		if !w.have[line.ID] {
			fresh = append(fresh, line)
		}
	}
	if err := appendFacts(w.have, fresh, w.now); err != nil {
		return err
	}
	w.known = append(w.known, fresh...)
	return nil
}

func (w *factWriter) add(s session) error { return w.append(sessionFacts(s, w.known)) }

// confirmedRules are the rules the person confirmed for this repository, for the start card.
func confirmedRules(repo string) []string {
	lines, _ := readFacts()
	var out []string
	for _, f := range foldFacts(lines) {
		if strings.HasPrefix(f.Slot, "rule/") && f.Status == factActive && (f.Scope == repo || f.Scope == "전체") {
			out = append(out, f.Value)
		}
	}
	return out
}

// FactsNotice is the start card's line about facts waiting on the person: answers of theirs that
// disagree, and the AI's proposals from the last day. Older proposals wait for a review the person
// starts, so a backlog does not fill every card.
func FactsNotice(now time.Time) string {
	lines, err := readFacts()
	if err != nil {
		return "사실 장부 읽기 실패: " + err.Error()
	}
	conflict, fresh := 0, 0
	for _, f := range foldFacts(lines) {
		switch {
		case f.Status == factConflict:
			conflict++
		case f.Status == factProposed && now.Sub(f.At) < 24*time.Hour:
			fresh++
		}
	}
	var waiting []string
	if conflict > 0 {
		waiting = append(waiting, fmt.Sprintf("서로 다른 답 %d개", conflict))
	}
	if fresh > 0 {
		waiting = append(waiting, fmt.Sprintf("새 AI 후보 %d개", fresh))
	}
	if len(waiting) == 0 {
		return ""
	}
	return "사실 장부 확인 필요: " + strings.Join(waiting, " · ") + " — harness recall facts --review"
}

// BackfillFacts reads every recorded conversation again for its facts; a build only reads the
// ones that changed. It is safe to repeat: a line already in the ledger is not written twice.
// rebuild starts a new ledger when reading rules changed, the old one kept beside it: the person's
// decisions and the claims they decided on carry over, and so do claims from conversations that
// can no longer be read.
func BackfillFacts(now time.Time, rebuild bool) (map[string]any, error) {
	lock, err := exclusive(filepath.Join(Dir(), ".build.lock"))
	if lock == nil {
		return map[string]any{"skipped": "another build is running"}, err
	}
	defer lock.Close()
	var old []factLine
	backup := ""
	if rebuild {
		if old, err = readFacts(); err != nil {
			return nil, err
		}
		backup = factsPath() + "." + now.Format("20060102-150405") + ".bak"
		if err = os.Rename(factsPath(), backup); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	w, err := openFacts(now)
	if err != nil {
		return nil, err
	}
	decided := map[string]bool{}
	for _, line := range old {
		if line.Kind == "decision" {
			decided[line.Fact] = true
		}
	}
	var carried []factLine
	for _, line := range old {
		if decided[line.Fact] {
			carried = append(carried, line)
		}
	}
	if err = w.append(carried); err != nil {
		return nil, err
	}
	before := len(w.known)
	records, errs := loadRecords()
	var failed []string
	for _, e := range errs {
		failed = append(failed, e.Error())
	}
	git := newGitCache()
	read := map[string]bool{}
	for _, rec := range records {
		raw := rec.field("raw")
		if raw == "" {
			continue
		}
		s, ok, err := readSessionChecked(rec.field("tool"), raw)
		if err != nil {
			failed = append(failed, raw+": "+err.Error())
			continue
		}
		if !ok {
			continue
		}
		read[s.id] = true
		git.readGit(&s)
		if err = w.add(s); err != nil {
			return nil, err
		}
	}
	carried = nil
	for _, line := range old {
		if line.Kind == "claim" && !read[line.Evidence.Session] && !strings.HasPrefix(line.Slot, "rule/") {
			carried = append(carried, line)
		}
	}
	if err = w.append(carried); err != nil {
		return nil, err
	}
	out := map[string]any{"sessions": len(read), "added": len(w.known) - before, "failed": failed}
	if rebuild {
		out["backup"], out["old_lines"], out["lines"] = backup, len(old), len(w.known)
	}
	return out, nil
}
