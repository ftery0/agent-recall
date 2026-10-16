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
	hintPasted   = regexp.MustCompile(`(?s)<pasted_content[^>]*>.*?(</pasted_content[^>]*>|$)`)
	hintLogTime  = regexp.MustCompile(`^[ T]\d\d:\d\d`)
	// Words that only ask about the past or ask for an answer; what is left names the topic.
	hintFiller = map[string]bool{"저번": true, "지난번": true, "지난": true, "전에": true, "하던": true, "하던거": true, "그거": true, "이거": true, "저거": true,
		"어디": true, "어디까지": true, "뭐였지": true, "했는지": true, "했었지": true, "하다가": true, "말았던": true, "이어서": true, "계속": true,
		"다시": true, "이유": true, "우리": true, "내가": true, "너가": true, "여기": true, "거기": true, "저기": true, "항상": true, "어제": true,
		"그제": true, "그저께": true, "오늘": true, "있냐": true, "있니": true, "있던데": true, "했던데": true,
		"보고": true, "정리": true, "확인": true, "하자": true, "해보자": true, "진행": true, "얘기": true, "애기": true, "무슨": true,
		"어떻게": true, "됐어": true, "됐지": true, "했어": true, "할일": true, "해야": true, "해야되는거": true, "부분": true, "내용": true,
		"뭐하다": true, "하다": true, "말았니": true, "말았지": true, "기억": true, "안나": true, "지금": true, "한번": true, "그냥": true,
		"일단": true, "근데": true, "그리고": true, "전부": true, "전부다": true, "이제": true, "같은데": true, "보니까": true}
	hintFillerEnd = []string{"줘", "줘봐", "봐", "봐줘", "해봐", "할까", "했지", "했나", "는지", "던데", "있니", "있냐"}
	hintDayPart   = regexp.MustCompile(`^\d{1,2}(월|일)$`)
)

// hintTopic is the request's words without the ones that only point to the past.
func hintTopic(prompt string) []string {
	var topic []string
	for _, word := range terms(prompt) {
		word = strings.TrimRight(word, "?!.,~")
		if len([]rune(word)) < 2 || hintFiller[word] || hintDayWord(word) {
			continue
		}
		filler := false
		for _, end := range hintFillerEnd {
			filler = filler || strings.HasSuffix(word, end)
		}
		if !filler && !slices.Contains(topic, word) {
			topic = append(topic, word)
		}
	}
	// A long sentence left after the filler is not a topic to search for.
	if len(topic) > 4 {
		return nil
	}
	return topic
}

func hintDayWord(word string) bool {
	return hintISODay.MatchString(word) || hintMonthDay.MatchString(word) || hintDayPart.MatchString(word)
}

// HintText is what the harness looks up before the agent answers: the day, ticket, pull request
// or unfinished work a request points to. A request that points nowhere in the past gets nothing.
func HintText(prompt, cwd, session string, now time.Time) string {
	// Pasted text is someone else's words, often logs full of dates and numbers.
	prompt = hintPasted.ReplaceAllString(prompt, " ")
	repo := RepoName(cwd)
	topic := hintTopic(hintTicket.ReplaceAllString(hintPR.ReplaceAllString(prompt, " "), " "))
	var parts []string
	if day, ok := hintDay(prompt, now); ok {
		parts = append(parts, dayHint(day, repo, topic))
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
		if hint := topicHint(topic, session); hint != "" {
			parts = append(parts, hint)
		} else {
			parts = append(parts, leftOffHint(repo, session))
		}
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
// With topic words it keeps the conversations that mention them, and all of them if none does.
func dayHint(day time.Time, repo string, topic []string) string {
	date := day.Format("2006-01-02")
	all, label := dayConversations(day, topic), fmt.Sprintf("%s 중 '%s'", date, strings.Join(topic, " "))
	if len(topic) == 0 || len(all) == 0 {
		all, label = dayConversations(day, nil), date
	}
	if len(all) == 0 {
		return fmt.Sprintf("[%s] 그날 기록된 요청이 없다", date)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].repo == repo && all[j].repo != repo })
	lines := []string{fmt.Sprintf("[%s 대화 %d개 — 전부: harness recall day %s]", label, len(all), date)}
	for _, c := range all[:min(len(all), 5)] {
		last := c.requests[len(c.requests)-1]
		lines = append(lines, fmt.Sprintf("- %s · 그날 마지막 %s %s%s (세션 %s)", c.repo, last.at.Format("15:04"), cut(last.text, 60), firstTitle(c.title, last.text), short(c.session)))
	}
	return strings.Join(lines, "\n")
}

// topicHint is what the topic words find outside this conversation, or nothing.
func topicHint(topic []string, session string) string {
	if len(topic) == 0 {
		return ""
	}
	query := strings.Join(topic, " ")
	var lines []string
	for _, h := range Find(query, 6) {
		id := header([]byte(h.text), "session")
		if id == session && id != "" || len(lines) == 3 {
			continue
		}
		line := fmt.Sprintf("- %s · %s", cut(h.Title, 40), cut(h.Line, 90))
		if id != "" {
			line += " (세션 " + short(id) + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(append([]string{fmt.Sprintf("['%s' 찾은 기억 — 더: harness recall find '%s']", query, query)}, lines...), "\n")
}

// firstTitle names a conversation's first request when it differs from the latest one; a long
// conversation keeps its first request as its title long after the work moved on.
func firstTitle(title, latest string) string {
	if title == "" || strings.HasPrefix(latest, strings.TrimSuffix(title, "…")) {
		return ""
	}
	return " (처음: " + cut(title, 30) + ")"
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
		all = append(all, recent{last.at, fmt.Sprintf("- %s 마지막 요청 %s%s · 끝난 지점(AI 주장) %s (세션 %s)",
			last.at.Format("01-02 15:04"), cut(last.text, 60), firstTitle(title(string(raw), filepath.Base(path)), last.text), cut(last.answer, 60), short(header(raw, "session")))})
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
