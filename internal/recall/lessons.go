package recall

import (
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const (
	sameLesson  = 0.55 // bigram overlap at which two phrasings count as one lesson
	cardLessons = 5
)

// Lesson is one rule learned from past sessions. Items are merged one by one; the file is
// never regenerated wholesale, so an old lesson cannot vanish because a summary forgot it.
type Lesson struct {
	Rule      string         `json:"rule"`
	Scope     string         `json:"scope"` // repo name, or 전체
	Origin    string         `json:"origin"`
	Sessions  []string       `json:"sessions"`
	LastSeen  string         `json:"last_seen"`
	Status    string         `json:"status"` // 후보, 활성, 보관
	Confirmed bool           `json:"confirmed,omitempty"`
	Sources   []LessonSource `json:"sources,omitempty"`
	extra     map[string]json.RawMessage
}

func lessonsPath() string { return filepath.Join(Dir(), "lessons.json") }

func loadLessons() ([]Lesson, error) {
	raw, err := os.ReadFile(lessonsPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Lesson
	return out, json.Unmarshal(raw, &out)
}

func saveLessons(all []Lesson) error {
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	return writeLessonFile(lessonsPath(), raw)
}

func bigrams(s string) map[string]bool {
	r := []rune(strings.ToLower(strings.Join(strings.Fields(s), "")))
	out := map[string]bool{}
	for i := 0; i+1 < len(r); i++ {
		out[string(r[i:i+2])] = true
	}
	return out
}

func overlap(a, b string) float64 {
	x, y := bigrams(a), bigrams(b)
	if len(x) == 0 || len(y) == 0 {
		return 0
	}
	common := 0
	for k := range x {
		if y[k] {
			common++
		}
	}
	return float64(common) / float64(len(x)+len(y)-common)
}

// Repeated model claims remain candidates; repetition is not user confirmation.
func merge(all []Lesson, add Lesson, session, day string) []Lesson {
	add.Rule = strings.TrimSpace(mask(add.Rule))
	if len([]rune(add.Rule)) < 6 {
		return all
	}
	for i := range all {
		if all[i].Scope != add.Scope || overlap(all[i].Rule, add.Rule) < sameLesson {
			continue
		}
		if !slices.Contains(all[i].Sessions, session) {
			all[i].Sessions = append(all[i].Sessions, session)
		}
		all[i].LastSeen = max(all[i].LastSeen, day)
		return all
	}
	add.Sessions, add.LastSeen, add.Status = []string{session}, day, "후보"
	return append(all, add)
}

// RepoName is the git top folder's name, else the folder's own name.
func RepoName(cwd string) string {
	if root := worklog.RepoRoot(cwd); root != "" {
		return filepath.Base(root)
	}
	return filepath.Base(cwd)
}

// learn folds a summarized session's lesson candidates into lessons.json and re-renders 배운것.md.
func learn(s session, m summary) error {
	if len(m.Lessons) == 0 {
		return nil
	}
	all, err := loadLessons()
	if err != nil {
		return err
	}
	for _, rule := range m.Lessons {
		day := s.start.Local().Format("2006-01-02")
		// Source migration is manual; installing a new writer must not apply it in the background.
		all = merge(all, Lesson{Rule: rule, Scope: RepoName(s.cwd), Origin: "기록"}, s.id, day)
	}
	if err = saveLessons(all); err != nil {
		return err
	}
	return renderLessons(all)
}

// renderLessons writes 배운것.md next to lessons.json. It is a view; lessons.json is the source.
func renderLessons(all []Lesson) error {
	var rows []Lesson
	for _, l := range all {
		if l.Status != "보관" {
			rows = append(rows, l)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Status != rows[j].Status {
			return rows[i].Status == "활성"
		}
		return knownSessions(rows[i]) > knownSessions(rows[j])
	})
	var b strings.Builder
	b.WriteString("# 배운 것\n\n세션 기록에서 모은 교훈 후보다. 정본은 `lessons.json`, 이 파일은 보기용이다. 반복 횟수만으로 규칙이 되지 않는다. 시작 카드에는 사용자가 확인한 활성 항목만 들어간다. 세션 시작·메모리 수입 날짜는 발언 날짜가 아니다. 옛 last_seen 값은 호환 이력으로 보존하며 발언 날짜로 사용하지 않는다.\n\n| 상태 | 범위 | 교훈 | 확인 세션 | 미확인 연결 | 세션 시작 | 수입일 | 발언일 |\n| --- | --- | --- | --- | --- | --- | --- | --- |\n")
	for _, l := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %d | %s | %s | 미확인 |\n", l.Status, l.Scope, strings.ReplaceAll(l.Rule, "|", "/"), knownSessions(l), unknownSessions(l), sourceDate(l, false), sourceDate(l, true))
	}
	return writeLessonFile(filepath.Join(Dir(), "배운것.md"), []byte(b.String()))
}

func writeLessonFile(path string, raw []byte) error {
	previous, err := os.ReadFile(path)
	if err == nil && string(previous) == string(raw) {
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return memory.PrivateWrite(path, raw)
}

// CardLines go under the start card: this repo's active lessons and how to search the past.
func CardLines(cwd string) []string {
	all, _ := loadLessons()
	repo := RepoName(cwd)
	var rows []Lesson
	for _, l := range all {
		if l.Status == "활성" && l.Confirmed && (l.Scope == repo || l.Scope == "전체") {
			rows = append(rows, l)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if knownSessions(rows[i]) != knownSessions(rows[j]) {
			return knownSessions(rows[i]) > knownSessions(rows[j])
		}
		return latestSourceDate(rows[i], false) > latestSourceDate(rows[j], false)
	})
	var rules []string
	for _, l := range rows {
		rules = append(rules, l.Rule)
	}
	// Rules the person confirmed in the fact ledger, newest first.
	for _, rule := range confirmedRules(repo) {
		if !slices.Contains(rules, rule) {
			rules = append(rules, rule)
		}
	}
	var out []string
	if len(rules) > 0 {
		out = append(out, "=== 배운 것 — 사용자가 확인한 교훈 ===")
		for _, rule := range rules[:min(len(rules), cardLessons)] {
			out = append(out, "- "+rule)
		}
	}
	return append(out, "=== 과거 작업은 `harness recall find <말>`, 그날 한 일은 `harness recall day <YYYY-MM-DD|어제>`, 원문은 `harness recall source <세션 ID> <말>` 로 본다 ===")
}
