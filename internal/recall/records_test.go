package recall

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func summaryOf(raw []byte) (summary, bool) { return record{raw: raw, head: frontMatter(raw)}.summary() }

func TestRecordReadsItsFieldsSummaryAndSections(t *testing.T) {
	raw := []byte("---\ntitle: \"첫 요청\"\ndate: 2026-10-07\ntime: 23:50-01:10\nsession: s1\nsummary: haiku\n---\n# 첫 요청\n\nsession: 본문 줄\n\n## 내용\n\n" +
		"### 요청\n- 10-07 23:50 [a 줄3 claude:typed] 첫 요청\n  → AI(주장): 답\n\n### 결정\n- 묶음 저장 — 빠르다\n\n### 남은 일\n- 확인\n\n" +
		"### 기억 명령\n- 10-08 00:10 `harness recall find x` → 빈 결과 [tool:t1]\n\n### 바뀐 파일\n- a.go\n")
	rec := record{raw: raw, head: frontMatter(raw)}
	if rec.field("session") != "s1" || header(raw, "session") != "s1" || header(raw, "time") != "23:50-01:10" {
		t.Fatal("머리말을 본문 줄로 읽음", rec.head)
	}
	if want := time.Date(2026, 10, 7, 23, 50, 0, 0, time.Local); !rec.started().Equal(want) {
		t.Fatal("시작 시각이 틀림", rec.started())
	}
	m, ok := rec.summary()
	if !ok || m.Title != "첫 요청" || len(m.Decisions) != 1 || m.Decisions[0] != (decision{What: "묶음 저장", Why: "빠르다"}) || !slices.Equal(m.Todo, []string{"확인"}) {
		t.Fatal("옛 요약 절을 읽지 못함", m, ok)
	}
	if got := rec.items("바뀐 파일"); !slices.Equal(got, []string{"a.go"}) {
		t.Fatal("절 항목을 읽지 못함", got)
	}
	if body := withoutSection(string(raw), "기억 명령"); strings.Contains(body, "recall find") || !strings.Contains(body, "### 바뀐 파일\n- a.go") {
		t.Fatal("한 절만 빼지 못함", body)
	}
	if requests := rec.requests(); len(requests) != 1 || requests[0].id != "a" || requests[0].line != 3 || requests[0].answer != "답" {
		t.Fatal("요청 줄을 읽지 못함", requests)
	}
	data := []byte("---\ntitle: \"x\"\nsummary_data: {\"title\":\"모델 제목\",\"todo\":[\"남김\"]}\n---\n# x\n")
	if m, ok := summaryOf(data); !ok || m.Title != "모델 제목" || m.Todo[0] != "남김" {
		t.Fatal("모델 요약 JSON 을 읽지 못함", m, ok)
	}
	if _, ok := summaryOf([]byte("---\ntitle: \"x\"\nsummary: 없음\n---\n# x\n")); ok {
		t.Fatal("모델 요약이 없는 기록을 요약으로 읽음")
	}
	if (record{}).started() != (time.Time{}) {
		t.Fatal("날짜 없는 기록에 시작 시각을 만듦")
	}
}
