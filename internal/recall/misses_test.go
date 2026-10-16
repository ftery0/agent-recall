package recall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMissesCountLookupsThatCameBackEmptyOrFailedAndNoticeWeekly(t *testing.T) {
	lookupSessions(t)
	built := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if out, err := Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), false, built); err != nil || out["written"] != 2 {
		t.Fatal(out, err)
	}
	at := func(minute int) string {
		return time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC).Local().Format("01-02 15:04")
	}
	codex := func(minute int) string {
		return "- " + at(minute) + " repo codex " + short("01a0aaaa-0000-7000-8000-00000000c0de") + " · "
	}
	claude := func(minute int) string { return "- " + at(minute) + " repo claude abcdef12 · " }
	text := MissesText(7, built)
	lines := strings.Split(text, "\n")
	want := []string{
		"지난 7일 기억 명령 10번 — 결과 있음 3 · 빈 결과 4 · 오류 3",
		codex(11) + "`rtk proxy harness recall source 01a11bf3 --limit 4` → 오류 · ",
		codex(9) + "`rtk harness recall source 01a0 --limit 10` → 빈 결과 · ",
		codex(5) + "`harness recall day 10/15` → 오류 · ",
		claude(5) + "`harness recall source abcdef12 PDF` → 오류 · ",
		codex(3) + "`rtk harness recall find 'TSK-1344'` → 빈 결과 · ",
		codex(1) + "`rtk harness recall day 2026-10-15 web_app` → 빈 결과 · ",
		claude(1) + "`harness recall day 어제` → 빈 결과 · ",
	}
	if len(lines) != len(want) {
		t.Fatalf("want %d lines\n%s", len(want), text)
	}
	for i := range want {
		if !strings.HasPrefix(lines[i], want[i]) {
			t.Fatalf("line %d: want prefix %q\n%s", i, want[i], text)
		}
	}
	// A conversation can run for days; lookups older than the window are not counted.
	if text := MissesText(7, time.Date(2026, 10, 14, 1, 6, 0, 0, time.UTC)); !strings.HasPrefix(text, "지난 7일 기억 명령 4번 — 결과 있음 2 · 빈 결과 1 · 오류 1\n"+codex(11)) {
		t.Fatal("창보다 오래된 명령을 셈", text)
	}
	if text := MissesText(7, built.AddDate(0, 0, 30)); text != "지난 7일 기록에 AI가 부른 기억 명령이 없다" {
		t.Fatal("지난 기록을 이번 주로 셈", text)
	}
	notice := "하네스: 지난 7일 AI 기억 명령 10번 중 빈 결과 4번·오류 3번 — 자세히는 harness recall misses"
	if got := MissesNotice(built); got != notice {
		t.Fatal("첫 알림이 다름", got)
	}
	if got := MissesNotice(built.Add(time.Hour)); got != "" {
		t.Fatal("한 주 안에 다시 알림", got)
	}
	os.WriteFile(filepath.Join(Dir(), "misses-checked"), []byte(built.Add(-8*24*time.Hour).Format(time.RFC3339)+"\n"), 0o600)
	if got := MissesNotice(built); got != notice {
		t.Fatal("한 주가 지나도 다시 알리지 않음", got)
	}
}
