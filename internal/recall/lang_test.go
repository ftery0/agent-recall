package recall

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLangMarkerDoesNotProveKoreanAndCodeIsUnjudged(t *testing.T) {
	s := judgeSummary("요약 언어: 한국어\nThe work has finished and every test has passed successfully.", time.Time{}, "s", "v")
	if !s.marked || !s.judged || !s.en {
		t.Fatal("표식이 영어 본문 판정을 뒤집음", s)
	}
	s = judgeSummary("요약 언어: 한국어\n```go\npackage main\nfunc main() {}\n```", time.Time{}, "s", "v")
	if s.judged {
		t.Fatal("코드만 있는 요약을 한국어로 집계", s)
	}
	if text := langTable(1, nil, []langSummary{s}); !strings.Contains(text, "판정 불가 1개") || !strings.Contains(text, "설명이 한국어 0개") {
		t.Fatal(text)
	}
}

func TestLangSummaryWithoutBoundaryAndPreciseSince(t *testing.T) {
	_, raw := fixture(t)
	p := filepath.Join(raw, "claude", "2026-10-07", "abcdef12.jsonl")
	write(t, p, jsonLines(t,
		said("2026-10-07T01:00:00Z", "Reading the first document and checking the output.", true),
		map[string]any{"type": "user", "isCompactSummary": true, "timestamp": "2026-10-07T02:00:00Z", "message": map[string]any{"content": "요약 언어: 한국어\n사용자가 선택한 결정과 남은 작업을 보존했다."}},
		said("2026-10-07T02:01:00Z", "Checking the remaining task after the context has been compacted.", true),
	))
	blocks, summaries, err := langRead(p, time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC))
	if err != nil || len(blocks) != 1 || !blocks[0].compacted || len(summaries) != 1 {
		t.Fatal(blocks, summaries, err)
	}
	if _, _, err := langRead(p+"missing", time.Time{}); err == nil {
		t.Fatal("파일 읽기 실패 숨김")
	}
	write(t, p, "{broken\n")
	if _, err := Lang([]string{p}, time.Time{}, 0); err == nil {
		t.Fatal("손상 파일이 0개 성공으로 집계")
	}
}
