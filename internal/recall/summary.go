package recall

import (
	"crypto/sha256"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"slices"
	"strings"
)

type summaryAttempt struct {
	At       string `json:"at"`
	RawSHA   string `json:"raw_sha256"`
	Cursor   int    `json:"cursor"`
	Outcome  string `json:"outcome"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

type resolution struct {
	What     string `json:"what"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Source   string `json:"source"`
	Evidence string `json:"evidence"`
	Stale    bool   `json:"stale,omitempty"`
}

// Requests are split without dropping their middle; the cursor advances only after success.
func fragments(s session) []worklog.Request {
	var out []worklog.Request
	for _, r := range s.requests {
		text := []rune(mask(r.Text))
		for len(text) > 0 {
			n := min(1800, len(text))
			part := r
			part.Text = string(text[:n])
			out = append(out, part)
			text = text[n:]
		}
	}
	return out
}

func sourceText(s session, id string) (string, string) {
	for _, r := range s.requests {
		if r.ID == id {
			return mask(r.Text), "사용자"
		}
	}
	for _, c := range s.commands {
		if c.source == id && c.result == "성공" {
			return mask(c.text), "검사"
		}
	}
	return "", ""
}

func grounded(s session, id, evidence string) string {
	text, kind := sourceText(s, id)
	if strings.TrimSpace(evidence) == "" || text == "" || !strings.Contains(text, evidence) {
		return ""
	}
	for _, r := range s.requests {
		if r.ID == id && strings.HasSuffix(r.Source, ":choice") {
			_, answer, ok := strings.Cut(r.Text, "\n선택: ")
			if !ok || !strings.Contains(evidence, answer) {
				return ""
			}
		}
	}
	return kind
}

func mergeSummary(s session, old, fresh summary) summary {
	out := old
	out.Decisions = slices.Clone(old.Decisions)
	out.Todo = slices.Clone(old.Todo)
	out.Lessons = slices.Clone(old.Lessons)
	out.Resolutions = slices.Clone(old.Resolutions)
	// A rewritten source cannot continue proving an old interpretation or closure.
	for i := range out.Decisions {
		d := &out.Decisions[i]
		if d.Source != "" && grounded(s, d.Source, d.Evidence) == "" {
			d.Status = "추정·원문 변경"
		}
	}
	for i := range out.Resolutions {
		r := &out.Resolutions[i]
		if grounded(s, r.Source, r.Evidence) == "" {
			r.Stale = true
		}
	}
	if fresh.Title != "" {
		out.Title = fresh.Title
	}
	for _, d := range fresh.Decisions {
		d.Status = grounded(s, d.Source, d.Evidence)
		if d.Status == "" {
			d.Status, d.Source, d.Evidence = "추정", "", ""
		}
		// A UI answer is already explicit: store it verbatim instead of a model paraphrase.
		for _, r := range s.requests {
			if d.Status == "사용자" && r.ID == d.Source && strings.HasSuffix(r.Source, ":choice") {
				d.What = mask(r.Text)
			}
		}
		i := slices.IndexFunc(out.Decisions, func(v decision) bool { return v.What == d.What })
		if i < 0 {
			out.Decisions = append(out.Decisions, d)
		} else if d.Status != "추정" && sourceOrder(s, d.Source) >= sourceOrder(s, out.Decisions[i].Source) {
			out.Decisions[i] = d
		}
	}
	for _, v := range fresh.Todo {
		if !slices.Contains(out.Todo, v) {
			out.Todo = append(out.Todo, v)
		}
	}
	for _, v := range fresh.Lessons {
		if !slices.Contains(out.Lessons, v) {
			out.Lessons = append(out.Lessons, v)
		}
	}
	for _, r := range fresh.Resolutions {
		kind := grounded(s, r.Source, r.Evidence)
		if kind == "" || !slices.Contains([]string{"취소", "교체", "완료", "재개"}, r.State) {
			continue
		}
		if r.Kind == "decision" && (kind != "사용자" || r.State == "완료" || !slices.ContainsFunc(out.Decisions, func(d decision) bool { return d.What == r.What })) {
			continue
		}
		if r.Kind == "todo" && (!slices.Contains(out.Todo, r.What) || (kind == "검사" && r.What != r.Evidence)) {
			continue
		}
		if r.Kind != "decision" && r.Kind != "todo" {
			continue
		}
		if kind == "사용자" {
			text, _ := sourceText(s, r.Source)
			if strings.TrimSpace(text) != strings.TrimSpace(r.Evidence) || !strings.Contains(r.Evidence, r.What) || !stateCue(r.State, r.Evidence) {
				continue
			}
		}
		if kind == "검사" && r.State != "완료" {
			continue
		}
		// Repeated processing is idempotent; a later explicit change replaces this state.
		i := slices.IndexFunc(out.Resolutions, func(v resolution) bool { return v.Kind == r.Kind && v.What == r.What })
		if i < 0 {
			out.Resolutions = append(out.Resolutions, r)
		} else if sourceOrder(s, r.Source) >= sourceOrder(s, out.Resolutions[i].Source) {
			out.Resolutions[i] = r
		}
	}
	return out
}

func resolved(m summary, kind, what string) (resolution, bool) {
	for i := len(m.Resolutions) - 1; i >= 0; i-- {
		r := m.Resolutions[i]
		if r.Kind == kind && r.What == what {
			return r, !r.Stale && slices.Contains([]string{"완료", "취소", "교체"}, r.State)
		}
	}
	return resolution{}, false
}

func sourceOrder(s session, id string) int {
	for _, r := range s.requests {
		if r.ID == id {
			return r.Line
		}
	}
	return -1
}

func stateCue(state, text string) bool {
	negations := map[string][]string{
		"완료": {"미완료", "미확인", "아직", "못", "않", "아니", "실패"},
		"취소": {"취소하지", "취소 안", "취소는 아니"},
		"교체": {"바꾸지", "안 바꿔", "교체하지", "교체 안"},
		"재개": {"재개하지", "재개 안", "다시 하지", "안 해"},
	}
	if slices.ContainsFunc(negations[state], func(cue string) bool { return strings.Contains(text, cue) }) {
		return false
	}
	cues := map[string][]string{"취소": {"취소", "하지 마", "하지마", "그만"}, "교체": {"교체", "대신", "바꿔"}, "완료": {"완료", "끝났", "끝났어", "끝났다"}, "재개": {"재개", "다시", "이어"}}
	return slices.ContainsFunc(cues[state], func(cue string) bool { return strings.Contains(text, cue) })
}

// requestPrefix detects edits to a processed request, not just growth in byte count.
func requestPrefix(s session, cursor int) string {
	parts := fragments(s)
	if cursor < 0 || cursor > len(parts) {
		return ""
	}
	h := sha256.New()
	for _, r := range parts[:cursor] {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", r.ID, r.Text, mask(r.Context))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}
