package recall

import (
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFailedSummaryTurnsOffAndKeepsStoredSummary(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	s.summaryState, s.summaryError = "failed", "요약 호출 실패; 기존 기억 유지"
	code := 7
	s.summaryAttempt = &summaryAttempt{At: "prior", RawSHA: s.hash, Cursor: 0, Outcome: "exit", ExitCode: &code}
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	old := summary{Title: "이전 요약", Todo: []string{"남은 확인"}}
	write(t, dest, render(s, old, true))
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	// Sync does not ask the model, so the record holds its fixed fields and earlier summary
	// and nothing waits on, or fails in, a summary.
	appendRecordRows(t, path, recordHuman("sync", "또 새 원문", now.Add(-time.Hour)))
	out, err := Build(now.AddDate(0, 0, -1), now)
	if err != nil || out["unresolved"] != 0 || out["pending"] != 0 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	message, attempt := summaryDiagnostics(raw)
	kept, ok := summaryOf(raw)
	if header(raw, "summary_state") != "off" || message != "" || attempt != nil || !ok || kept.Title != old.Title || kept.Todo[0] != old.Todo[0] || strings.Contains(string(raw), "요약 갱신:") || !strings.Contains(string(raw), "또 새 원문") {
		t.Fatal("모델 없는 수집이 대기·실패를 남기거나 기존 요약을 지움", string(raw))
	}
	if err := SaveSyncStatus(nil, out, nil); err != nil || SyncWarning() != "" {
		t.Fatal(err, SyncWarning())
	}
}

func TestSummaryWriteFailureBeforeAndAfterReplacementIsNotReportedAsSaved(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(fmt.Sprint(replaced), func(t *testing.T) {
			_, _, path := recordFixture(t)
			s, _ := readSession("claude", path)
			s.summaryState, s.summaryError = "failed", "요약 호출 실패; 기존 기억 유지"
			dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
			before := render(s, summary{Title: "기존 기억", Todo: []string{"기존 남은 일"}}, true)
			write(t, dest, before)
			now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
			appendRecordRows(t, path, recordHuman("new", "새 원문", now.Add(-time.Hour)))
			out, err := buildWithRecordWrite(now.AddDate(0, 0, -1), now,
				func(path string, raw []byte) error {
					if replaced {
						if err := memory.PrivateWrite(path, raw); err != nil {
							return err
						}
					}
					return errors.New("synthetic storage failure with private details")
				}, "")
			if err != nil || out["written"] != 0 || out["storage_uncertain"] != true {
				t.Fatal(out, err)
			}
			wantPending := 1
			if replaced {
				wantPending = 0
			}
			if out["pending"] != wantPending || out["unresolved"] != wantPending {
				t.Fatal("집계가 실제 저장 기록과 다름", out)
			}
			after, _ := os.ReadFile(dest)
			kept, ok := summaryOf(after)
			if !ok || kept.Todo[0] != "기존 남은 일" || (!replaced && string(after) != before) {
				t.Fatal("저장 오류로 기존 기억 소실", string(after))
			}
			if err := SaveSyncStatus(nil, out, nil); err != nil || !strings.Contains(SyncWarning(), "이번 수집 실패 1건") || !strings.Contains(SyncWarning(), "저장 결과 미확인") || strings.Contains(SyncWarning(), "private details") {
				t.Fatal(err, SyncWarning())
			}
		})
	}
}

func TestSummaryChoiceCannotInvertNegativeAnswer(t *testing.T) {
	s := session{requests: []worklog.Request{{ID: "choice", Line: 3, Source: "claude:choice", Text: "질문: 안내 훅을 어떻게 할까요?\n선택: 등록하지 않음"}}}
	bad := mergeSummary(s, summary{}, summary{Decisions: []decision{{What: "등록한다", Source: "choice", Evidence: "등록"}}})
	if bad.Decisions[0].Status != "추정" {
		t.Fatal(bad)
	}
	good := mergeSummary(s, summary{}, summary{Decisions: []decision{{What: "등록한다", Source: "choice", Evidence: "등록하지 않음"}}})
	if good.Decisions[0].What != s.requests[0].Text {
		t.Fatal("선택 결과를 모델이 뒤집음", good)
	}
}

func TestRewrittenEvidenceDoesNotKeepOldClosureAsVerified(t *testing.T) {
	old := summary{Title: "시험", Decisions: []decision{{What: "시험", Source: "u", Evidence: "시험 완료", Status: "사용자"}}, Todo: []string{"시험"}, Resolutions: []resolution{{What: "시험", Kind: "todo", State: "완료", Source: "u", Evidence: "시험 완료"}}}
	s := session{requests: []worklog.Request{{ID: "u", Line: 1, Text: "시험 미완료"}}}
	m := mergeSummary(s, old, summary{})
	if _, closed := resolved(m, "todo", "시험"); closed || !m.Resolutions[0].Stale || m.Decisions[0].Status == "사용자" {
		t.Fatal("수정된 원문으로 과거 완료를 확정", m)
	}
	if len(m.Decisions) != 1 || len(m.Resolutions) != 1 || m.Resolutions[0].State != "완료" {
		t.Fatal("과거 이력을 삭제함", m)
	}
}

func TestSummaryClosureNeedsTargetAndLatestUserEvidence(t *testing.T) {
	s := session{requests: []worklog.Request{{ID: "a", Line: 1, Text: "시험 취소"}, {ID: "b", Line: 2, Text: "시험 다시 재개"}, {ID: "c", Line: 3, Text: "응"}}}
	m := summary{Title: "시험", Todo: []string{"시험"}}
	change := func(id, state, evidence string) {
		m = mergeSummary(s, m, summary{Resolutions: []resolution{{What: "시험", Kind: "todo", State: state, Source: id, Evidence: evidence}}})
	}
	change("c", "완료", "응")
	if _, closed := resolved(m, "todo", "시험"); closed {
		t.Fatal("짧은 동의를 완료로 추정", m)
	}
	s.requests = append(s.requests, worklog.Request{ID: "negative", Line: 4, Text: "시험 완료 못했어"})
	change("negative", "완료", "시험 완료")
	change("negative", "완료", "시험 완료 못했어")
	if _, closed := resolved(m, "todo", "시험"); closed {
		t.Fatal("부정 원문의 일부 인용으로 완료를 확정", m)
	}
	change("a", "취소", "시험 취소")
	if _, closed := resolved(m, "todo", "시험"); !closed {
		t.Fatal("명시적 취소 누락", m)
	}
	change("b", "재개", "시험 다시 재개")
	change("a", "취소", "시험 취소")
	if _, closed := resolved(m, "todo", "시험"); closed {
		t.Fatal("옛 원문 재처리가 최신 재개를 덮음", m)
	}
	s.commands = []command{{source: "tool:t", text: "go test ./...", result: "성공"}}
	m = mergeSummary(s, m, summary{Resolutions: []resolution{{What: "시험", Kind: "todo", State: "완료", Source: "tool:t", Evidence: "go test ./..."}}})
	if _, closed := resolved(m, "todo", "시험"); closed {
		t.Fatal("검사 하나로 전체 작업 완료", m)
	}
}

func TestBuildDetectsSameSizeEditAndShortIDCollision(t *testing.T) {
	_, _, path := recordFixture(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if _, err := Build(now.AddDate(0, 0, -1), now); err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.ReplaceAll(claudeSession, "헤더", "푸터"))
	out, err := Build(now.AddDate(0, 0, -1), now)
	if err != nil || out["written"] != 1 {
		t.Fatal("같은 크기 수정 누락", out, err)
	}
	other := filepath.Join(filepath.Dir(path), "abcdef12-1111-0000-0000-000000000000.jsonl")
	write(t, other, strings.ReplaceAll(claudeSession, "abcdef12-0000-0000-0000-000000000000", "abcdef12-1111-0000-0000-000000000000"))
	out, err = Build(now.AddDate(0, 0, -1), now)
	indexed, indexErr := existing()
	if err != nil || indexErr != nil || out["written"] != 1 || len(indexed) != 2 {
		t.Fatal("짧은 ID 충돌로 기록 덮음", out, err, indexed)
	}
}

func TestMalformedSessionIsRetryableAndNoSecretInSummary(t *testing.T) {
	_, _, path := recordFixture(t)
	write(t, path, claudeSession+"{broken\n")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	out, err := Build(now.AddDate(0, 0, -1), now)
	if err != nil || len(out["failed"].([]string)) != 1 || out["written"] != 0 {
		t.Fatal(out, err)
	}
	write(t, path, claudeSession)
	out, err = Build(now.AddDate(0, 0, -1), now)
	if err != nil || out["written"] != 1 {
		t.Fatal("해석 오류가 영구 제외됨", out, err)
	}
	if raw, _ := os.ReadFile(out["files"].([]string)[0]); strings.Contains(string(raw), "abcdef123456") {
		t.Fatal("비밀값을 기록에 남김")
	}
}
