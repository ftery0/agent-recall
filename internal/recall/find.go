package recall

import (
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Hit is one note or record that matched a search.
type Hit struct {
	Path   string
	Title  string
	Line   string
	Score  float64
	text   string
	body   string
	mod    time.Time
	record bool
}

// Korean particles and endings stuck to a search word; "세션에서" should still find "세션".
var particles = []string{"에서는", "에서", "으로", "에게", "까지", "부터", "했지", "했던", "하던", "했어", "이랑", "은", "는", "이", "가", "을", "를", "에", "로", "와", "과", "도", "만", "의", "랑"}

func terms(query string) []string {
	var out []string
	for _, word := range strings.Fields(strings.ToLower(query)) {
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
	add := func(root, base string, skipRecords, record bool) {
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && skipRecords && d.Name() == recordFolder {
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
			body := text
			if strings.HasPrefix(body, "---\n") {
				if _, rest, ok := strings.Cut(body[4:], "\n---\n"); ok {
					body = rest
				}
			}
			if record {
				body = withoutLookups(body)
			}
			docs = append(docs, Hit{Path: rel, Title: title(text, d.Name()), text: text, body: body, mod: info.ModTime(), record: record})
			return nil
		})
	}
	add(RecordDir(), Dir(), false, true)
	for _, root := range worklog.NoteRoots(vault) {
		add(root, vault, true, false)
	}
	return docs
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

// withoutLookups drops a record's memory lookups: they repeat the words the agent looked for
// and answer nothing.
func withoutLookups(body string) string {
	before, rest, ok := strings.Cut(body, "\n### 기억 명령\n")
	if !ok {
		return body
	}
	if i := strings.Index(rest, "\n#"); i >= 0 {
		return before + rest[i:]
	}
	return before + "\n"
}

// Find ranks documents by how many of the words they hold, rarer words counting more,
// and shows the line that holds the most of them. Ties go to the newer file.
func Find(query string, limit int) []Hit {
	return findDocuments(query, limit, documents())
}

func findDocuments(query string, limit int, docs []Hit) []Hit {
	words := terms(query)
	if len(words) == 0 || len(docs) == 0 {
		return nil
	}
	weight := map[string]float64{}
	for _, w := range words {
		df := 0
		for _, d := range docs {
			if strings.Contains(strings.ToLower(d.body), w) {
				df++
			}
		}
		weight[w] = math.Log(1 + float64(len(docs))/float64(df+1))
	}
	var hits []Hit
	for _, d := range docs {
		lower := strings.ToLower(d.body)
		for _, w := range words {
			if n := strings.Count(lower, w); n > 0 {
				d.Score += weight[w] * (1 + math.Log(float64(n)))
				if strings.Contains(strings.ToLower(d.Title), w) {
					d.Score += weight[w]
				}
			}
		}
		if d.Score == 0 {
			continue
		}
		best := 0
		for _, line := range strings.Split(d.body, "\n") {
			count := 0
			for _, w := range words {
				if strings.Contains(strings.ToLower(line), w) {
					count++
				}
			}
			if count > best {
				best, d.Line = count, strings.TrimSpace(line)
			}
		}
		hits = append(hits, d)
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].mod.After(hits[j].mod)
	})
	return hits[:min(limit, len(hits))]
}

// FindText is the command output: one hit per line, path · title · matching line.
func FindText(query string, limit int) string {
	hits := Find(query, limit)
	if len(hits) == 0 {
		return "찾은 기록 없음 — 다른 말로 다시 찾거나, 원본은 `harness recall source <세션 앞 8자리> <말>` 로 본다"
	}
	var lines []string
	for _, h := range hits {
		line := fmt.Sprintf("%s · %s · %s", h.Path, h.Title, cut(h.Line, 160))
		if id := header([]byte(h.text), "session"); id != "" {
			line += " · 원문 세션 " + id
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

func evalMatches(q Question, hits []Hit, aliases map[string]string) (bool, []evalHit) {
	found := false
	results := make([]evalHit, 0, len(hits))
	for _, h := range hits {
		id := evalSession(h)
		matched := q.Note != "" && h.Path == q.Note
		if canonical := evalIdentity(id, aliases); canonical != "" {
			for _, expected := range q.Sessions {
				matched = matched || canonical == evalIdentity(expected, aliases)
			}
		}
		found = found || matched
		results = append(results, evalHit{Path: h.Path, Session: id, Matched: matched})
	}
	return found, results
}

// Eval counts the questions whose expected session or note shows up in the top five results,
// once with keywords and once with the question. A retrieval hit does not verify a decision.
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
	aliases := evalAliases(docs)
	keywordHits, questionHits := 0, 0
	var keywordMissed, questionMissed []string
	results := make([]evalResult, 0, len(questions))
	for _, q := range questions {
		keywordHit, keywordResults := evalMatches(q, findDocuments(strings.Join(q.Keywords, " "), 5, docs), aliases)
		questionHit, questionResults := evalMatches(q, findDocuments(q.Question, 5, docs), aliases)
		if keywordHit {
			keywordHits++
		} else {
			keywordMissed = append(keywordMissed, q.ID)
		}
		if questionHit {
			questionHits++
		} else {
			questionMissed = append(questionMissed, q.ID)
		}
		results = append(results, evalResult{q.ID, keywordHit, questionHit, keywordResults, questionResults})
	}
	return map[string]any{"questions": len(questions), "keyword_hits": keywordHits, "question_hits": questionHits,
		"keyword_missed": keywordMissed, "question_missed": questionMissed, "results": results}, nil
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
	return strings.Join(parts, " ")
}
