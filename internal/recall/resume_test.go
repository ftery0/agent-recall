package recall

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func resumeFixture(t *testing.T, cwd, id, state string, m summary) string {
	t.Helper()
	s := session{tool: "claude", id: id, path: filepath.Join(t.TempDir(), "raw.jsonl"), cwd: cwd, start: time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC), end: time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC), requests: []worklog.Request{{Text: "작업 요청", ID: "original", Line: 1, Source: "claude:typed"}}, summaryState: state}
	write(t, s.path, jsonLines(t, map[string]any{"type": "user", "cwd": cwd, "uuid": "original", "promptSource": "typed", "message": map[string]any{"content": "작업 요청"}}))
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	s.size = info.Size()
	path := filepath.Join(RecordDir(), "2026-10-07", "1000-repo-"+short(id)+".md")
	write(t, path, render(s, m, true))
	return path
}

func resumeTranscript(t *testing.T, cwd string, rows ...map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "current.jsonl")
	first := map[string]any{"type": "session_meta", "payload": map[string]any{"cwd": cwd}}
	write(t, path, jsonLines(t, append([]map[string]any{first}, rows...)...))
	return path
}

func TestChoiceExcerptKeepsHookIdentityAndQuestionOrder(t *testing.T) {
	question := "예비시험에서 효과를 판정하지 못한 도구 묶음 뒤 한국어 안내(PostToolBatch)를 어떻게 할까요?"
	requests := []worklog.Request{
		{ID: "first", Line: 2, QuestionID: "q", Text: "질문: " + question + "\n선택: 등록하지 않음"},
		{ID: "second", Line: 2, QuestionID: "q", Text: "질문: 다른 점검은?\n선택: 중복만 수정"},
	}
	text := requestView(requests, 4, 180)
	if !strings.Contains(text, "PostToolBatch") || !strings.Contains(text, "등록하지 않음") || strings.Index(text, "first") > strings.Index(text, "second") {
		t.Fatal("질문 대상 잘림 또는 한 선택창 안의 질문 순서 역전", text)
	}
}

func resumeHuman(id, text string) map[string]any {
	return map[string]any{"type": "user", "uuid": id, "promptSource": "typed", "message": map[string]any{"content": text}}
}

func TestResumeReadsImmediateArchiveBeforeAnySummaryAndSkipsSyntheticConversations(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	path := filepath.Join(RawDir(), "claude", "2025-01-01", "immediate-session.jsonl")
	row := resumeHuman("latest-choice", "PostToolBatch는 등록하지 않음")
	row["cwd"], row["sessionId"], row["timestamp"] = cwd, "immediate-session", time.Now().UTC().Format(time.RFC3339Nano)
	write(t, path, jsonLines(t, row))
	synthetic := resumeHuman("sdk", "자동 시험을 등록한다")
	synthetic["cwd"], synthetic["promptSource"], synthetic["timestamp"] = cwd, "sdk", time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	write(t, filepath.Join(RawDir(), "claude", "2026-10-07", "synthetic.jsonl"), jsonLines(t, synthetic))
	other := resumeHuman("other", "다른 프로젝트의 작업")
	other["cwd"], other["timestamp"] = t.TempDir(), time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
	write(t, filepath.Join(RawDir(), "claude", "2026-10-07", "unrelated.jsonl"), jsonLines(t, other))
	card, err := ResumeText(cwd, "")
	if err != nil || !strings.Contains(card, "- 목표/제목: PostToolBatch는 등록하지 않음") || !strings.Contains(card, "latest-choice 줄1") || !strings.Contains(card, path) || strings.Contains(card, "요약 갱신") || !strings.Contains(card, "source immediate-session") {
		t.Fatalf("immediate raw-only continuation missing: %s error=%v", card, err)
	}
	if strings.Contains(card, "자동 시험") || strings.Contains(card, "다른 프로젝트") {
		t.Fatal("scripted or unrelated input mixed into card", card)
	}
	if records, _ := filepath.Glob(filepath.Join(RecordDir(), "*", "*.md")); len(records) != 0 {
		t.Fatal("read-only continuation wrote records", records)
	}
}

func TestResumeDoesNotDuplicateCurrentArchiveAlias(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	row := resumeHuman("current-choice", "이번 원문은 한 번만 보여야 한다")
	row["cwd"], row["sessionId"] = cwd, "current-session"
	current := filepath.Join(t.TempDir(), "current-session.jsonl")
	write(t, current, jsonLines(t, row))
	write(t, filepath.Join(RawDir(), "claude", "2026-10-07", "current-session.jsonl"), jsonLines(t, row))
	card, err := ResumeText(cwd, current)
	if err != nil || strings.Count(card, "이번 원문은 한 번만") != 1 || strings.Contains(card, "보관 원본:") {
		t.Fatalf("current transcript and archive were duplicated: %s error=%v", card, err)
	}
}

func TestResumeDoesNotDuplicateCurrentInputAfterArchiveHasARecord(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	path := resumeFixture(t, cwd, "current-session", "pending", summary{Title: "현재 작업", Todo: []string{"실제 검증"}})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	row := resumeHuman("current-choice", "이 선택은 현재 입력으로 한 번만 보여줘")
	row["cwd"], row["sessionId"] = cwd, "current-session"
	write(t, header(data, "raw"), jsonLines(t, row))
	current := filepath.Join(t.TempDir(), "current-session.jsonl")
	write(t, current, jsonLines(t, row))
	card, err := ResumeText(cwd, current)
	if err != nil || strings.Count(card, "이 선택은 현재 입력으로 한 번만") != 1 || !strings.Contains(card, "실제 검증") {
		t.Fatalf("summarized archive duplicated current input or lost prior work: %s error=%v", card, err)
	}
}

func TestResumeLatestQuestionAnswerCanSelectTaskWithoutUsingQuestionWording(t *testing.T) {
	records := []resumeRecord{
		{raw: "a", m: summary{Title: "언어 설정 개선"}},
		{raw: "b", m: summary{Title: "원본 저장 개선"}},
	}
	requests := []worklog.Request{
		{Text: "언어 설정 개선 작업 이어가줘"},
		{QuestionID: "which", Text: "질문: 언어 설정 개선 대신 어느 작업?\n선택: 원본 저장 개선"},
	}
	got, ok := requestedRecord(records, requests)
	if !ok || got.raw != "b" {
		t.Fatalf("latest question answer lost to older typed request or question: %+v %v", got, ok)
	}
	requests[1].Text = "질문: 언어 설정 개선 대신 어느 작업?\n선택: 다른 작업"
	if _, ok := requestedRecord(records, requests); ok {
		t.Fatal("unclear latest choice narrowed candidates using the older request")
	}
}

func TestResumeBudgetWithLongPathsKeepsSourceIDsAndLatestInput(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	long := t.TempDir()
	for i := 0; i < 8; i++ {
		long = filepath.Join(long, strings.Repeat("p", 95))
	}
	t.Setenv("HARNESS_HOME", long)
	first, second := "aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002"
	resumeFixture(t, cwd, first, "pending", summary{Title: "언어 설정", Todo: []string{"검증"}})
	resumeFixture(t, cwd, second, "pending", summary{Title: "원문 저장", Todo: []string{"검증"}})
	current := filepath.Join(long, "current.jsonl")
	write(t, current, jsonLines(t, map[string]any{"type": "session_meta", "payload": map[string]any{"cwd": cwd}}, resumeHuman("latest-human", "PostToolBatch는 등록하지 않음")))
	card, err := ResumeText(cwd, current)
	if err != nil || len([]rune(card)) > 2000 || !strings.Contains(card, current) || !strings.Contains(card, "PostToolBatch는 등록하지 않음") || !strings.Contains(card, "source "+first) || !strings.Contains(card, "source "+second) {
		t.Fatalf("long paths exceeded budget or lost lookup/latest request (%d): %s error=%v", len([]rune(card)), card, err)
	}
}

func TestResumeReportsUnreadableRawAndCurrentEvenWithoutAnyRecords(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	path := filepath.Join(RawDir(), "claude", "2026-10-07", "unreadable.jsonl")
	write(t, path, "{}\n")
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0600) })
	card, err := ResumeText(cwd, "")
	if err == nil || card != "" || !strings.Contains(err.Error(), path) {
		t.Fatalf("unreadable raw silently disappeared: %s error=%v", card, err)
	}
	_, err = ResumeText(cwd, filepath.Join(cwd, "missing.jsonl"))
	if err == nil || !strings.Contains(err.Error(), "missing.jsonl") {
		t.Fatalf("missing current path silently disappeared: %v", err)
	}
}

func TestResumeRevivesClosedRecordOnlyWhenItsRawChanges(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	m := summary{Title: "이전에는 끝난 작업", Todo: []string{"검사"}, Resolutions: []resolution{{Kind: "todo", What: "검사", State: "완료", Source: "user:done"}}}
	path := resumeFixture(t, cwd, "closed-session", "ready", m)
	card, err := ResumeText(cwd, "")
	if err != nil || card != "" {
		t.Fatalf("unchanged explicitly closed record revived: %s error=%v", card, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := header(data, "raw")
	f, err := os.OpenFile(raw, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(jsonLines(t, resumeHuman("reopened", "새로운 문제가 있으니 다시 고쳐줘")))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	card, err = ResumeText(cwd, "")
	if err != nil || !strings.Contains(card, "새로운 문제가 있으니 다시 고쳐줘") || !strings.Contains(card, "명시된 닫힘") {
		t.Fatalf("changed raw did not revive closed history safely: %s error=%v", card, err)
	}
}

func TestResumeShowsLatestHumanChoiceBeforeOldPlanAndKeepsUnverifiedWork(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	record := resumeFixture(t, cwd, "11111111-0000-0000-0000-000000000000", "pending", summary{Title: "언어 문제 개선", Decisions: []decision{{What: "PostToolBatch를 등록한다", Why: "예비시험 계획", Status: "추정"}}, Todo: []string{"실제 새 세션 이어가기 검증"}})
	question := "PostToolBatch를 어떻게 할까요?"
	current := resumeTranscript(t, cwd,
		resumeHuman("actual-human", "언어 문제 작업 이어가자"),
		map[string]any{"type": "assistant", "uuid": "asked", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "ask", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"question": question}}}}}}},
		map[string]any{"type": "user", "uuid": "final-choice", "version": "2.1.292", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "ask"}}}, "toolUseResult": map[string]any{"answers": map[string]any{question: "등록하지 않음"}}},
		map[string]any{"type": "assistant", "message": map[string]any{"content": "전체 기억 개선은 완료했습니다."}},
		map[string]any{"type": "user", "promptSource": "sdk", "message": map[string]any{"content": "예비시험 결정: PostToolBatch를 등록한다"}},
	)
	card, err := ResumeText(cwd, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"등록하지 않음", "final-choice:ask:0 줄4", "실제 새 세션 이어가기 검증", "요약·근거 미확인", record} {
		if !strings.Contains(card, want) {
			t.Fatalf("missing %q: %s", want, card)
		}
	}
	if strings.Index(card, "등록하지 않음") > strings.Index(card, "PostToolBatch를 등록한다") || strings.Contains(card, "전체 기억 개선은 완료") || strings.Contains(card, "예비시험 결정") {
		t.Fatalf("old, reported, or scripted state overrode the final human choice: %s", card)
	}
}

func TestResumeKeepsSameFolderTasksAmbiguousUntilRequestNamesOne(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "11111111-0000-0000-0000-000000000000", "ready", summary{Title: "언어 설정 개선", Todo: []string{"수동 압축 시험"}})
	resumeFixture(t, cwd, "22222222-0000-0000-0000-000000000000", "ready", summary{Title: "원본 저장 개선", Todo: []string{"중단 복구 시험"}})
	current := resumeTranscript(t, cwd, resumeHuman("yes", "응"))
	card, err := ResumeText(cwd, current)
	if err != nil || !strings.Contains(card, "이 레포 대화 2개") || !strings.Contains(card, "언어 설정 개선") || !strings.Contains(card, "원본 저장 개선") || strings.Contains(card, "사용자 요청의 제목과 일치") {
		t.Fatalf("recency or confirmation incorrectly selected a task: card=%s error=%v", card, err)
	}
	current = resumeTranscript(t, cwd, resumeHuman("specific", "원본 저장 작업 이어가자"))
	card, err = ResumeText(cwd, current)
	if err != nil || !strings.Contains(card, "원본 저장 개선") || strings.Contains(card, "언어 설정 개선") || !strings.Contains(card, "사용자 요청의 제목과 일치") {
		t.Fatalf("explicit task request was not used: card=%s error=%v", card, err)
	}
}

func TestResumeWorksOutsideRepositoryAndDoesNotMixUnrelatedFolders(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "33333333-0000-0000-0000-000000000000", "ready", summary{Title: "기억 저장 개선", Todo: []string{"손실 시험"}})
	card, err := ResumeText(cwd, "")
	if err != nil || !strings.Contains(card, "기억 저장 개선") {
		t.Fatalf("non-repository record missing: card=%s error=%v", card, err)
	}
	card, err = ResumeText(t.TempDir(), resumeTranscript(t, cwd, resumeHuman("human", "이어가자")))
	if err != nil || card != "" {
		t.Fatalf("unrelated folder received another task's memory: card=%s error=%v", card, err)
	}
}

func TestResumeMatchesRepositorySubfolders(t *testing.T) {
	fixture(t)
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init fixture: %v %s", err, out)
	}
	first, second := filepath.Join(repo, "first"), filepath.Join(repo, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	resumeFixture(t, first, "44444444-0000-0000-0000-000000000000", "ready", summary{Title: "공통 저장 작업", Todo: []string{"검사"}})
	card, err := ResumeText(second, "")
	if err != nil || !strings.Contains(card, "공통 저장 작업") {
		t.Fatalf("same repository's subfolder did not match: card=%s error=%v", card, err)
	}
}

func TestResumeDoesNotReopenExplicitlyClosedTodo(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	m := summary{Title: "설정 정리", Todo: []string{"PostToolBatch 등록", "실제 이어가기 검증"}, Resolutions: []resolution{{What: "PostToolBatch 등록", Kind: "todo", State: "취소", Source: "final", Evidence: "등록하지 않음"}}}
	resumeFixture(t, cwd, "55555555-0000-0000-0000-000000000000", "ready", m)
	card, err := ResumeText(cwd, "")
	if err != nil || strings.Contains(card, "남은 일: PostToolBatch 등록") || !strings.Contains(card, "명시된 닫힘: PostToolBatch 등록 → 취소") || !strings.Contains(card, "남은 일: 실제 이어가기 검증") {
		t.Fatalf("explicit cancellation was reopened: card=%s error=%v", card, err)
	}
	allClosed := summary{Title: "완료된 별개 작업", Todo: []string{"검사"}, Resolutions: []resolution{{What: "검사", Kind: "todo", State: "완료", Source: "actual", Evidence: "검사를 마쳤다"}}}
	resumeFixture(t, cwd, "66666666-0000-0000-0000-000000000000", "ready", allClosed)
	card, err = ResumeText(cwd, "")
	if err != nil || strings.Contains(card, "완료된 별개 작업") {
		t.Fatalf("closed work selected as unfinished: card=%s error=%v", card, err)
	}
}

func TestResumeDoesNotTreatEmptyTodoAsVerifiedCompletion(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "77777777-0000-0000-0000-000000000000", "ready", summary{Title: "검증 여부가 없는 작업"})
	card, err := ResumeText(cwd, "")
	if err != nil || !strings.Contains(card, "현황 미확정") {
		t.Fatalf("empty todo was interpreted as full completion: card=%s error=%v", card, err)
	}
}

func TestResumeBoundsCardAndPreservesDetailedSources(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	m := summary{Title: "많은 결정"}
	for i := 0; i < 30; i++ {
		m.Decisions = append(m.Decisions, decision{What: strings.Repeat("결정 ", 100), Why: fmt.Sprintf("%d 번째", i)})
		m.Todo = append(m.Todo, strings.Repeat("해야 할 일 ", 100))
	}
	path := resumeFixture(t, cwd, "88888888-0000-0000-0000-000000000000", "pending", m)
	card, err := ResumeText(cwd, "")
	if err != nil || len([]rune(card)) > 2000 || !strings.Contains(card, path) {
		t.Fatalf("card grew without a bound or lost source: chars=%d card=%s error=%v", len([]rune(card)), card, err)
	}
}

func TestResumeReportsUnreadableCurrentTranscriptInsteadOfInventingState(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "99999999-0000-0000-0000-000000000000", "pending", summary{Title: "진행 중", Todo: []string{"확인"}})
	path := resumeTranscript(t, cwd, resumeHuman("real", "응"))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{bad json\n")
	f.Close()
	card, err := ResumeText(cwd, path)
	if err == nil || !strings.Contains(card, "real 줄2") {
		t.Fatalf("read error was hidden: card=%s error=%v", card, err)
	}
}

func TestNewSessionRestoresPriorFinalChoiceBeforeConflictingSummary(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	id := "abcdefab-0000-0000-0000-000000000000"
	path := resumeFixture(t, cwd, id, "pending", summary{Title: "언어 설정 개선", Decisions: []decision{{What: "PostToolBatch를 등록한다", Why: "예비 계획", Status: "추정"}}, Todo: []string{"실제 이어가기 검증"}})
	record, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	question := "PostToolBatch를 어떻게 할까요?"
	first := resumeHuman("before", "언어 설정 고쳐줘")
	first["cwd"] = cwd
	write(t, header(record, "raw"), jsonLines(t, first,
		map[string]any{"type": "assistant", "uuid": "question", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "q", "name": "AskUserQuestion", "input": map[string]any{"questions": []any{map[string]any{"question": question}}}}}}},
		map[string]any{"type": "user", "uuid": "prior-final", "version": "2.1.292", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "q"}}}, "toolUseResult": map[string]any{"answers": map[string]any{question: "등록하지 않음"}}},
		map[string]any{"type": "assistant", "message": map[string]any{"content": "기억 개선을 모두 마쳤습니다."}},
		map[string]any{"type": "user", "promptSource": "sdk", "message": map[string]any{"content": "시험에서는 PostToolBatch를 등록한다"}},
	))
	current := resumeTranscript(t, cwd, resumeHuman("new", "응"))
	card, err := ResumeText(cwd, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"등록하지 않음", "prior-final:q:0 줄3", "실제 이어가기 검증", "harness recall source " + id} {
		if !strings.Contains(card, want) {
			t.Fatalf("prior final decision or source lost: missing=%q card=%s", want, card)
		}
	}
	if strings.Index(card, "등록하지 않음") > strings.Index(card, "PostToolBatch를 등록한다") || strings.Contains(card, "시험에서는") {
		t.Fatalf("new session preferred inferred or scripted state: %s", card)
	}
	// The agent's last answer shows where the work stopped, but only as a labelled claim after the person's choice.
	if ending := strings.Index(card, "끝난 지점(AI 주장): 기억 개선을 모두 마쳤습니다."); ending < 0 || ending < strings.Index(card, "등록하지 않음") || strings.Count(card, "모두 마쳤습니다") != 1 {
		t.Fatalf("agent claim missing, unlabelled, or placed before the person's choice: %s", card)
	}
}

func TestResumeDoesNotSelectTaskFromOneGenericTitleWord(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "aaaaaaaa-0000-0000-0000-000000000000", "ready", summary{Title: "언어 설정 개선", Todo: []string{"시험"}})
	resumeFixture(t, cwd, "bbbbbbbb-0000-0000-0000-000000000000", "ready", summary{Title: "원본 저장 개선", Todo: []string{"시험"}})
	card, err := ResumeText(cwd, resumeTranscript(t, cwd, resumeHuman("new", "설정을 이어가줘")))
	if err != nil || !strings.Contains(card, "이 레포 대화 2개") || strings.Contains(card, "사용자 요청의 제목과 일치") {
		t.Fatalf("one generic word selected a task: card=%s error=%v", card, err)
	}
}

func TestResumeWarnsWhenPriorRawIsMissing(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	path := resumeFixture(t, cwd, "cccccccc-0000-0000-0000-000000000000", "pending", summary{Title: "자료 읽기", Todo: []string{"확인"}})
	raw, _ := os.ReadFile(path)
	if err := os.Remove(header(raw, "raw")); err != nil {
		t.Fatal(err)
	}
	card, err := ResumeText(cwd, "")
	if err == nil || !strings.Contains(card, "원문 조회 미확정") || !strings.Contains(card, path) {
		t.Fatalf("missing raw was treated as verified continuation: card=%s error=%v", card, err)
	}
}

func TestResumeMasksSecretsInRawInputContextAndModelView(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "dddddddd-0000-0000-0000-000000000000", "ready", summary{Title: "기억 token=secret123456", Decisions: []decision{{What: "token=decision123456 보존", Why: "password=reason123456"}}, Todo: []string{"api_key=todo123456 확인"}})
	current := resumeTranscript(t, cwd, resumeHuman("person", "요청"), map[string]any{"type": "assistant", "message": map[string]any{"content": "token=context123456 값을 써도 될까요?"}}, resumeHuman("yes", "응"), resumeHuman("latest", "token=raw123456 저장"))
	card, err := ResumeText(cwd, current)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret123456", "decision123456", "reason123456", "todo123456", "context123456", "raw123456"} {
		if strings.Contains(card, secret) {
			t.Fatalf("secret leaked into card: %s", card)
		}
	}
	if !strings.Contains(card, "[가림]") {
		t.Fatalf("masked values not shown: %s", card)
	}
}

func TestResumeShowsNewestInputFirstAndLabelsUserLinkedInterpretation(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "eeeeeeee-0000-0000-0000-000000000000", "ready", summary{Title: "기억 개선", Decisions: []decision{{What: "기록 개선", Why: "누락 방지", Status: "사용자", Source: "old"}}, Todo: []string{"검사"}})
	current := resumeTranscript(t, cwd, resumeHuman("older", "옛 요청"), resumeHuman("latest", "최종 요청"))
	card, err := ResumeText(cwd, current)
	if err != nil || strings.Index(card, "최종 요청") > strings.Index(card, "옛 요청") || !strings.Contains(card, "사용자 원문 연결·요약 해석") {
		t.Fatalf("newest request or authority distinction missing: card=%s error=%v", card, err)
	}
}

func TestLastAnswerWaitsForTheReplyToTheLastRequest(t *testing.T) {
	asked := []worklog.Request{{Text: "첫 요청"}, {Text: "둘째 요청", Context: "첫 답"}}
	if got := lastAnswer(asked, []string{"첫 답"}); got != "" {
		t.Fatal("답 없는 마지막 요청에 이전 답을 끝난 지점으로 표시", got)
	}
	if got := lastAnswer(asked, []string{"첫 답", "둘째 답"}); got != "둘째 답" {
		t.Fatal(got)
	}
}

func TestResumeShowsWhereACandidateStoppedAndSkipsResentRequest(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resent := "보드 정보의 장소 칸을 개요와 같은 선에 맞춰줘"
	rows := []map[string]any{resumeHuman("first", resent), resumeHuman("again", resent), {"type": "assistant", "message": map[string]any{"content": "## 결론\n장소 칸을 개요 선에 맞췄습니다.\n남은 일은 화면 확인입니다."}}}
	for i, row := range rows {
		row["cwd"], row["sessionId"], row["timestamp"] = cwd, "stopped-session", time.Now().Add(time.Duration(i)*time.Second).UTC().Format(time.RFC3339Nano)
	}
	write(t, filepath.Join(RawDir(), "claude", "2026-10-07", "stopped-session.jsonl"), jsonLines(t, rows...))
	card, err := ResumeText(cwd, "")
	if err != nil || !strings.Contains(card, "끝난 지점(AI 주장): 장소 칸을 개요 선에 맞췄습니다. 남은 일은 화면 확인입니다.") || strings.Count(card, "] "+resent) != 1 || !strings.Contains(card, "- 목표/제목: "+resent) {
		t.Fatalf("candidate ending missing or resent request repeated: %s error=%v", card, err)
	}
}

// endAt rewrites when a stored record says its conversation ended.
func endAt(t *testing.T, path, to, end string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, to, regexp.MustCompile(`(?m)^end: .*$`).ReplaceAllString(string(data), "end: "+end))
}

func TestResumeLeadsWithTheOpenConversationThenNamesTheOthers(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "current-session", "pending", summary{Title: "지금 작업", Todo: []string{"지금 할 일"}})
	other := resumeFixture(t, cwd, "other-session", "pending", summary{Title: "다른 작업", Todo: []string{"다른 할 일"}})
	endAt(t, other, other, "2026-10-07 12:00") // the other conversation moved more recently
	row := resumeHuman("now", "지금 대화의 입력")
	row["cwd"], row["sessionId"] = cwd, "current-session"
	current := filepath.Join(t.TempDir(), "current-session.jsonl")
	write(t, current, jsonLines(t, row))
	card, err := ResumeText(cwd, current)
	now, label, another := strings.Index(card, "지금 작업"), strings.Index(card, "같은 폴더의 다른 대화"), strings.Index(card, "다른 작업")
	if err != nil || now < 0 || label < now || another < label {
		t.Fatalf("다른 대화가 지금 대화 입력과 제목 사이에 끼었다: %s error=%v", card, err)
	}
}

func TestResumeShowsAConversationKeptInTwoFilesOnce(t *testing.T) {
	fixture(t)
	cwd := t.TempDir()
	resumeFixture(t, cwd, "third-session", "pending", summary{Title: "셋째 작업", Todo: []string{"셋째 할 일"}})
	split := resumeFixture(t, cwd, "split-session", "pending", summary{Title: "나뉜 작업", Todo: []string{"나뉜 할 일"}})
	endAt(t, split, split, "2026-10-07 12:00")
	endAt(t, split, filepath.Join(filepath.Dir(split), "1200-repo-split-2.md"), "2026-10-07 12:30")
	card, err := ResumeText(cwd, "")
	if err != nil || strings.Count(card, "나뉜 작업") != 1 || !strings.Contains(card, "셋째 작업") {
		t.Fatalf("같은 대화가 카드 두 자리를 차지했다: %s error=%v", card, err)
	}
}
