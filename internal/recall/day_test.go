package recall

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// twoDayFixture writes a conversation from 10-07 23:50 to 10-08 09:11 and another that starts
// on 10-08, both in Seoul time, and builds their records.
func twoDayFixture(t *testing.T) (path, night, morning string) {
	t.Helper()
	raw, _, path := recordFixture(t)
	night, morning = "abcdef12-0000-0000-0000-000000000000", "fedcba98-0000-0000-0000-000000000000"
	human := func(id, uuid, text string, at time.Time) map[string]any {
		row := recordHuman(uuid, text, at)
		row["sessionId"], row["cwd"] = id, "/work/repo"
		return row
	}
	utc := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	write(t, path, jsonLines(t,
		human(night, "evening", "밤에 시작한 일", utc(7, 14, 50)),
		said(utc(7, 14, 51).Format(time.RFC3339), "밤 작업을 했습니다.", false),
		human(night, "morning", "아침에 이어서 한 일", utc(8, 0, 10)),
		said(utc(8, 0, 11).Format(time.RFC3339), "아침 작업을 끝냈습니다.", false),
	))
	write(t, filepath.Join(raw, "claude", "2026-10-08", morning+".jsonl"), jsonLines(t,
		human(morning, "other-1", "다른 대화 첫 요청", utc(7, 23, 0)),
		said(utc(7, 23, 1).Format(time.RFC3339), "첫 요청을 했습니다.", false),
		human(morning, "other-2", "다른 대화 두번째 요청", utc(8, 1, 0)),
		said(utc(8, 1, 1).Format(time.RFC3339), "두번째 요청도 끝냈습니다.", false),
	))
	if out, err := Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), false, utc(9, 3, 0)); err != nil || out["written"] != 2 {
		t.Fatal(out, err)
	}
	return path, night, morning
}

func TestRecordDatesRequestsOfAConversationThatRunsForDays(t *testing.T) {
	path, _, _ := twoDayFixture(t)
	s, ok := readSession("claude", path)
	if !ok {
		t.Fatal("session not read", s)
	}
	text := render(s, summary{}, false)
	for _, want := range []string{"date: 2026-10-07\n", "time: 23:50-09:11\n", "end: 2026-10-08 09:11\n", "- 10-07 23:50 [evening 줄1", "- 10-08 09:10 [morning 줄3"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	if at := recordTime([]byte(text), time.Time{}); !at.Equal(time.Date(2026, 10, 8, 9, 11, 0, 0, time.Local)) {
		t.Fatal("여러 날 대화를 시작한 날로 정렬", at)
	}
}

func TestDayListsWhatWasAskedThatDayWhicheverDayTheConversationBegan(t *testing.T) {
	_, night, morning := twoDayFixture(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	text, err := DayText("2026-10-08", "", 8, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2026-10-08 요청이 있던 대화 2개", filepath.Join(RecordDir(), "2026-10-07") + "/", "세션 " + night, "세션 " + morning, "  - 09:10 아침에 이어서 한 일\n    → AI(주장): 아침 작업을 끝냈습니다.", "  - 08:00 다른 대화 첫 요청\n    → AI(주장): 첫 요청을 했습니다."} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "23:50") || strings.Index(text, morning) > strings.Index(text, night) {
		t.Fatal("다른 날 요청이 섞였거나 그날 먼저 한 대화가 뒤에 옴", text)
	}
	if text, _ := DayText("10-07", "", 8, now); !strings.Contains(text, "  - 23:50 밤에 시작한 일") || strings.Contains(text, "아침에") || strings.Contains(text, morning) {
		t.Fatal("시작한 날의 요청만 골라내지 못함", text)
	}
	if yesterday, _ := DayText("어제", "", 8, now); yesterday != text {
		t.Fatal("어제가 10-08 과 다름", yesterday)
	}
	if text, _ := DayText("2026-10-08", "두번째", 8, now); strings.Contains(text, night) || !strings.Contains(text, "  - 10:00 다른 대화 두번째 요청") || strings.Contains(text, "  - 08:00") {
		t.Fatal("찾는 말로 요청을 거르지 못함", text)
	}
	// Shortened to the last request, the morning conversation still began the day first.
	if text, _ := DayText("2026-10-08", "", 1, now); !strings.Contains(text, "  … 그날 앞 요청 1개 생략 — 전부는 위 기록 파일, 원문은 harness recall source "+morning+" <말>\n  - 10:00 다른 대화 두번째 요청") || strings.Index(text, morning) > strings.Index(text, night) {
		t.Fatal("많은 요청을 줄이며 안내를 빠뜨렸거나 그날 순서가 바뀜", text)
	}
	if text, err := DayText("2026-10-10", "", 8, now); err != nil || text != "그날 기록된 요청이 없다: 2026-10-10" {
		t.Fatal(text, err)
	}
	for _, bad := range []string{"10/08", "2026-13-01", "내일"} {
		if _, err := DayText(bad, "", 8, now); err == nil {
			t.Fatal("잘못된 날짜를 받아들임", bad)
		}
	}
}

func TestDayReadsOlderRecordsWhoseLinesCarryOnlyTheTime(t *testing.T) {
	fixture(t)
	write(t, filepath.Join(RecordDir(), "2026-10-07", "2350-repo-old.md"), `---
title: "옛 기록"
date: 2026-10-07
time: 23:50-09:10
tool: claude
session: old-session
repo: repo
format: 4
---

# 옛 기록

## 내용

### 요청
- 23:50 [a 줄1 claude:typed] 밤 요청
  → AI(주장): 밤 답
- 09:10 [b 줄5 claude:typed] 아침 요청

### 끝난 지점
- 아침 답 (AI 주장)

## 자료
`)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	if text, _ := DayText("2026-10-08", "", 8, now); !strings.Contains(text, "  - 09:10 아침 요청\n    → AI(주장): 아침 답") || strings.Contains(text, "밤 요청") {
		t.Fatal("시각이 줄어든 줄을 다음 날로 보지 못함", text)
	}
	if text, _ := DayText("2026-10-07", "", 8, now); !strings.Contains(text, "  - 23:50 밤 요청\n    → AI(주장): 밤 답") || strings.Contains(text, "아침 요청") {
		t.Fatal(text)
	}
}

func TestDayWordsAlsoMatchTheConversationAndSayWhenTheyMiss(t *testing.T) {
	_, night, morning := twoDayFixture(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	// A repo name narrows the day to whole conversations instead of matching no request.
	text, _ := DayText("2026-10-08", "repo", 8, now)
	for _, want := range []string{"대화 2개", "세션 " + night, "세션 " + morning, "  - 08:00 다른 대화 첫 요청", "  - 10:00 다른 대화 두번째 요청", "  - 09:10 아침에 이어서 한 일"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	if text, _ := DayText("2026-10-08", "밤에 시작한", 8, now); !strings.Contains(text, "  - 09:10 아침에 이어서 한 일") || strings.Contains(text, morning) {
		t.Fatal("제목으로 대화를 고르지 못함", text)
	}
	want := "그날 찾을 말에 맞는 요청이 없다: 2026-10-08 (찾을 말: 없는말) — 말을 빼면 대화 2개: harness recall day 2026-10-08"
	if text, _ := DayText("2026-10-08", "없는말", 8, now); text != want {
		t.Fatal("빗나간 말을 빈 날로 안내", text)
	}
	if text, _ := DayText("2026-10-10", "repo", 8, now); text != "그날 기록된 요청이 없다: 2026-10-10" {
		t.Fatal(text)
	}
}
