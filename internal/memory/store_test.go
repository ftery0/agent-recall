package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func fixture(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.TempDir(), "project", true)
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func rawEvent(key, task string) Object {
	return Object{"key": key, "task": task, "session": "session", "kind": "raw", "subject": "request", "body": "사용자 원문", "source_keys": []any{}}
}
func mustRecord(t *testing.T, s *Store, v Object) Object {
	t.Helper()
	r, e := s.Ingest(v)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func TestCanonical(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{"z":"한글<>&\u2028", "a":[1,1.0,0.00001,10000000.0]}`, "{\"a\": [1, 1.0, 1e-05, 10000000.0], \"z\": \"한글<>&\u2028\"}"},
		{`{"s":"\\u2028\n\t", "n":null}`, `{"n": null, "s": "\\u2028\n\t"}`},
	} {
		v, e := Decode([]byte(c.in))
		if e != nil {
			t.Fatal(e)
		}
		b, e := Canonical(v)
		if e != nil || string(b) != c.want {
			t.Fatalf("got %q want %q: %v", b, c.want, e)
		}
	}
}
func TestDuplicateAndConflict(t *testing.T) {
	s := fixture(t)
	v := rawEvent("one", "task")
	mustRecord(t, s, v)
	if r := mustRecord(t, s, v); r["result"] != "duplicate" {
		t.Fatal(r)
	}
	v["body"] = "different"
	if _, e := s.Ingest(v); e == nil {
		t.Fatal("collision accepted")
	}
	h, _ := s.Health()
	if h["invalid"] != 1 || h["events"] != 1 {
		t.Fatal(h)
	}
}
func TestRevisionsAndTaskIsolation(t *testing.T) {
	s := fixture(t)
	for _, task := range []string{"a", "b"} {
		v := rawEvent("raw-"+task, task)
		mustRecord(t, s, v)
		d := Object{"key": "decision-" + task, "task": task, "session": "s", "kind": "decision", "subject": "database", "body": "SQLite-" + task, "source_keys": []any{"event:raw-" + task}, "status": "adopted", "scope": task, "user_confirmed": true, "expected_revision": 0}
		mustRecord(t, s, d)
		d["key"] = "conflict-" + task
		d["body"] = "bad"
		if r := mustRecord(t, s, d); r["result"] != "conflict" {
			t.Fatal(r)
		}
	}
	a, e := s.Recall("a")
	if e != nil {
		t.Fatal(e)
	}
	if a["current"].([]Object)[0]["body"] != "SQLite-a" {
		t.Fatal(a)
	}
	v := Object{"key": "cross", "task": "b", "session": "s", "kind": "decision", "subject": "other", "body": "cross", "source_keys": []any{"event:raw-a"}, "status": "adopted", "scope": "b", "user_confirmed": true, "expected_revision": 0}
	if _, e = s.Ingest(v); e == nil {
		t.Fatal("cross-task source")
	}
}
func TestSourcesAndCorruption(t *testing.T) {
	s := fixture(t)
	a, e := s.Artifact([]byte("same"), "origin-a")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.Artifact([]byte("same"), "origin-b")
	if e != nil || a == b {
		t.Fatal(a, b, e)
	}
	v, data, e := s.Source(b)
	if e != nil || v["origin"] != "origin-b" || string(data) != "same" {
		t.Fatal(v, e)
	}
	db, _ := s.Connect(false)
	_, e = db.Exec("UPDATE artifacts SET bytes=?", []byte("corrupt"))
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = s.Source(a); e == nil {
		t.Fatal("checksum ignored")
	}
}
func TestQueueAndReplay(t *testing.T) {
	s := fixture(t)
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := s.Ingest(rawEvent("queued", "task"))
	unlock()
	if e != nil || r["result"] != "queued" {
		t.Fatal(r, e)
	}
	out, e := s.Replay()
	if e != nil || len(out) != 1 || out[0]["result"] != "recorded" {
		t.Fatal(out, e)
	}
	if s.queueCount("json") != 0 {
		t.Fatal("queue not drained")
	}
}
func TestBackupRestorePreservesClearsBindings(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("raw", "task"))
	if e := s.Bind("codex", "session", "task", s.Root); e != nil {
		t.Fatal(e)
	}
	config := Object{"schema": 1, "category": "personal", "project": "project", "data_root": filepath.Dir(s.Root), "project_root": s.Root, "tasks": []any{"task"}, "source_roots": []any{s.Root}}
	config["project_aliases"] = []any{t.TempDir()}
	config["exact_cwds"] = []any{t.TempDir()}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, e := s.Backup(backup, config); e != nil {
		t.Fatal(e)
	}
	restored := t.TempDir()
	result, e := Restore(backup, restored)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile(Text(result["config"]))
	if e != nil {
		t.Fatal(e)
	}
	restoredConfig, e := Decode(raw)
	if e != nil {
		t.Fatal(e)
	}
	for _, scope := range []string{"project_aliases", "exact_cwds"} {
		if _, present := restoredConfig[scope]; present {
			t.Fatalf("restore retained old scope %s: %v", scope, restoredConfig[scope])
		}
	}
	if restoredConfig["project_root"] != result["restored"] || Text(restoredConfig["data_root"]) != filepath.Dir(Text(result["restored"])) || string(Packed(restoredConfig["source_roots"])) != string(Packed([]any{result["restored"]})) {
		t.Fatal("restore did not isolate new paths", restoredConfig)
	}
	r, e := Open(restored, "project", false)
	if e != nil {
		t.Fatal(e)
	}
	h, e := r.Health()
	if e != nil || h["events"] != 1 || h["bindings"] != 0 {
		t.Fatal(h, e)
	}
	if _, e = Restore(backup, restored); e == nil {
		t.Fatal("restore overwrote existing")
	}
}
func TestExportCannotOverwrite(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("one", "task"))
	if _, e := s.Export("task", s.Path); e == nil {
		t.Fatal("DB overwritten")
	}
	p := filepath.Join(t.TempDir(), "export.md")
	if _, e := s.Export("task", p); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Export("task", p); e == nil {
		t.Fatal("file overwritten")
	}
}
func TestVerificationStaleness(t *testing.T) {
	s := fixture(t)
	source, e := s.Artifact([]byte("log"), "test")
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(t.TempDir(), "code.go")
	os.WriteFile(file, []byte("initial"), 0600)
	v := Object{"key": "verification", "task": "task", "session": "session", "kind": "verification", "subject": "test", "body": "local", "source_keys": []any{source}, "authority": "local-command", "command": []any{"go", "test"}, "exit_code": 0, "checked_at": "2026-10-04", "snapshot": Object{file: Hash([]byte("initial"))}}
	mustRecord(t, s, v)
	r, e := s.Recall("task")
	if e != nil {
		t.Fatal(e)
	}
	if r["checks"].([]Object)[0]["stale"] != false {
		t.Fatal(r)
	}
	os.WriteFile(file, []byte("changed"), 0600)
	r, _ = s.Recall("task")
	if r["checks"].([]Object)[0]["stale"] != true {
		t.Fatal(r)
	}
	v["key"] = "user-check"
	v["subject"] = "manual"
	v["authority"] = "user-confirmed"
	v["passed"] = true
	mustRecord(t, s, v)
	r, _ = s.Recall("task")
	if r["checks"].([]Object)[1]["passed"] != true {
		t.Fatal(r)
	}
}
func TestConcurrentWriters(t *testing.T) {
	s := fixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, e := s.Ingest(rawEvent(ID(), "task")); errs <- e }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Replay(); e != nil {
		t.Fatal(e)
	}
	h, _ := s.Health()
	if h["events"] != 20 || h["pending"] != 0 {
		t.Fatal(h)
	}
}
func TestValidationAndPaging(t *testing.T) {
	s := fixture(t)
	for _, v := range []Object{{}, rawEvent("../escape", "task"), {"key": "x", "task": "task", "session": "s", "subject": "sub", "kind": "state", "body": "bad", "status": "active", "expected_revision": json.Number("1.5")}} {
		if _, e := s.Ingest(v); e == nil {
			t.Fatal("invalid event accepted")
		}
	}
	for i := 0; i < 5; i++ {
		mustRecord(t, s, rawEvent(ID(), "task"))
	}
	a, _ := s.Records("task", "", 2, 0)
	b, _ := s.Records("task", "", 2, int(Number(a[1]["seq"])))
	if len(b) != 2 || Number(b[0]["seq"]) != 3 {
		t.Fatal(b)
	}
	if _, e := s.Records("task", "", 1001, 0); e == nil {
		t.Fatal("unbounded page")
	}
	mustRecord(t, s, rawEvent("unicode", "other"))
	x, _ := s.Records("task", "사용자", 100, 0)
	if len(x) != 5 {
		t.Fatal(x)
	}
}
func TestPathBoundary(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	os.Symlink(outside, filepath.Join(root, "escape"))
	if Within(root, filepath.Join(root, "escape", "new")) {
		t.Fatal("symlink escaped")
	}
	if Within(root, root+"-other") {
		t.Fatal("prefix escaped")
	}
	if !Within(root, filepath.Join(root, "new")) {
		t.Fatal("new path rejected")
	}
}
func TestPendingMalformedPreserved(t *testing.T) {
	s := fixture(t)
	p := filepath.Join(s.Root, "pending", "bad.json")
	os.WriteFile(p, []byte("{"), 0600)
	r, e := s.Replay()
	if e != nil || len(r) != 1 || r[0]["result"] != "invalid" {
		t.Fatal(r, e)
	}
	b, e := os.ReadFile(strings.TrimSuffix(p, ".json") + ".invalid")
	if e != nil || string(b) != "{" {
		t.Fatal(e)
	}
}
