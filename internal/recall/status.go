package recall

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"time"
)

// syncStatus is what the next card says about the last collection. Sync no longer calls a
// model, so summary counts belong to a manual build's own output, not to this line.
type syncStatus struct {
	At       string `json:"at"`
	Failures *int   `json:"failures,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Counts come from the stored records, rather than the failures of this one run. "off" records
// wait on nothing. Missing old metadata or unreadable records leave the total unknown.
func storedSummaryCounts() (*int, *int) {
	records, errs := loadRecords()
	if len(errs) > 0 {
		return nil, nil
	}
	pending, unresolved := 0, 0
	for _, rec := range records {
		if rec.field("raw") == "" {
			continue
		}
		state := rec.field("summary_state")
		switch state {
		case "pending", "ready", "failed", "off":
		default:
			return nil, nil
		}
		if state == "pending" || state == "failed" || rec.field("lessons_pending") == "true" {
			pending++
		}
		if message, _ := summaryDiagnostics(rec.raw); message != "" {
			unresolved++
		}
	}
	return &pending, &unresolved
}

// SaveSyncStatus keeps a small outcome for the next card; it does not contain conversation text.
func SaveSyncStatus(archive, records map[string]any, cause error) error {
	s := syncStatus{At: time.Now().Format(time.RFC3339)}
	failures := 0
	for _, result := range []map[string]any{archive, records} {
		if failed, ok := result["failed"].([]string); ok {
			failures += len(failed)
		}
	}
	s.Failures = &failures
	if uncertain, _ := records["storage_uncertain"].(bool); uncertain {
		s.Error = "기억 저장 결과 미확인"
	}
	if cause != nil {
		s.Error = "수집 중 오류 발생"
		failures++
	}
	return errors.Join(cause, memory.PrivateWrite(filepath.Join(Dir(), "sync-status.json"), memory.Packed(s)))
}

func SyncWarning() string {
	raw, err := os.ReadFile(filepath.Join(Dir(), "sync-status.json"))
	if os.IsNotExist(err) {
		return ""
	}
	var s syncStatus
	if err != nil || json.Unmarshal(raw, &s) != nil {
		return "=== 기억 수집 상태 확인 실패 — recall sync 결과를 확인한다 ==="
	}
	if s.At != "" && s.Failures != nil && *s.Failures == 0 && s.Error == "" {
		return ""
	}
	failures := "미확인"
	if s.Failures != nil && *s.Failures >= 0 {
		failures = fmt.Sprintf("%d건", *s.Failures)
	}
	extra := ""
	if s.Error != "" {
		extra = " · 수집 또는 저장 결과 미확인"
	}
	return fmt.Sprintf("=== 기억 수집 %s — 이번 수집 실패 %s%s. recall sync로 확인한다 ===", s.At, failures, extra)
}
