package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAutomaticReplayHasSmallBatchAndKeepsInvalidBytes(t *testing.T) {
	s := fixture(t)
	for i := 0; i < 20; i++ {
		if e := PrivateWrite(filepath.Join(s.Root, "pending", fmt.Sprintf("%020d-old.json", i)), Packed(rawEvent(fmt.Sprintf("old-%d", i), "task"))); e != nil {
			t.Fatal(e)
		}
	}
	badPath := filepath.Join(s.Root, "pending", "00000000000000000000-bad.json")
	badBytes := []byte("{original invalid bytes")
	if e := PrivateWrite(badPath, badBytes); e != nil {
		t.Fatal(e)
	}
	mustRecord(t, s, rawEvent("new-0", "task"))
	h, e := s.Health()
	if e != nil || Number(h["events"]) < 1 || Number(h["events"]) > 8 || Number(h["pending"]) < 13 {
		t.Fatal("automatic replay ignored its 8-file batch limit", h, e)
	}
	for i := 1; i <= 20 && s.queueCount("json") > 0; i++ {
		mustRecord(t, s, rawEvent(fmt.Sprintf("new-%d", i), "task"))
	}
	if s.queueCount("json") != 0 {
		t.Fatal("repeated ingest did not recover backlog")
	}
	got, e := os.ReadFile(filepath.Join(s.Root, "pending", "00000000000000000000-bad.invalid"))
	if e != nil || string(got) != string(badBytes) {
		t.Fatal("invalid bytes not preserved", string(got), e)
	}
	old, e := s.Records("task", "old-", 100, 0)
	if e != nil || len(old) != 20 {
		t.Fatal("backlog lost records", len(old), e)
	}
	h, e = s.Health()
	if e != nil || h["invalid"] != 1 || h["integrity"] != "ok" {
		t.Fatal(h, e)
	}
}

func TestReplayTimeBudgetStopsBeforeApplyingNextFile(t *testing.T) {
	s := fixture(t)
	if e := PrivateWrite(filepath.Join(s.Root, "pending", "event.json"), Packed(rawEvent("old", "task"))); e != nil {
		t.Fatal(e)
	}
	out, e := s.replayLocked(8, time.Nanosecond, "")
	if e != nil || len(out) != 0 || s.queueCount("json") != 1 {
		t.Fatal("exhausted budget still applied events", out, e)
	}
}

func TestPendingDirectoryAndSymlinkDoNotBlockCurrentIngest(t *testing.T) {
	s := fixture(t)
	entry := filepath.Join(s.Root, "pending", "000-directory.json")
	if e := os.Mkdir(entry, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(entry, "original"), []byte("directory original"), 0600); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(t.TempDir(), "original")
	if e := os.WriteFile(outside, []byte("outside original"), 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(s.Root, "pending", "001-link.json")
	if e := os.Symlink(outside, link); e != nil {
		t.Fatal(e)
	}
	r := mustRecord(t, s, rawEvent("current", "task"))
	if r["result"] != "recorded" || len(r["recovery_issues"].([]Object)) != 2 {
		t.Fatal("old nonregular queue prevented current event", r)
	}
	got, e := os.ReadFile(filepath.Join(s.Root, "pending", "000-directory.invalid", "original"))
	if e != nil || string(got) != "directory original" {
		t.Fatal("directory original changed", string(got), e)
	}
	if target, e := os.Readlink(filepath.Join(s.Root, "pending", "001-link.invalid")); e != nil || target != outside {
		t.Fatal("symlink was followed or lost", target, e)
	}
	got, e = os.ReadFile(outside)
	if e != nil || string(got) != "outside original" {
		t.Fatal("symlink target changed", string(got), e)
	}
	h, e := s.Health()
	if e != nil || h["events"] != 1 || h["pending"] != 0 || h["invalid"] != 2 || h["integrity"] != "ok" {
		t.Fatal(h, e)
	}
}

func TestUnreadablePendingPreservedWhileCurrentIngests(t *testing.T) {
	s := fixture(t)
	path := filepath.Join(s.Root, "pending", "000-unreadable.json")
	bytes := Packed(rawEvent("unreadable", "task"))
	if e := os.WriteFile(path, bytes, 0000); e != nil {
		t.Fatal(e)
	}
	if _, e := os.ReadFile(path); e == nil {
		t.Skip("this user can read mode-000 files; permission-error case unavailable")
	}
	r := mustRecord(t, s, rawEvent("current", "task"))
	if r["result"] != "recorded" || r["recovery_issues"] == nil {
		t.Fatal("old read error prevented current event", r)
	}
	target := filepath.Join(s.Root, "pending", "000-unreadable.invalid")
	if e := os.Chmod(target, 0600); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(target)
	if e != nil || string(got) != string(bytes) {
		t.Fatal("unreadable bytes changed", string(got), e)
	}
	h, e := s.Health()
	if e != nil || h["events"] != 1 || h["pending"] != 0 || h["invalid"] != 1 {
		t.Fatal(h, e)
	}
}

func TestQuarantineCollisionPreservesBothOriginals(t *testing.T) {
	s := fixture(t)
	path := filepath.Join(s.Root, "pending", "000-bad.json")
	old := filepath.Join(s.Root, "pending", "000-bad.invalid")
	if e := os.WriteFile(path, []byte("{new invalid original"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(old, []byte("old invalid original"), 0600); e != nil {
		t.Fatal(e)
	}
	mustRecord(t, s, rawEvent("current", "task"))
	got, e := os.ReadFile(old)
	if e != nil || string(got) != "old invalid original" {
		t.Fatal("older quarantine overwritten", string(got), e)
	}
	paths, e := filepath.Glob(filepath.Join(s.Root, "pending", "000-bad-*.invalid"))
	if e != nil || len(paths) != 1 {
		t.Fatal("collision did not create a separate quarantine", paths, e)
	}
	got, e = os.ReadFile(paths[0])
	if e != nil || string(got) != "{new invalid original" {
		t.Fatal("new invalid original lost", string(got), e)
	}
}

func TestNonregularQueueFloodRemainsBounded(t *testing.T) {
	s := fixture(t)
	for i := 0; i < 30; i++ {
		if e := os.Mkdir(filepath.Join(s.Root, "pending", fmt.Sprintf("%020d-dir.json", i)), 0700); e != nil {
			t.Fatal(e)
		}
	}
	r := mustRecord(t, s, rawEvent("current", "task"))
	issues := r["recovery_issues"].([]Object)
	if r["result"] != "queued" || len(issues) < 1 || len(issues) > 8 || s.queueCount("json") < 23 {
		t.Fatal("nonregular entries bypassed automatic batch limit", r, s.queueCount("json"))
	}
}

func TestReplayInfrastructureFailureKeepsValidPendingAndQueuesCaller(t *testing.T) {
	s := fixture(t)
	old := rawEvent("old", "task")
	old["source_keys"] = []any{"source-key"}
	path := filepath.Join(s.Root, "pending", "000-old.json")
	bytes := Packed(old)
	if e := PrivateWrite(path, bytes); e != nil {
		t.Fatal(e)
	}
	// Only the old event needs the temporarily unavailable source table.
	db, e := s.Connect(false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("DROP TABLE artifacts")
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	r := mustRecord(t, s, rawEvent("current", "task"))
	if r["result"] != "queued" || r["recovery_issues"] == nil {
		t.Fatal("caller overtook a pending predecessor", r)
	}
	got, e := os.ReadFile(path)
	if e != nil || string(got) != string(bytes) || s.queueCount("invalid") != 0 {
		t.Fatal("valid old event was quarantined or altered", string(got), e)
	}
}

func TestCurrentInfrastructureFailureKeepsPendingForRetry(t *testing.T) {
	s := fixture(t)
	db, e := s.Connect(false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("DROP TABLE events")
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	v := rawEvent("current", "task")
	if _, e = s.Ingest(v); e == nil {
		t.Fatal("missing table was reported successful")
	}
	if s.queueCount("json") != 1 || s.queueCount("invalid") != 0 {
		t.Fatal("current valid event was not retained for retry")
	}
	db, e = s.Connect(false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(schema)
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	r := mustRecord(t, s, v)
	h, e := s.Health()
	if e != nil || r["result"] != "duplicate" || h["events"] != 1 || h["pending"] != 0 || h["invalid"] != 0 {
		t.Fatal("retained current event did not recover exactly once", r, h, e)
	}
}

func TestOptOutMarkerIsScopedAndNotRestored(t *testing.T) {
	s := fixture(t)
	if e := s.Unbind("codex", "session"); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		tool, session string
		want          bool
	}{{"codex", "session", true}, {"claude", "session", false}, {"codex", "another", false}} {
		got, e := s.OptedOut(tc.tool, tc.session)
		if e != nil || got != tc.want {
			t.Fatal(tc, got, e)
		}
	}
	// A previous bind attempt's inactive marker must still ask for a choice.
	if e := PrivateWrite(s.bindingPath("codex", "unfinished"), Packed(Object{"active": false})); e != nil {
		t.Fatal(e)
	}
	if optedOut, e := s.OptedOut("codex", "unfinished"); e != nil || optedOut {
		t.Fatal("unfinished binding became an opt-out", optedOut, e)
	}
	mustRecord(t, s, rawEvent("raw", "task"))
	config := Object{"schema": 1, "category": "personal", "project": "project", "data_root": filepath.Dir(s.Root), "project_root": s.Root, "tasks": []any{"task"}, "source_roots": []any{s.Root}}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, e := s.Backup(backup, config); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	if _, e := Restore(backup, root); e != nil {
		t.Fatal(e)
	}
	restored, e := Open(root, "project", false)
	if e != nil {
		t.Fatal(e)
	}
	if optedOut, e := restored.OptedOut("codex", "session"); e != nil || optedOut {
		t.Fatal("restore reused old choice", optedOut, e)
	}
	if b, e := restored.Binding("codex", "session"); e != nil || b != nil {
		t.Fatal("restore silently bound old task", b, e)
	}
	if e := s.Bind("codex", "session", "task", s.Root); e != nil {
		t.Fatal(e)
	}
	if optedOut, e := s.OptedOut("codex", "session"); e != nil || optedOut {
		t.Fatal("new explicit binding retained none marker", optedOut, e)
	}
}
