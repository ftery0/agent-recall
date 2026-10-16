package main

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/recall"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRelearnRejectsAmbiguousOptionsWithoutWriting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HARNESS_HOME", root)
	for _, args := range [][]string{
		{"recall", "relearn", "--dry-run", "--expect-token", "token"},
		{"recall", "relearn", "--expect-token", ""},
		{"recall", "relearn", "unexpected"},
		{"recall", "relearn", "--unknown"},
	} {
		if _, err := run(args); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid options changed the storage", entries, err)
	}
}

func TestNativeStopKeepsEvidenceQuietAndStructureVisible(t *testing.T) {
	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			root, err := memory.Resolve(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if raw, err := exec.Command("git", "init", "--quiet", root).CombinedOutput(); err != nil {
				t.Fatal(string(raw), err)
			}
			path := filepath.Join(root, "one.go")
			p := memory.Object{"session_id": "native-notice", "cwd": root, "tool_name": "Write", "tool_use_id": "first", "tool_input": memory.Object{"file_path": path}}
			if tool == "codex" {
				p["tool_name"] = "apply_patch"
				p["tool_input"] = memory.Object{"patch": "*** Add File: one.go\n+x"}
			}
			edit := func(id, content string) {
				t.Helper()
				p["tool_use_id"], p["hook_event_name"] = id, "PreToolUse"
				if out := runWithInput(t, p, "edit-scope", tool); out != nil {
					t.Fatal("편집 전 안내", out)
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				p["hook_event_name"] = "PostToolUse"
				if out := runWithInput(t, p, "edit-scope", tool); out != nil {
					t.Fatal("편집 후 안내", out)
				}
			}
			stop := func() any { return runWithInput(t, p, "edit-scope", tool, "stop") }
			long := strings.Repeat("x\n", 410)
			edit("first", long)
			if out, ok := stop().(memory.Object); !ok || !strings.Contains(memory.Text(out["systemMessage"]), "410줄") || out["decision"] != nil {
				t.Fatal("구조 안내 누락 또는 차단", out)
			}
			if out := stop(); out != nil {
				t.Fatal("동일 구조 안내 반복", out)
			}
			if err := os.WriteFile(path, []byte(long+"format\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if out := stop(); out != nil {
				t.Fatal("수집되지 않은 포맷팅을 표시", out)
			}
			exported := runWithInput(t, p, "edit-scope", tool, "export").(memory.Object)
			if len(exported["unverified"].([]string)) != 1 || len(exported["files"].([]string)) != 0 {
				t.Fatal("포맷팅 근거를 누락 또는 소유로 판정", exported)
			}
			if err := os.WriteFile(path, []byte(long), 0600); err != nil {
				t.Fatal(err)
			}
			if out := stop(); out != nil {
				t.Fatal("검사 불가를 해소로 처리", out)
			}
			statePath := strings.TrimSuffix(memory.Text(exported["baseline"]), ".baseline.json") + ".json"
			raw, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			state, err := memory.Decode(raw)
			if err != nil {
				t.Fatal(err)
			}
			state["unverified"] = []string{"연속 편집 소유권 미확인: old.go"}
			raw = memory.Packed(state)
			if err := os.WriteFile(statePath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if out := stop(); out != nil {
				t.Fatal("과거 소유권 목록 또는 동일 구조 안내 반복", out)
			}
			exported = runWithInput(t, p, "edit-scope", tool, "export").(memory.Object)
			if len(exported["unverified"].([]string)) != 1 || !strings.Contains(exported["unverified"].([]string)[0], "old.go") {
				t.Fatal("과거 근거 삭제", exported)
			}
			if err := os.WriteFile(statePath, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			if out, ok := stop().(memory.Object); !ok || !strings.Contains(memory.Text(out["systemMessage"]), "근거") {
				t.Fatal("실제 상태 오류 숨김", out)
			}
			if err := os.WriteFile(statePath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if out := stop(); out != nil {
				t.Fatal("오류가 구조 중복 기준을 훼손", out)
			}
			edit("resolved", "package fixture\n")
			if out := stop(); out != nil {
				t.Fatal("정상 해소에 과거 근거 출력", out)
			}
			edit("recurred", long)
			if out, ok := stop().(memory.Object); !ok || !strings.Contains(memory.Text(out["systemMessage"]), "410줄") {
				t.Fatal("해소 뒤 재발 안내 누락", out)
			}
		})
	}
}

func TestStartCardWorksOutsideGitAndDoesNotInjectGlobalLessonsIntoCasualCompact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(root, "vault"))
	cwd := filepath.Join(root, "task")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(recall.RawDir(), "claude", "2026-10-07", "abcdef12.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	row := memory.Object{"type": "user", "uuid": "u", "cwd": cwd, "sessionId": "abcdef12", "promptSource": "typed", "timestamp": "2026-10-07T01:00:00Z", "message": memory.Object{"role": "user", "content": "기억 저장 흐름을 이어가자"}}
	if err := os.WriteFile(path, append(memory.Packed(row), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := recall.Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), false, time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	card := startCard("claude", memory.Object{"cwd": cwd, "source": "startup"})
	if !strings.Contains(card, "기억 저장 흐름") || strings.Contains(card, "요약 갱신") {
		t.Fatal("nongit 작업 기억 누락", card)
	}
	card = startCard("claude", memory.Object{"cwd": filepath.Join(root, "unrelated"), "source": "compact"})
	if !strings.HasPrefix(card, "=== 압축·이어 열기 뒤다.") || strings.Contains(card, "과거 작업은") || strings.Contains(card, "기억 저장 흐름") {
		t.Fatal("무관한 기억 주입", card)
	}
}

func TestNewConversationCardHasNoErrorBeforeItsTranscriptExists(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(root, "vault"))
	missing := filepath.Join(root, "not-written-yet.jsonl")
	for _, source := range []string{"startup", "clear"} {
		if card := startCard("claude", memory.Object{"cwd": root, "source": source, "transcript_path": missing}); strings.Contains(card, "기억 조회 미완료") {
			t.Fatal("아직 첫 줄이 없는 새 대화를 조회 실패로 보고", source, card)
		}
	}
	for _, source := range []string{"resume", "compact"} {
		if card := startCard("claude", memory.Object{"cwd": root, "source": source, "transcript_path": missing}); !strings.Contains(card, "기억 조회 미완료") {
			t.Fatal("이어 여는 대화의 사라진 원본을 숨김", source, card)
		}
	}
}

func TestSourceWordsMayComeBeforeOptions(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	path := filepath.Join(recall.RawDir(), "claude", "2026-10-07", "abcdef12-0000.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var rows []byte
	for i, text := range []string{"첫 요청", "중요한 결정 하나", "중요한 결정 둘"} {
		row := memory.Object{"type": "user", "uuid": "u" + string(rune('1'+i)), "promptSource": "typed", "message": memory.Object{"role": "user", "content": text}}
		rows = append(append(rows, memory.Packed(row)...), '\n')
	}
	if err := os.WriteFile(path, rows, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"결정", "--limit", "1"}, {"--limit", "1", "결정"}, {"--limit=1", "결정"}, {"결정", "-limit", "1"}} {
		out, err := run(append([]string{"recall", "source", "abcdef12-0000"}, args...))
		text, _ := out.(string)
		if err != nil || !strings.Contains(text, "중요한 결정 하나") || strings.Contains(text, "중요한 결정 둘") || strings.Contains(text, "첫 요청") {
			t.Fatal("찾을 말과 옵션 순서에 따라 결과가 달라짐", args, text, err)
		}
	}
	if got := flagsFirst([]string{"--", "--limit", "1"}, "limit"); strings.Join(got, " ") != "-- --limit 1" {
		t.Fatal("-- 뒤의 말을 옵션으로 읽음", got)
	}
}

func TestWorklogStopArchivesWithoutBlockingAndReportsFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	t.Setenv("HARNESS_RECALL", "")
	path := filepath.Join(root, "session.jsonl")
	if err := os.WriteFile(path, []byte("{\"timestamp\":\"2026-10-07T01:00:00Z\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out := runWithInput(t, memory.Object{"session_id": "s", "cwd": root, "transcript_path": path}, "worklog", "claude", "stop")
	if out != nil {
		t.Fatal("원본 보관이 노트 저장을 강요", out)
	}
	if _, err := os.Stat(recall.Dest(recall.RawDir(), "claude", path)); err != nil {
		t.Fatal(err)
	}
	out = runWithInput(t, memory.Object{"session_id": "s", "cwd": root, "transcript_path": path + "missing"}, "worklog", "claude", "stop")
	if text, ok := out.(string); !ok || !strings.Contains(text, "보관 미완료") {
		t.Fatal("보관 실패 숨김", out)
	}
}

func TestStopThenStartImmediatelyKeepsChoiceWithoutBuildingRecords(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(root, "vault"))
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	t.Setenv("HARNESS_RECALL", "")
	path := filepath.Join(root, "immediate-session.jsonl")
	row := memory.Object{"type": "user", "uuid": "final-human", "cwd": root, "sessionId": "immediate-session", "promptSource": "typed", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "message": memory.Object{"role": "user", "content": "PostToolBatch는 등록하지 않음"}}
	if err := os.WriteFile(path, append(memory.Packed(row), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if out := runWithInput(t, memory.Object{"cwd": root, "transcript_path": path}, "worklog", "claude", "stop"); out != nil {
		t.Fatal(out)
	}
	if records, _ := filepath.Glob(filepath.Join(recall.RecordDir(), "*", "*.md")); len(records) != 0 {
		t.Fatal("Stop unexpectedly built summaries", records)
	}
	card := startCard("claude", memory.Object{"cwd": root, "source": "resume"})
	if !strings.HasPrefix(card, "=== 압축·이어 열기 뒤다.") || !strings.Contains(card, "PostToolBatch는 등록하지 않음") || !strings.Contains(card, "final-human 줄1") || !strings.Contains(card, "보관 원본:") || strings.Contains(card, "요약 갱신") {
		t.Fatal("immediate next start missed archived choice", card)
	}
}

func TestCompleteStartCardBudgetReservesSourcesLatestChoiceAndFailures(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(root, "vault"))
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	path := filepath.Join(recall.RawDir(), "claude", "2026-10-07", "budget-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	for i := 0; i < 12; i++ {
		row := memory.Object{"type": "user", "uuid": "old", "cwd": root, "sessionId": "budget-session", "promptSource": "typed", "message": memory.Object{"content": strings.Repeat("오래된 계획 내용 ", 30)}}
		raw = append(raw, append(memory.Packed(row), '\n')...)
	}
	row := memory.Object{"type": "user", "uuid": "last-choice", "cwd": root, "sessionId": "budget-session", "promptSource": "typed", "message": memory.Object{"content": "PostToolBatch는 등록하지 않음"}}
	raw = append(raw, append(memory.Packed(row), '\n')...)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	lessons := []recall.Lesson{{Rule: strings.Repeat("매우 긴 확인된 교훈 ", 700), Scope: "전체", Status: "활성", Confirmed: true}}
	if err := os.MkdirAll(recall.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(recall.Dir(), "lessons.json"), memory.Packed(lessons), 0600); err != nil {
		t.Fatal(err)
	}
	status := memory.Object{"at": "2026-10-08T09:53:52+09:00", "failures": 1, "pending": 29, "unresolved": 29, "summary_attempts": 0}
	if err := os.WriteFile(filepath.Join(recall.Dir(), "sync-status.json"), memory.Packed(status), 0600); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(root, "vault", "personal", "10_프로젝트", "P", "작업 기록", "관련 과업.md")
	if err := os.MkdirAll(filepath.Dir(note), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(note, []byte("---\nstatus: 진행\nrepos: "+filepath.Base(root)+"\n---\n## 다음\n- 실제 확인\n"), 0600); err != nil {
		t.Fatal(err)
	}
	card := startCard("claude", memory.Object{"cwd": root, "source": "compact"})
	if len([]rune(card)) > 3000 || !strings.Contains(card, path) || !strings.Contains(card, "last-choice 줄13") || !strings.Contains(card, "PostToolBatch는 등록하지 않음") || !strings.Contains(card, "이번 수집 실패 1건") || strings.Contains(card, "미해결 요약 실패") || !strings.Contains(card, "생략된 내용") || !strings.Contains(card, "작업 구분: 개인") || !strings.Contains(card, "관련 과업") {
		t.Fatalf("combined budget lost priority content (%d): %s", len([]rune(card)), card)
	}
}

func TestLanguageSinceAcceptsInstallationTimestamp(t *testing.T) {
	for _, value := range []string{"2026-10-07", "2026-10-07T13:25:00+09:00"} {
		if at, err := parseSince(value); err != nil || at.IsZero() {
			t.Fatal(value, at, err)
		}
	}
	if _, err := parseSince("yesterday"); err == nil {
		t.Fatal("모호한시각허용")
	}
}

func runWithInput(t *testing.T, payload memory.Object, args ...string) any {
	t.Helper()
	input, e := os.CreateTemp(t.TempDir(), "stdin-")
	if e != nil {
		t.Fatal(e)
	}
	defer input.Close()
	input.Write(memory.Packed(payload))
	input.Seek(0, 0)
	before := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = before }()
	out, e := run(args)
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func TestLanguageHooks(t *testing.T) {
	t.Setenv("HARNESS_RECALL", "")
	if out, ok := runWithInput(t, memory.Object{"hook_event_name": "PostToolBatch", "session_id": "s"}, "worklog", "claude", "batch").(memory.Object); !ok || out["hookSpecificOutput"] == nil {
		t.Fatalf("LH1 메인 대화 묶음에는 안내: %v", out)
	}
	if out := runWithInput(t, memory.Object{"hook_event_name": "PostToolBatch", "session_id": "s", "agent_id": "a1"}, "worklog", "claude", "batch"); out != nil {
		t.Fatalf("LH2 하위 에이전트에는 없음: %v", out)
	}
	if out := runWithInput(t, memory.Object{"hook_event_name": "PreCompact", "trigger": "auto"}, "worklog", "claude", "precompact"); out != worklog.CompactInstruction {
		t.Fatalf("LH3 압축 지시 출력: %v", out)
	}
	t.Setenv("HARNESS_RECALL", "1")
	for _, command := range []string{"batch", "precompact"} {
		if out := runWithInput(t, memory.Object{"session_id": "s"}, "worklog", "claude", command); out != nil {
			t.Fatalf("LH4 하네스 자체 호출에는 없음(%s): %v", command, out)
		}
	}
}

func TestPromptHookAddsThePastARequestPointsToInJSONBothClientsRead(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("HARNESS_HOME", filepath.Join(root, "data"))
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(root, "vault"))
	t.Setenv("HARNESS_RECALL", "")
	for _, tool := range []string{"claude", "codex"} {
		if out := runWithInput(t, memory.Object{"hook_event_name": "UserPromptSubmit", "prompt": "이 파일 역할 알려줘", "cwd": root, "session_id": "s"}, "worklog", tool, "prompt"); out != nil {
			t.Fatal("과거를 말하지 않은 질문에 무언가 붙임", tool, out)
		}
		out, ok := runWithInput(t, memory.Object{"hook_event_name": "UserPromptSubmit", "prompt": "어제 한 거 보고 오늘 할 일", "cwd": root, "session_id": "s"}, "worklog", tool, "prompt").(memory.Object)
		specific, _ := out["hookSpecificOutput"].(memory.Object)
		yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
		if !ok || len(out) != 1 || specific["hookEventName"] != "UserPromptSubmit" || !strings.Contains(memory.Text(specific["additionalContext"]), "["+yesterday+"] 그날 기록된 요청이 없다") {
			t.Fatal("질문 훅 출력 모양이 다름", tool, out)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "data", "recall", "misses-checked")); err != nil {
		t.Fatal("주간 확인을 남기지 않음", err)
	}
}
