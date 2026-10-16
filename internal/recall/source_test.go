package recall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourcePagesFullRequestAndRejectsAmbiguousSession(t *testing.T) {
	_, raw := fixture(t)
	path := filepath.Join(raw, "claude", "2026-10-07", "abcdef12-0000.jsonl")
	text := strings.Repeat("처음 ", 200) + "중간 중요한 결정" + strings.Repeat(" 끝 ", 200)
	write(t, path, jsonLines(t, map[string]any{"type": "user", "uuid": "u", "promptSource": "typed", "message": map[string]any{"role": "user", "content": text}}))
	var joined string
	for offset := 0; offset < 4; offset++ {
		part, err := SourcePage("abcdef12-0000", "", "u", offset, 1)
		if err != nil {
			t.Fatal(err)
		}
		joined += part
	}
	if !strings.Contains(joined, "중간 중요한 결정") || !strings.Contains(joined, "[줄1 u 문자400]") || !strings.Contains(joined, "이어서:") {
		t.Fatal(joined)
	}
	write(t, filepath.Join(filepath.Dir(path), "abcdef12-1111.jsonl"), "{}\n")
	if _, err := Source("abcdef12", ""); err == nil {
		t.Fatal("동명이원본의첫파일을임의선택")
	}
	if _, err := SourcePage("abcdef12-0000", "", "u", 99, 1); err == nil {
		t.Fatal("잘못된페이지를빈성공으로반환")
	}
}

func TestSyncStatusReportsCollectionFailureWithoutConversation(t *testing.T) {
	fixture(t)
	if err := SaveSyncStatus(map[string]any{"failed": []string{"private raw path"}}, map[string]any{"pending": 2}, nil); err != nil {
		t.Fatal(err)
	}
	if warning := SyncWarning(); !strings.Contains(warning, "이번 수집 실패 1건") || strings.Contains(warning, "요약") || strings.Contains(warning, "private raw path") {
		t.Fatal(warning)
	}
	if err := SaveSyncStatus(nil, map[string]any{"storage_uncertain": true}, nil); err != nil {
		t.Fatal(err)
	}
	if warning := SyncWarning(); !strings.Contains(warning, "수집 또는 저장 결과 미확인") {
		t.Fatal("저장 결과 미확인이 성공으로 표시됨", warning)
	}
	if err := SaveSyncStatus(nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if warning := SyncWarning(); warning != "" {
		t.Fatal("실패 없는 수집에 경고", warning)
	}
}

func TestStoredSummaryFailureNoLongerWarnsAtStart(t *testing.T) {
	_, _, path := recordFixture(t)
	s, _ := readSession("claude", path)
	s.summaryState, s.summaryError = "failed", "요약 호출 실패; 기존 기억 유지"
	write(t, filepath.Join(RecordDir(), "2026-10-07", "failed.md"), render(s, summary{}, false))
	if err := SaveSyncStatus(nil, map[string]any{"pending": 1, "summary_attempts": 0}, nil); err != nil {
		t.Fatal(err)
	}
	if warning := SyncWarning(); warning != "" {
		t.Fatal("모델을 부르지 않는 수집에 옛 요약 실패를 경고", warning)
	}
	if pending, unresolved := storedSummaryCounts(); pending == nil || unresolved == nil || *pending != 1 || *unresolved != 1 {
		t.Fatal("수동 요약 집계가 옛 실패를 잃음", pending, unresolved)
	}
}

func TestOldAndMissingSyncStatusDoNotClaimVerifiedSuccess(t *testing.T) {
	fixture(t)
	if warning := SyncWarning(); warning != "" {
		t.Fatal("아직 수집하지 않은 환경에 경고", warning)
	}
	path := filepath.Join(Dir(), "sync-status.json")
	write(t, path, `{"at":"old","failures":0,"pending":3,"unresolved":2}`)
	if warning := SyncWarning(); warning != "" {
		t.Fatal("구형 상태의 요약 집계를 수집 실패로 표시", warning)
	}
	write(t, path, `{}`)
	if warning := SyncWarning(); !strings.Contains(warning, "이번 수집 실패 미확인") {
		t.Fatal("실패 집계 없는 상태를 성공으로 해석", warning)
	}
	write(t, path, `{"at":`)
	if warning := SyncWarning(); !strings.Contains(warning, "상태 확인 실패") {
		t.Fatal("깨진 상태 파일을 성공으로 해석", warning)
	}
}

func TestStoredSummaryCountsDistinguishesMissingAndUnreadableDirectories(t *testing.T) {
	for _, at := range []string{"root", "date", "broken-link"} {
		t.Run(at, func(t *testing.T) {
			fixture(t)
			pending, unresolved := storedSummaryCounts()
			if pending == nil || unresolved == nil || *pending != 0 || *unresolved != 0 {
				t.Fatal("아직 없는 기록 폴더를 읽기 실패로 처리", pending, unresolved)
			}
			path := RecordDir()
			if at == "date" {
				path = filepath.Join(path, "2026-10-07")
			}
			if at == "broken-link" {
				if err := os.MkdirAll(Dir(), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(Dir(), "missing-target"), path); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, path, "a file where a directory belongs")
			}
			pending, unresolved = storedSummaryCounts()
			if pending != nil || unresolved != nil {
				t.Fatal("읽기 실패를 빈 성공으로 집계", pending, unresolved)
			}
		})
	}
}
