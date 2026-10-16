package recall

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const hintLimit = 1200 // characters added to a request, about a thousand tokens

var (
	hintMonthDay = regexp.MustCompile(`(\d{1,2})월\s*(\d{1,2})일`)
	hintISODay   = regexp.MustCompile(`\b20\d\d-\d\d-\d\d\b`)
	hintTicket   = regexp.MustCompile(`(?i)\bTSK-\d+\b`)
	hintPR       = regexp.MustCompile(`(?i)\bPR\s*#?\s*(\d{2,5})\b`) // "PR 2개" is a count, not a number
	hintLeftOff  = regexp.MustCompile(`저번|지난\s?번|전에 하던|하던\s?거|하다가?\s?말`)
	hintPasted   = regexp.MustCompile(`(?s)<pasted_content[^>]*>.*?</pasted_content[^>]*>`)
	hintLogTime  = regexp.MustCompile(`^[ T]\d\d:\d\d`)
)

// HintText is what the harness looks up before the agent answers: the day, ticket, pull request
// or unfinished work a request points to. A request that points nowhere in the past gets nothing.
func HintText(prompt, cwd, session string, now time.Time) string {
	// Pasted text is someone else's words, often logs full of dates and numbers.
	prompt = hintPasted.ReplaceAllString(prompt, " ")
	repo := RepoName(cwd)
	var parts []string
	if day, ok := hintDay(prompt, now); ok {
		parts = append(parts, dayHint(day, repo))
	}
	var tickets []string
	for _, ticket := range hintTicket.FindAllString(prompt, -1) {
		if ticket = strings.ToUpper(ticket); len(tickets) < 2 && !slices.Contains(tickets, ticket) {
			tickets = append(tickets, ticket)
			parts = append(parts, findHint(ticket, ticket, 3))
		}
	}
	for _, m := range hintPR.FindAllStringSubmatch(prompt, 2) {
		parts = append(parts, prHint(cwd, m[1]))
	}
	// "저번 주" is a week, not the work left off.
	if len(parts) == 0 && hintLeftOff.MatchString(strings.NewReplacer("저번주", "", "저번 주", "").Replace(prompt)) {
		parts = append(parts, leftOffHint(repo, session))
	}
	if len(parts) == 0 {
		return ""
	}
	return cut("=== 하네스가 질문을 보고 먼저 찾은 기억 — AI 주장이 섞여 있으니 원문·git 으로 확인한다 ===\n"+strings.Join(parts, "\n"), hintLimit)
}

func hintDay(prompt string, now time.Time) (time.Time, bool) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	switch {
	case strings.Contains(prompt, "그저께") || strings.Contains(prompt, "그제"):
		return today.AddDate(0, 0, -2), true
	case strings.Contains(prompt, "어제"):
		return today.AddDate(0, 0, -1), true
	}
	for _, m := range hintISODay.FindAllStringIndex(prompt, -1) {
		before, after := prompt[:m[0]], prompt[m[1]:]
		// A log line's time, or a date in a file name or path, is not a day asked about.
		inPath := strings.HasSuffix(before, "/") || (after != "" && strings.ContainsRune("-_/", rune(after[0])))
		if inPath || hintLogTime.MatchString(after) {
			continue
		}
		day, err := parseDay(prompt[m[0]:m[1]], now)
		return day, err == nil
	}
	if m := hintMonthDay.FindStringSubmatch(prompt); m != nil {
		month, _ := strconv.Atoi(m[1])
		date, _ := strconv.Atoi(m[2])
		day, err := parseDay(fmt.Sprintf("%02d-%02d", month, date), now)
		return day, err == nil
	}
	return time.Time{}, false
}

// dayHint is the day in one line per conversation, this repository's first.
func dayHint(day time.Time, repo string) string {
	date := day.Format("2006-01-02")
	all := dayConversations(day, nil)
	if len(all) == 0 {
		return fmt.Sprintf("[%s] 그날 기록된 요청이 없다", date)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].repo == repo && all[j].repo != repo })
	lines := []string{fmt.Sprintf("[%s 대화 %d개 — 전부: harness recall day %s]", date, len(all), date)}
	for _, c := range all[:min(len(all), 5)] {
		last := c.requests[len(c.requests)-1]
		lines = append(lines, fmt.Sprintf("- %s · %s · 그날 마지막 %s %s (세션 %s)", c.repo, cut(c.title, 40), last.at.Format("15:04"), cut(last.text, 60), short(c.session)))
	}
	return strings.Join(lines, "\n")
}

func findHint(label, query string, limit int) string {
	hits := Find(query, limit)
	if len(hits) == 0 {
		return fmt.Sprintf("[%s] 찾은 기록 없음", label)
	}
	lines := []string{fmt.Sprintf("[%s — 더: harness recall find %s]", label, query)}
	for _, h := range hits {
		line := fmt.Sprintf("- %s · %s", cut(h.Title, 40), cut(h.Line, 90))
		if id := header([]byte(h.text), "session"); id != "" {
			line += " (세션 " + short(id) + ")"
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// prHint says whether the local history holds the pull request's merge, then where it was talked about.
func prHint(cwd, n string) string {
	root := gitFolder(cwd)
	if root == "" {
		return findHint("PR #"+n, "#"+n, 2)
	}
	state := fmt.Sprintf("[PR #%s] 내 컴퓨터 git 기록에 머지 없음 — 아직 안 받아 왔을 수 있으니 gh pr view %s 로 확인", n, n)
	if hash, at, ok := worklog.PRMerged(root, n); ok {
		state = fmt.Sprintf("[PR #%s] 내 컴퓨터 git 기록: %s 머지됨 (%s)", n, at.Local().Format("01-02 15:04"), hash)
	}
	return state + "\n" + findHint("PR #"+n+" 이야기", "#"+n, 2)
}

// leftOffHint is this repository's latest conversations and where each stopped, for "what we
// were doing" said without a day or a name.
func leftOffHint(repo, session string) string {
	type recent struct {
		when time.Time
		line string
	}
	var all []recent
	paths, _ := filepath.Glob(filepath.Join(RecordDir(), "*", "*.md"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil || header(raw, "repo") != repo || header(raw, "session") == session {
			continue
		}
		requests := dayRequests(string(raw))
		if len(requests) == 0 {
			continue
		}
		last := requests[len(requests)-1]
		all = append(all, recent{last.at, fmt.Sprintf("- %s %s — 마지막 요청 %s · 끝난 지점(AI 주장) %s (세션 %s)",
			last.at.Format("01-02 15:04"), cut(title(string(raw), filepath.Base(path)), 40), cut(last.text, 60), cut(last.answer, 60), short(header(raw, "session")))})
	}
	if len(all) == 0 {
		return "[이 레포 최근 대화] 기록 없음"
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].when.After(all[j].when) })
	lines := []string{"[이 레포 최근 대화 — 원문: harness recall source <세션> <말>]"}
	for _, r := range all[:min(len(all), 3)] {
		lines = append(lines, r.line)
	}
	return strings.Join(lines, "\n")
}
