package recall

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// dayConversation is one conversation's requests on one day.
type dayConversation struct {
	path, session, repo, tool, title string
	first                            time.Time
	requests                         []recordRequest
}

// dayConversations reads every record, since a record is filed under the day its conversation
// began. Words match a request, its answer, or the conversation's repo, tool, title and session,
// so a repo name narrows the day to that repo.
func dayConversations(day time.Time, words []string) []dayConversation {
	var all []dayConversation
	records, _ := loadRecords()
	for _, rec := range records {
		c := dayConversation{path: rec.path, session: rec.field("session"), repo: rec.field("repo"), tool: rec.field("tool"), title: rec.title()}
		about := strings.Join([]string{c.repo, c.tool, c.title, c.session}, " ")
		for _, r := range rec.requests() {
			if r.at.Format("2006-01-02") == day.Format("2006-01-02") && anyWord(r.text+" "+r.answer+" "+about, words) {
				c.requests = append(c.requests, r)
			}
		}
		if len(c.requests) > 0 {
			c.first = c.requests[0].at
			all = append(all, c)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].first.Before(all[j].first) })
	return all
}

// DayText lists what was asked on one day, conversation by conversation.
func DayText(when, query string, limit int, now time.Time) (string, error) {
	day, err := parseDay(when, now)
	if err != nil {
		return "", err
	}
	date := day.Format("2006-01-02")
	words := terms(query)
	all := dayConversations(day, words)
	if len(all) == 0 {
		// Words that miss would otherwise read as an empty day.
		if n := len(dayConversations(day, nil)); n > 0 && len(words) > 0 {
			return fmt.Sprintf("그날 찾을 말에 맞는 요청이 없다: %s (찾을 말: %s) — 말을 빼면 대화 %d개: harness recall day %s", date, query, n, date), nil
		}
		return "그날 기록된 요청이 없다: " + date, nil
	}
	lines := []string{fmt.Sprintf("%s 요청이 있던 대화 %d개 — 시작한 날과 상관없이 그날 한 요청만 보인다", date, len(all))}
	for _, c := range all {
		lines = append(lines, "", strings.Join([]string{c.path, c.repo, c.tool, c.title, "세션 " + c.session}, " · "))
		// The day's last requests show where the work stopped; the title already names the goal.
		skipped := max(0, len(c.requests)-limit)
		if skipped > 0 {
			lines = append(lines, fmt.Sprintf("  … 그날 앞 요청 %d개 생략 — 전부는 위 기록 파일, 원문은 harness recall source %s <말>", skipped, c.session))
		}
		for _, r := range c.requests[skipped:] {
			lines = append(lines, "  - "+r.at.Format("15:04")+" "+cut(r.text, 160))
			if r.answer != "" {
				lines = append(lines, "    → AI(주장): "+cut(r.answer, 120))
			}
		}
	}
	return strings.Join(lines, "\n"), nil
}

func parseDay(when string, now time.Time) (time.Time, error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	switch when {
	case "오늘":
		return today, nil
	case "어제":
		return today.AddDate(0, 0, -1), nil
	}
	if day, err := time.ParseInLocation("2006-01-02", when, time.Local); err == nil {
		return day, nil
	}
	if day, err := time.ParseInLocation("2006-01-02", fmt.Sprintf("%d-%s", today.Year(), when), time.Local); err == nil && len(when) == 5 {
		if day.After(today) { // early January asking about late December
			day = day.AddDate(-1, 0, 0)
		}
		return day, nil
	}
	return time.Time{}, fmt.Errorf("날짜를 읽지 못했다: %q — YYYY-MM-DD, MM-DD, 오늘, 어제 중 하나로 쓴다", when)
}

func anyWord(text string, words []string) bool {
	if len(words) == 0 {
		return true
	}
	lower := strings.ToLower(text)
	for _, w := range words {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}
