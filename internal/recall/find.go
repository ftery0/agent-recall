package recall

import (
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Hit is one note or record that matched a search.
type Hit struct {
	Path     string
	Title    string
	Line     string
	Score    float64
	Tier     string // 정확, 단어형 or 부분: how the words were found
	Ref      string // the request or heading the line came from
	text     string
	body     string
	mod      time.Time
	start    time.Time // a record's first request; zero for a note
	record   bool
	repo     string
	requests []recordRequest
}

// Korean particles and endings stuck to a search word; "세션에서" should still find "세션".
var particles = []string{"에서는", "에서", "으로", "에게", "까지", "부터", "했지", "했던", "하던", "했어", "이랑", "은", "는", "이", "가", "을", "를", "에", "로", "와", "과", "도", "만", "의", "랑"}

func terms(query string) []string {
	var out, words []string
	for _, field := range strings.Fields(strings.ToLower(query)) {
		words = append(words, scriptWords(field)...)
	}
	for _, word := range words {
		for _, p := range particles {
			if trimmed := strings.TrimSuffix(word, p); trimmed != word && len([]rune(trimmed)) >= 2 {
				word = trimmed
				break
			}
		}
		if len([]rune(word)) >= 2 {
			out = append(out, word)
		}
	}
	return out
}

// documents are the records plus the work notes in the vault. Old record copies left in the
// vault's 기록 folders are skipped so a session never shows up twice.
func documents() []Hit {
	var docs []Hit
	vault := worklog.VaultDir()
	add := func(root, base string) {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && d.Name() == recordFolder {
				return filepath.SkipDir
			}
			if err != nil || d.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			raw, err := os.ReadFile(path)
			info, errInfo := d.Info()
			if err != nil || errInfo != nil {
				return nil
			}
			rel, _ := filepath.Rel(base, path)
			text := string(raw)
			docs = append(docs, Hit{Path: rel, Title: title(text, d.Name()), text: text, body: frontless(text), mod: info.ModTime(), repo: header(raw, "repos")})
			return nil
		})
	}
	records, _ := loadRecords()
	for _, rec := range records {
		rel, _ := filepath.Rel(Dir(), rec.path)
		docs = append(docs, Hit{Path: rel, Title: rec.title(), text: string(rec.raw), body: withoutSection(frontless(string(rec.raw)), "기억 명령"),
			mod: rec.ended(), start: rec.started(), record: true, repo: rec.field("repo"), requests: rec.requests()})
	}
	for _, root := range worklog.NoteRoots(vault) {
		add(root, vault)
	}
	return docs
}

// frontless is a file's text after its front matter.
func frontless(text string) string {
	if strings.HasPrefix(text, "---\n") {
		if _, rest, ok := strings.Cut(text[4:], "\n---\n"); ok {
			return rest
		}
	}
	return text
}

func title(text, name string) string {
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "title: "); ok {
			return strings.Trim(v, `"`)
		}
		if v, ok := strings.CutPrefix(line, "# "); ok {
			return v
		}
	}
	return strings.TrimSuffix(name, ".md")
}

// Find ranks the records and notes holding the query's words; see searchIndex.search.
func Find(query string, limit int) []Hit {
	return newIndex(documents()).search(query, limit, time.Now(), nil)
}

// FindFilter narrows a find to one repository and to documents touched since a day.
type FindFilter struct {
	Repo  string
	Since time.Time
}

func (f FindFilter) keep(h Hit) bool {
	if f.Repo != "" && !strings.Contains(strings.ToLower(h.repo+" "+h.Path), strings.ToLower(f.Repo)) {
		return false
	}
	return f.Since.IsZero() || !h.mod.Before(f.Since)
}

// FindText is the command output: one hit per line, path · title · [tier] matching line, then
// where the line came from.
func FindText(query string, limit int, filter FindFilter) string {
	hits := newIndex(documents()).search(query, limit, time.Now(), filter.keep)
	if len(hits) == 0 {
		return "찾은 기록 없음 — 다른 말로 다시 찾거나, 원본은 `harness recall source <세션 앞 8자리> <말>` 로 본다"
	}
	var lines []string
	for _, h := range hits {
		line := fmt.Sprintf("%s · %s · [%s]", h.Path, h.Title, h.Tier)
		if h.Line != "" {
			line += " " + cut(h.Line, 160)
		}
		if id := header([]byte(h.text), "session"); id != "" {
			line += " · 원문 세션 " + id
		}
		if strings.HasPrefix(h.Ref, "요청 ") {
			line += " · " + h.Ref
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// Question is one entry of the measurement set.
type Question struct {
	ID       string   `json:"id"`
	Question string   `json:"question"`
	Sessions []string `json:"expect_sessions"`
	Note     string   `json:"expect_note"`
	Keywords []string `json:"keywords"`
	AskedAt  string   `json:"asked_at"` // UTC, to the minute
}

type evalHit struct {
	Path    string `json:"path"`
	Session string `json:"session,omitempty"`
	Matched bool   `json:"matched"`
}

type evalResult struct {
	ID              string    `json:"id"`
	KeywordHit      bool      `json:"keyword_hit"`
	QuestionHit     bool      `json:"question_hit"`
	KeywordRank     int       `json:"keyword_rank,omitempty"` // 1-based place of the first right result
	QuestionRank    int       `json:"question_rank,omitempty"`
	KeywordResults  []evalHit `json:"keyword_results"`
	QuestionResults []evalHit `json:"question_results"`
}

// Only a frontmatter session identifies a record; a body can cite another session.
func evalSession(h Hit) string {
	if !h.record || !strings.HasPrefix(h.text, "---\n") {
		return ""
	}
	front, _, ok := strings.Cut(h.text[4:], "\n---\n")
	if !ok {
		return ""
	}
	return strings.TrimSpace(header([]byte(front), "session"))
}

// An old eight-character name needs one full identity across the whole corpus,
// including records that are absent from the top five search results.
func evalAliases(docs []Hit) map[string]string {
	aliases := map[string]string{}
	for _, h := range docs {
		id := evalSession(h)
		if len(id) <= 8 {
			continue
		}
		for _, alias := range []string{id[:8], id[len(id)-8:]} {
			if old, ok := aliases[alias]; !ok || old == id {
				aliases[alias] = id
			} else {
				aliases[alias] = ""
			}
		}
	}
	return aliases
}

func evalIdentity(id string, aliases map[string]string) string {
	if len(id) == 8 {
		return aliases[id]
	}
	return id
}

func evalMatches(q Question, hits []Hit, aliases map[string]string) (int, []evalHit) {
	rank := 0
	results := make([]evalHit, 0, len(hits))
	for i, h := range hits {
		id := evalSession(h)
		matched := q.Note != "" && h.Path == q.Note
		if canonical := evalIdentity(id, aliases); canonical != "" {
			for _, expected := range q.Sessions {
				matched = matched || canonical == evalIdentity(expected, aliases)
			}
		}
		if matched && rank == 0 {
			rank = i + 1
		}
		results = append(results, evalHit{Path: h.Path, Session: id, Matched: matched})
	}
	return rank, results
}

// Eval counts the questions whose expected session or note shows up in the top five results,
// once with keywords and once with the question, with how many came first and the mean reciprocal
// rank. A retrieval hit does not verify a decision.
func Eval(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var questions []Question
	if err = json.Unmarshal(raw, &questions); err != nil {
		return nil, err
	}
	docs := documents()
	ix := newIndex(docs)
	aliases := evalAliases(docs)
	now := time.Now()
	type tally struct {
		hits, first int
		reciprocal  float64
		missed      []string
	}
	var keyword, question tally
	count := func(t *tally, id string, rank int) {
		if rank == 0 {
			t.missed = append(t.missed, id)
			return
		}
		t.hits++
		t.reciprocal += 1 / float64(rank)
		if rank == 1 {
			t.first++
		}
	}
	results := make([]evalResult, 0, len(questions))
	for _, q := range questions {
		// A question is searched as of when it was asked: later conversations, the asking one
		// included, were not there to be found and often repeat the answer.
		var keep func(Hit) bool
		if asked, err := time.Parse("2006-01-02T15:04", q.AskedAt); err == nil {
			keep = func(h Hit) bool { return h.start.IsZero() || h.start.Before(asked) }
		}
		keywordRank, keywordResults := evalMatches(q, ix.search(strings.Join(q.Keywords, " "), 5, now, keep), aliases)
		questionRank, questionResults := evalMatches(q, ix.search(q.Question, 5, now, keep), aliases)
		count(&keyword, q.ID, keywordRank)
		count(&question, q.ID, questionRank)
		results = append(results, evalResult{q.ID, keywordRank > 0, questionRank > 0, keywordRank, questionRank, keywordResults, questionResults})
	}
	mrr := func(t tally) float64 {
		if len(questions) == 0 {
			return 0
		}
		return math.Round(t.reciprocal/float64(len(questions))*1000) / 1000
	}
	return map[string]any{"questions": len(questions), "keyword_hits": keyword.hits, "question_hits": question.hits,
		"keyword_hit1": keyword.first, "question_hit1": question.first, "keyword_mrr": mrr(keyword), "question_mrr": mrr(question),
		"keyword_missed": keyword.missed, "question_missed": question.missed, "results": results}, nil
}

func flatText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	var parts []string
	items, _ := v.([]any)
	for _, item := range items {
		block, _ := item.(map[string]any)
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}
