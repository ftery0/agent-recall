package recall

import (
	"context"
	"encoding/json"
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

func fakeSummaryExecutable(t *testing.T, body string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".local", "bin", "claude")
	write(t, path, "#!/bin/sh\n"+body+"\n")
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestSummarySubprocessFailuresKeepOnlySafeDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, body, kind string
		timeout          bool
		code             int
	}{
		{name: "start", kind: "start"},
		{name: "exit", body: "cat >/dev/null\nprintf 'private credential value' >&2\nexit 7", kind: "exit", code: 7},
		{name: "response", body: "cat >/dev/null\nprintf 'private credential value'", kind: "response"},
		{name: "timeout", body: "exec sleep 5", kind: "timeout", timeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, path := recordFixture(t)
			if tc.body != "" {
				fakeSummaryExecutable(t, tc.body)
			}
			s, _ := readSession("claude", path)
			ctx := context.Background()
			if tc.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			_, next, err := summarizeWithContext(ctx, s, summary{}, 0)
			var failure *summaryFailure
			if !errors.As(err, &failure) || failure.Kind != tc.kind || next != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal(next, err)
			}
			if tc.kind == "exit" && (failure.ExitCode == nil || *failure.ExitCode != tc.code) {
				t.Fatal(failure)
			}
		})
	}
}

func TestSummaryFakeExecutableReceivesExistingArgsAndIsolatedWorkDir(t *testing.T) {
	_, _, path := recordFixture(t)
	fakeSummaryExecutable(t, `printf '%s\n' "$@" > "$HOME/argv"
printf '%s\n' "$HARNESS_RECALL" > "$HOME/recall-env"
pwd > "$HOME/workdir"
cat >/dev/null
printf '{"title":"가짜 요약"}'`)
	s, _ := readSession("claude", path)
	m, next, err := summarizeNext(s, summary{}, 0)
	if err != nil || m.Title != "가짜 요약" || next != len(fragments(s)) {
		t.Fatal(m, next, err)
	}
	home, _ := os.UserHomeDir()
	argv, _ := os.ReadFile(filepath.Join(home, "argv"))
	if !strings.HasPrefix(string(argv), "-p\n--model\nhaiku\n--no-session-persistence\n--tools\n\n--strict-mcp-config\n--setting-sources\nproject\n--effort\nlow\n") {
		t.Fatal("호출 인자 변경", string(argv))
	}
	env, _ := os.ReadFile(filepath.Join(home, "recall-env"))
	wd, _ := os.ReadFile(filepath.Join(home, "workdir"))
	wantDir, _ := filepath.EvalSymlinks(filepath.Join(Dir(), "work"))
	if string(env) != "1\n" || strings.TrimSpace(string(wd)) != wantDir {
		t.Fatal(string(env), string(wd))
	}
}

func TestFailedSummarySurvivesManualBudgetThenTurnsOffWithoutModel(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	s.summaryState, s.summaryError = "failed", "요약 호출 실패; 기존 기억 유지"
	code := 7
	s.summaryAttempt = &summaryAttempt{At: "prior", RawSHA: s.hash, Cursor: 0, Outcome: "exit", ExitCode: &code}
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	old := summary{Title: "이전 요약", Todo: []string{"남은 확인"}}
	write(t, dest, render(s, old, true))
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	forbidden := func(session, summary, int) (summary, int, error) {
		t.Fatal("모델 금지 또는 하루 예산 소진 뒤 호출")
		return summary{}, 0, nil
	}
	// A manual model run with no budget left keeps the failure: new raw text is not a recovery.
	appendRecordRows(t, path, recordHuman("manual", "새 원문", now.Add(-time.Hour)))
	write(t, filepath.Join(Dir(), "model-calls-2026-10-07"), "30")
	out, err := build(now.AddDate(0, 0, -1), true, now, forbidden)
	if err != nil || out["summary_attempts"] != 0 || out["summarized"] != 0 || out["unresolved"] != 1 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	message, attempt := summaryDiagnostics(raw)
	kept, ok := summaryOf(raw)
	if header(raw, "summary_state") != "pending" || message == "" || attempt == nil || attempt.RawSHA != s.hash || !ok || kept.Title != old.Title || kept.Todo[0] != old.Todo[0] {
		t.Fatal("호출 없이 과거 실패 또는 기존 요약 삭제", string(raw))
	}
	// Sync does not ask the model, so the record holds its fixed fields and earlier summary
	// and nothing waits on, or fails in, a summary.
	appendRecordRows(t, path, recordHuman("sync", "또 새 원문", now.Add(-time.Hour)))
	out, err = build(now.AddDate(0, 0, -1), false, now, forbidden)
	if err != nil || out["summary_attempts"] != 0 || out["unresolved"] != 0 || out["pending"] != 0 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	raw, _ = os.ReadFile(dest)
	message, attempt = summaryDiagnostics(raw)
	kept, ok = summaryOf(raw)
	if header(raw, "summary_state") != "off" || message != "" || attempt != nil || !ok || kept.Title != old.Title || kept.Todo[0] != old.Todo[0] || strings.Contains(string(raw), "요약 갱신:") || !strings.Contains(string(raw), "또 새 원문") {
		t.Fatal("모델 없는 수집이 대기·실패를 남기거나 기존 요약을 지움", string(raw))
	}
	if err := SaveSyncStatus(nil, out, nil); err != nil || SyncWarning() != "" {
		t.Fatal(err, SyncWarning())
	}
}

func TestStoredPartialSuccessRecoversFailureAndRetainsCursorOnNextFailure(t *testing.T) {
	_, _, path := recordFixture(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	appendRecordRows(t, path, recordHuman("second", "다음 구간", now.Add(-time.Hour)))
	s, _ := readSession("claude", path)
	s.summaryState, s.summaryError = "failed", "요약 호출 실패; 기존 기억 유지"
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	write(t, dest, render(s, summary{Title: "이전 기억", Todo: []string{"남은 일"}}, true))
	out, err := build(now.AddDate(0, 0, -1), true, now, func(s session, old summary, cursor int) (summary, int, error) {
		return mergeSummary(s, old, summary{Todo: []string{"부분 확인"}}), cursor + 1, nil
	})
	if err != nil || out["summarized"] != 1 || out["pending"] != 1 || out["unresolved"] != 0 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	message, attempt := summaryDiagnostics(raw)
	if header(raw, "summary_state") != "pending" || header(raw, "summary_cursor") != "1" || message != "" || attempt == nil || attempt.Outcome != "success" {
		t.Fatal("부분 성공을 실패 또는 전체 완료로 표시", string(raw))
	}
	out, err = build(now.AddDate(0, 0, -1), true, now, func(_ session, _ summary, cursor int) (summary, int, error) {
		if cursor != 1 {
			t.Fatal("부분 저장 뒤 커서 재설정", cursor)
		}
		return summary{}, cursor, errors.New("private credential value")
	})
	if err != nil || out["summarized"] != 0 || out["unresolved"] != 1 {
		t.Fatal(out, err)
	}
	raw, _ = os.ReadFile(dest)
	var savedAttempt summaryAttempt
	if json.Unmarshal([]byte(header(raw, "last_summary_attempt")), &savedAttempt) != nil || savedAttempt.Cursor != 1 || savedAttempt.Outcome != "unknown" || header(raw, "summary_cursor") != "1" || strings.Contains(string(raw), "private credential value") {
		t.Fatal(string(raw))
	}
	kept, ok := summaryOf(raw)
	if !ok || len(kept.Todo) != 2 {
		t.Fatal("다음 실패가 부분 저장 기억 삭제", kept)
	}
}

func TestLegacyFailureKeepsUnknownAttemptBoundToPreviousInput(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	s.summaryState, s.summaryError = "failed", "요약 호출 실패; 기존 기억 유지"
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	write(t, dest, render(s, summary{Title: "구형 요약"}, true))
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	appendRecordRows(t, path, recordHuman("new", "추가 원문", now.Add(-time.Hour)))
	write(t, filepath.Join(Dir(), "model-calls-2026-10-07"), "30")
	out, err := build(now.AddDate(0, 0, -1), true, now, func(session, summary, int) (summary, int, error) {
		t.Fatal("구형 기록 확인 중 모델 호출")
		return summary{}, 0, nil
	})
	if err != nil || out["unresolved"] != 1 || out["summary_attempts"] != 0 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	message, attempt := summaryDiagnostics(raw)
	if message == "" || attempt == nil || attempt.RawSHA != s.hash || attempt.At != "" || attempt.Outcome != "unknown" || header(raw, "summary_state") != "pending" {
		t.Fatal("과거 시각·원인을 추정하거나 이전 입력 근거 소실", string(raw))
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
			out, err := buildWithRecordWrite(now.AddDate(0, 0, -1), true, now,
				func(s session, old summary, cursor int) (summary, int, error) {
					return mergeSummary(s, old, summary{Title: "새 기억"}), len(fragments(s)), nil
				}, func(path string, raw []byte) error {
					if replaced {
						if err := memory.PrivateWrite(path, raw); err != nil {
							return err
						}
					}
					return errors.New("synthetic storage failure with private details")
				}, "")
			if err != nil || out["summarized"] != 0 || out["written"] != 0 || out["summary_attempts"] != 1 || out["storage_uncertain"] != true {
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

func TestSummaryChunksCoverLongAndRepeatedRequests(t *testing.T) {
	s := session{cwd: "/repo"}
	for i := 0; i < 40; i++ {
		s.requests = append(s.requests, worklog.Request{ID: fmt.Sprint(i), Line: i + 1, Text: strings.Repeat("긴 사용자 원문 ", 500), Context: strings.Repeat("직전 설명 ", 200)})
	}
	old := summary{Title: "계속", Todo: []string{strings.Repeat("기존 기억 ", 20000)}}
	for i := 0; i < 15; i++ {
		s.commands = append(s.commands, command{source: strings.Repeat("원문ID", 60), text: strings.Repeat("검사 명령 ", 200), result: "성공"})
	}
	s.reports = []string{strings.Repeat("보고 ", 500), strings.Repeat("보고 ", 500)}
	parts := fragments(s)
	cursor, runs := 0, 0
	for cursor < len(parts) {
		text, end, err := nextInput(s, old, cursor)
		if err != nil || end <= cursor || len([]rune(text)) > modelInput {
			t.Fatal(cursor, end, err, len([]rune(text)))
		}
		cursor, runs = end, runs+1
	}
	if runs < 2 || cursor != len(parts) {
		t.Fatal(runs, cursor)
	}
	for _, r := range s.requests {
		var rebuilt string
		for _, p := range parts {
			if p.ID == r.ID {
				rebuilt += p.Text
			}
		}
		if rebuilt != r.Text {
			t.Fatal("원문 중간 유실", r.ID)
		}
	}
}

func TestDailyBudgetPreservesPendingAndRecoversNextDay(t *testing.T) {
	_, _, _ = recordFixture(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if err := memory.PrivateWrite(filepath.Join(Dir(), "model-calls-2026-10-07"), []byte("30")); err != nil {
		t.Fatal(err)
	}
	calls := 0
	call := func(s session, old summary, cursor int) (summary, int, error) {
		calls++
		return summary{Title: "예산 대기 작업"}, len(fragments(s)), nil
	}
	out, err := build(now.AddDate(0, 0, -1), true, now, call)
	if err != nil || calls != 0 || out["written"] != 1 || out["pending"] != 1 {
		t.Fatal(out, err, calls)
	}
	out, err = build(now.AddDate(0, 0, -1), true, now.AddDate(0, 0, 1), call)
	if err != nil || calls != 1 || out["summarized"] != 1 || out["pending"] != 0 {
		t.Fatal("예산 소진이 영구 누락됨", out, err, calls)
	}
}

func TestLessonFailureRetriesWithoutLosingStoredSummary(t *testing.T) {
	_, _, _ = recordFixture(t)
	write(t, lessonsPath(), "{broken")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	out, err := build(now.AddDate(0, 0, -1), true, now, func(s session, old summary, cursor int) (summary, int, error) {
		return summary{Title: "교훈 복구", Lessons: []string{"기억은 실제 원문으로 확인한다"}}, len(fragments(s)), nil
	})
	if err != nil || out["written"] != 1 || len(out["failed"].([]string)) != 1 {
		t.Fatal(out, err)
	}
	dest := out["files"].([]string)[0]
	raw, _ := os.ReadFile(dest)
	if header(raw, "lessons_pending") != "true" {
		t.Fatal("후처리 실패가 재시도에서 누락", string(raw))
	}
	write(t, lessonsPath(), "[]")
	out, err = Build(now.AddDate(0, 0, -1), false, now)
	if err != nil || out["written"] != 1 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	raw, _ = os.ReadFile(dest)
	m, ok := summaryOf(raw)
	if header(raw, "lessons_pending") != "false" || !ok || m.Title != "교훈 복구" {
		t.Fatal(string(raw))
	}
	all, err := loadLessons()
	if err != nil || len(all) != 1 || all[0].Status != "후보" {
		t.Fatal(all, err)
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

func TestBuildFailureKeepsSummaryAcrossRetriesThenRecovers(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	old := summary{Title: "기존 목표", Decisions: []decision{{What: "묶음 훅 미등록", Why: "효과 미확인"}}, Todo: []string{"실제 이어받기 확인"}}
	dest := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-abcdef12.md")
	write(t, dest, render(s, old, true))
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","uuid":"new","promptSource":"typed","timestamp":"2026-10-07T02:00:00Z","message":{"role":"user","content":"새 결정도 보존"}}` + "\n")
	f.Close()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 2; i++ {
		out, err := build(now.AddDate(0, 0, -1), true, now, func(session, summary, int) (summary, int, error) { return summary{}, 0, errors.New("합성 실패") })
		if err != nil || out["pending"] != 1 || len(out["failed"].([]string)) != 1 {
			t.Fatal(out, err)
		}
		raw, _ := os.ReadFile(dest)
		kept, ok := summaryOf(raw)
		if !ok || kept.Decisions[0].Why != old.Decisions[0].Why || kept.Todo[0] != old.Todo[0] {
			t.Fatal("실패 반복 후 이전 기억 소실", string(raw))
		}
	}
	// Discovery starts after the session, but its failed record is still eligible.
	out, err := build(now.AddDate(0, 0, 1), true, now, func(s session, prior summary, cursor int) (summary, int, error) {
		return mergeSummary(s, prior, summary{Title: "기존 목표", Todo: []string{"새 결정도 확인"}}), len(fragments(s)), nil
	})
	if err != nil || out["summarized"] != 1 || out["pending"] != 0 {
		t.Fatal(out, err)
	}
	raw, _ := os.ReadFile(dest)
	kept, _ := summaryOf(raw)
	if len(kept.Todo) != 2 || len(kept.Decisions) != 1 || header(raw, "summary_state") != "ready" {
		t.Fatal(string(raw))
	}
}

func TestBuildDetectsSameSizeEditAndShortIDCollision(t *testing.T) {
	_, _, path := recordFixture(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	if _, err := Build(now.AddDate(0, 0, -1), false, now); err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.ReplaceAll(claudeSession, "헤더", "푸터"))
	out, err := Build(now.AddDate(0, 0, -1), false, now)
	if err != nil || out["written"] != 1 {
		t.Fatal("같은 크기 수정 누락", out, err)
	}
	other := filepath.Join(filepath.Dir(path), "abcdef12-1111-0000-0000-000000000000.jsonl")
	write(t, other, strings.ReplaceAll(claudeSession, "abcdef12-0000-0000-0000-000000000000", "abcdef12-1111-0000-0000-000000000000"))
	out, err = Build(now.AddDate(0, 0, -1), false, now)
	indexed, indexErr := existing()
	if err != nil || indexErr != nil || out["written"] != 1 || len(indexed) != 2 {
		t.Fatal("짧은 ID 충돌로 기록 덮음", out, err, indexed)
	}
}

func TestMalformedSessionIsRetryableAndNoSecretInSummary(t *testing.T) {
	_, _, path := recordFixture(t)
	write(t, path, claudeSession+"{broken\n")
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	out, err := Build(now.AddDate(0, 0, -1), false, now)
	if err != nil || len(out["failed"].([]string)) != 1 || out["written"] != 0 {
		t.Fatal(out, err)
	}
	write(t, path, claudeSession)
	out, err = Build(now.AddDate(0, 0, -1), false, now)
	if err != nil || out["written"] != 1 {
		t.Fatal("해석 오류가 영구 제외됨", out, err)
	}
	s, _ := readSession("claude", path)
	if text, _, _ := nextInput(s, summary{}, 0); strings.Contains(text, "abcdef123456") {
		t.Fatal("비밀값을 모델 입력으로 보냄")
	}
}
