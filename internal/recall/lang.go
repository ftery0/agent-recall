package recall

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
)

// notProse is what the language judge skips — code, paths, URLs and quoted text — so a Korean
// sentence that names an English file or quotes an English error still counts as Korean.
var notProse = regexp.MustCompile("(?s:```.*?```)|`[^`\n]*`|https?://\\S+|\\S*/\\S+|(?m:^>.*$)|\"[^\"\n]*\"|“[^”\n]*”")

// heading is a section title in a compaction summary: a markdown heading or a "1. Title:" line.
var heading = regexp.MustCompile(`(?m)^(?:#{1,6} .+|\d+\. [^\n]{1,60}:)$`)

// english judges a piece of prose: English when under a tenth of its letters are Hangul.
// ok is false when fewer than least letters are left to judge.
func english(text string, least int) (en, ok bool) {
	ko, latin := 0, 0
	for _, r := range notProse.ReplaceAllString(text, " ") {
		if unicode.Is(unicode.Hangul, r) {
			ko++
		} else if r <= unicode.MaxASCII && unicode.IsLetter(r) {
			latin++
		}
	}
	return ko*10 < ko+latin, ko+latin >= least
}

func isHangul(r rune) bool { return unicode.Is(unicode.Hangul, r) }

type langBlock struct {
	model, kind, text string
	at                time.Time
	en, compacted     bool
}

type langSummary struct {
	at                       time.Time
	session, version         string
	en, marked, judged       bool
	headings, koreanHeadings int
}

// Lang measures, in Claude sessions where a person wrote Korean, how often the assistant's prose
// came out in English — progress notes (text followed by a tool call), final answers and
// compaction summaries, all with one judge. It reads the raw transcripts, since the record parser
// keeps only each turn's last report. With sample > 0 it prints that many judged blocks instead,
// so the judge can be checked by reading them. Named files (test sessions) are judged as given.
func Lang(files []string, since time.Time, sample int) (string, error) {
	paths := files
	if len(paths) == 0 {
		var err error
		if paths, err = filepath.Glob(filepath.Join(RawDir(), "claude", "*", "*.jsonl")); err != nil {
			return "", err
		}
	}
	var blocks []langBlock
	var summaries []langSummary
	sessions := 0
	for _, path := range paths {
		requests, _, err := worklog.ReadConversation(path)
		if err != nil {
			return "", err
		}
		if len(files) == 0 && !slices.ContainsFunc(requests, func(r worklog.Request) bool { return strings.ContainsFunc(r.Text, isHangul) }) {
			continue
		}
		b, s, err := langRead(path, since)
		if err != nil {
			return "", err
		}
		if len(b)+len(s) > 0 {
			sessions++
		}
		blocks, summaries = append(blocks, b...), append(summaries, s...)
	}
	if sample > 0 {
		return langSample(blocks, sample), nil
	}
	return langTable(sessions, blocks, summaries), nil
}

// langRead judges one transcript. A text block is a progress note when a tool call follows it
// before the next prompt, and a final answer otherwise.
func langRead(path string, since time.Time) ([]langBlock, []langSummary, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var blocks, pending []langBlock
	var summaries []langSummary
	settle := func(kind string) {
		for _, b := range pending {
			b.kind = kind
			blocks = append(blocks, b)
		}
		pending = nil
	}
	compacted := false
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 256<<20)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var row struct {
			Type             string `json:"type"`
			Subtype          string `json:"subtype"`
			Timestamp        string `json:"timestamp"`
			Version          string `json:"version"`
			SessionID        string `json:"sessionId"`
			IsSidechain      bool   `json:"isSidechain"`
			IsCompactSummary bool   `json:"isCompactSummary"`
			Message          struct {
				Model   string `json:"model"`
				Content any    `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, nil, fmt.Errorf("%s:%d: 대화 행 해석 실패", path, line)
		}
		if row.IsSidechain {
			continue
		}
		at := stamp(row.Timestamp)
		items, _ := row.Message.Content.([]any)
		switch {
		case row.Type == "system" && row.Subtype == "compact_boundary":
			compacted = true
		case row.IsCompactSummary:
			compacted = true
			settle("최종 답")
			if !at.Before(since) {
				summaries = append(summaries, judgeSummary(flatText(row.Message.Content), at, short(row.SessionID), row.Version))
			}
		case row.Type == "user":
			if slices.ContainsFunc(items, func(v any) bool { m, _ := v.(map[string]any); return m["type"] == "tool_result" }) {
				settle("진행 안내")
			} else {
				settle("최종 답")
			}
		case row.Type == "assistant" && row.Message.Model != "<synthetic>":
			for _, v := range items {
				m, _ := v.(map[string]any)
				switch m["type"] {
				case "tool_use":
					settle("진행 안내")
				case "text":
					text, _ := m["text"].(string)
					if en, ok := english(text, 15); ok && !at.Before(since) {
						pending = append(pending, langBlock{model: strings.TrimPrefix(row.Message.Model, "claude-"), text: text, at: at, en: en, compacted: compacted})
					}
				}
			}
		}
	}
	settle("최종 답")
	return blocks, summaries, scanner.Err()
}

// judgeSummary drops the fixed English wrapper Claude Code puts around a summary, then judges the
// summary's own prose and section titles.
func judgeSummary(text string, at time.Time, session, version string) langSummary {
	if _, body, ok := strings.Cut(text, "Summary:\n"); ok {
		text = body
	}
	if body, _, ok := strings.Cut(text, "If you need specific details from before compaction"); ok {
		text = body
	}
	s := langSummary{at: at, session: session, version: version, marked: strings.Contains(text, worklog.SummaryMarker)}
	text = strings.ReplaceAll(text, worklog.SummaryMarker, "")
	s.en, s.judged = english(text, 15)
	for _, h := range heading.FindAllString(text, -1) {
		if en, ok := english(h, 3); ok {
			s.headings++
			if !en {
				s.koreanHeadings++
			}
		}
	}
	return s
}

func langTable(sessions int, blocks []langBlock, summaries []langSummary) string {
	type tally struct{ en, all int }
	add := func(t *tally, en bool) {
		t.all++
		if en {
			t.en++
		}
	}
	show := func(t tally) string {
		if t.all == 0 {
			return "-"
		}
		return fmt.Sprintf("%d/%d %.1f%%", t.en, t.all, 100*float64(t.en)/float64(t.all))
	}
	rows := map[string]*[4]tally{} // progress notes, final answers, progress before compaction, after
	for _, b := range blocks {
		r := rows[b.model]
		if r == nil {
			r = &[4]tally{}
			rows[b.model] = r
		}
		if b.kind == "최종 답" {
			add(&r[1], b.en)
			continue
		}
		add(&r[0], b.en)
		if b.compacted {
			add(&r[3], b.en)
		} else {
			add(&r[2], b.en)
		}
	}
	models := slices.Collect(maps.Keys(rows))
	size := func(m string) int { return rows[m][0].all + rows[m][1].all }
	sort.Slice(models, func(i, j int) bool { return size(models[i]) > size(models[j]) })
	var b strings.Builder
	fmt.Fprintf(&b, "한국어로 말한 Claude 세션 %d개. 코드·경로·URL·인용을 빼고 한글이 글자의 10%% 미만이면 영어로 본다.\n\n", sessions)
	b.WriteString("관측값에는 요청한 영어 산출물도 포함될 수 있다. 압축 전후는 통제 비교가 아니다.\n\n")
	b.WriteString("| 모델 | 진행 안내 영어 | 최종 답 영어 | 진행 안내 압축 전 | 진행 안내 압축 이후 |\n| --- | --- | --- | --- | --- |\n")
	for _, m := range models {
		r := rows[m]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", m, show(r[0]), show(r[1]), show(r[2]), show(r[3]))
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].at.Before(summaries[j].at) })
	korean, marked, unjudged, headings, koreanHeadings := 0, 0, 0, 0, 0
	for _, s := range summaries {
		if !s.judged {
			unjudged++
		} else if !s.en {
			korean++
		}
		if s.marked {
			marked++
		}
		headings, koreanHeadings = headings+s.headings, koreanHeadings+s.koreanHeadings
	}
	fmt.Fprintf(&b, "\n압축 요약 %d개 — 설명이 한국어 %d개 · 표식(%s) %d개 · 판정 불가 %d개 · 절 제목 한국어 %d/%d\n", len(summaries), korean, worklog.SummaryMarker, marked, unjudged, koreanHeadings, headings)
	for _, s := range summaries[max(0, len(summaries)-10):] {
		lang, mark := "한국어", "없음"
		if !s.judged {
			lang = "판정 불가"
		} else if s.en {
			lang = "영어"
		}
		if s.marked {
			mark = "있음"
		}
		fmt.Fprintf(&b, "- %s %s %s · 설명 %s · 제목 %d/%d 한국어 · 표식 %s\n", s.at.Local().Format("01-02 15:04"), s.session, s.version, lang, s.koreanHeadings, s.headings, mark)
	}
	return b.String()
}

// langSample prints judged blocks spread evenly over the archive, half of them judged English,
// so the judge can be checked by reading them.
func langSample(blocks []langBlock, n int) string {
	var en, ko []langBlock
	for _, b := range blocks {
		if b.en {
			en = append(en, b)
		} else {
			ko = append(ko, b)
		}
	}
	pick := func(list []langBlock, k int) []langBlock {
		var out []langBlock
		for i := 0; i < k && i < len(list); i++ {
			out = append(out, list[i*len(list)/k])
		}
		return out
	}
	var lines []string
	for _, b := range append(pick(en, n/2), pick(ko, n-n/2)...) {
		label := "한국어"
		if b.en {
			label = "영어"
		}
		lines = append(lines, fmt.Sprintf("[%s] %s · %s · %s · %s", label, b.kind, b.model, b.at.Local().Format("01-02 15:04"), cut(mask(strings.Join(strings.Fields(b.text), " ")), 220)))
	}
	return strings.Join(lines, "\n")
}
