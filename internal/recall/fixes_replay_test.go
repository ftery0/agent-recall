//go:build replay

package recall

import (
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestFixReplay replays the archived conversations in order: each failure is matched against the
// pairs of the conversations before it only, as the hook after it would have been. A note counts as
// right when the conversation itself then got past the failure the way the note said.
//
//	go test -tags replay -run TestFixReplay ./internal/recall -v  (REPLAY_SINCE=2026-09-20)
func TestFixReplay(t *testing.T) {
	since, err := time.ParseInLocation("2006-01-02", os.Getenv("REPLAY_SINCE"), time.Local)
	if err != nil {
		since = time.Date(2026, 9, 20, 0, 0, 0, 0, time.Local)
	}
	records, _ := loadRecords()
	var sessions []session
	for _, rec := range records {
		if s, ok := readSession(rec.field("tool"), rec.field("raw")); ok && rec.field("raw") != "" {
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].start.Before(sessions[j].start) })
	var ledger []factLine
	failures, noted, right, wrong, unknown := 0, 0, 0, 0, 0
	for _, s := range sessions {
		repo := RepoName(s.cwd)
		own := map[string]fixPair{}
		for _, p := range fixPairs(s.tries) {
			own[p.source] = p
		}
		if !s.start.Before(since) {
			seen := map[string]bool{}
			folded := foldFacts(ledger)
			for _, f := range s.tries {
				if !f.failed {
					continue
				}
				failures++
				var fresh []string
				for _, note := range fixNotes(f.text, repo, folded) {
					if !seen[note] {
						seen[note] = true
						fresh = append(fresh, note)
					}
				}
				if len(fresh) == 0 {
					continue
				}
				noted++
				p, ok := own[f.source]
				switch {
				case !ok:
					unknown++
				case strings.Contains(strings.Join(fresh, "\n"), p.worked):
					right++
				default:
					wrong++
				}
				t.Logf("%s %s %s\n  실패: %s\n  붙음: %s\n  그 대화의 고침: %s", s.start.Format("01-02"), short(s.id), map[bool]string{true: "맞음", false: "?"}[ok && strings.Contains(strings.Join(fresh, "\n"), p.worked)], cut(f.text, 140), strings.Join(fresh, " / "), cut(p.worked, 140))
			}
		}
		ledger = append(ledger, fixFacts(s, repo)...)
	}
	pairs := 0
	for _, f := range foldFacts(ledger) {
		if f.Status == factActive {
			pairs++
			if os.Getenv("REPLAY_PAIRS") != "" {
				t.Logf("짝 %s %s · %s\n  → %s", f.At.Local().Format("01-02"), f.Scope, cut(f.Claims[0].Evidence.Quote, 140), cut(f.Value, 140))
			}
		}
	}
	t.Logf("대화 %d개 · 고친 짝 %d개 · %s 이후 실패 %d번 · 붙음 %d번 (맞음 %d, 다름 %d, 그 대화에서 안 고쳐짐 %d)", len(sessions), pairs, since.Format("01-02"), failures, noted, right, wrong, unknown)
}
