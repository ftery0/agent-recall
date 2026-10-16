package recall

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const noticeEvery = 7 * 24 * time.Hour

type recallMiss struct {
	when time.Time
	line string
}

// recallTally counts the memory lookups run since then, by how they came back, and keeps the
// empty and failed ones newest first.
func recallTally(since time.Time) (int, map[string]int, []recallMiss) {
	counts := map[string]int{}
	var misses []recallMiss
	total := 0
	paths, _ := filepath.Glob(filepath.Join(RecordDir(), "*", "*.md"))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if err != nil || statErr != nil {
			continue
		}
		ended := recordTime(raw, info.ModTime())
		_, section, found := strings.Cut(string(raw), "\n### 기억 명령\n")
		if !found || ended.Before(since) {
			continue
		}
		started, _ := time.ParseInLocation("2006-01-02", header(raw, "date"), time.Local)
		for _, line := range strings.Split(section, "\n") {
			item, ok := strings.CutPrefix(line, "- ")
			if !ok {
				break
			}
			// A conversation can run for days, so each lookup carries its own time.
			when := ended
			if stamp, rest, ok := strings.Cut(item, " `"); ok && len(stamp) == len(requestStamp) {
				if at, err := time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%d-%s", started.Year(), stamp), time.Local); err == nil {
					if at.Before(started) { // the conversation ran past New Year
						at = at.AddDate(1, 0, 0)
					}
					when, item = at, "`"+rest
				}
			}
			if when.Before(since) {
				continue
			}
			command, result, _ := strings.Cut(item, " → ")
			result, _, _ = strings.Cut(result, " [")
			total++
			counts[result]++
			if result == "빈 결과" || result == "오류" {
				misses = append(misses, recallMiss{when, fmt.Sprintf("%s %s %s %s · %s → %s · %s",
					when.Format("01-02 15:04"), header(raw, "repo"), header(raw, "tool"), short(header(raw, "session")), command, result, path)})
			}
		}
	}
	sort.SliceStable(misses, func(i, j int) bool { return misses[i].when.After(misses[j].when) })
	return total, counts, misses
}

// MissesText is the week's memory lookups by how they came back, and the empty or failed ones.
func MissesText(days int, now time.Time) string {
	total, counts, misses := recallTally(now.AddDate(0, 0, -days))
	if total == 0 {
		return fmt.Sprintf("지난 %d일 기록에 AI가 부른 기억 명령이 없다", days)
	}
	lines := []string{fmt.Sprintf("지난 %d일 기억 명령 %d번 — 결과 있음 %d · 빈 결과 %d · 오류 %d", days, total, counts["결과 있음"], counts["빈 결과"], counts["오류"])}
	for _, m := range misses[:min(len(misses), 10)] {
		lines = append(lines, "- "+m.line)
	}
	if len(misses) > 10 {
		lines = append(lines, fmt.Sprintf("… 외 %d개", len(misses)-10))
	}
	return strings.Join(lines, "\n")
}

// MissesNotice tells the person, at most once a week, that memory lookups came back empty or
// failed. The check itself also runs at most once a week.
func MissesNotice(now time.Time) string {
	path := filepath.Join(Dir(), "misses-checked")
	if raw, err := os.ReadFile(path); err == nil {
		if at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw))); err == nil && now.Sub(at) < noticeEvery {
			return ""
		}
	}
	if os.MkdirAll(Dir(), 0o700) != nil || os.WriteFile(path, []byte(now.Format(time.RFC3339)+"\n"), 0o600) != nil {
		return ""
	}
	total, counts, _ := recallTally(now.Add(-noticeEvery))
	if counts["빈 결과"]+counts["오류"] == 0 {
		return ""
	}
	return fmt.Sprintf("하네스: 지난 7일 AI 기억 명령 %d번 중 빈 결과 %d번·오류 %d번 — 자세히는 harness recall misses", total, counts["빈 결과"], counts["오류"])
}
