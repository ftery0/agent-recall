package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func queueFixture(t *testing.T, s *Store, order int, event Object) {
	t.Helper()
	if e := PrivateWrite(filepath.Join(s.Root, "pending", fmt.Sprintf("%020d-fixture.json", order)), Packed(event)); e != nil {
		t.Fatal(e)
	}
}

func TestBacklogPreservesSourceAndRevisionPredecessors(t *testing.T) {
	for _, mode := range []string{"source", "revision"} {
		t.Run(mode, func(t *testing.T) {
			s := fixture(t)
			for i := 0; i < 8; i++ {
				queueFixture(t, s, i, rawEvent(fmt.Sprintf("old-%d", i), "task"))
			}
			predecessor := rawEvent("predecessor", "task")
			newer := rawEvent("newer", "task")
			if mode == "source" {
				newer["source_keys"] = []any{"event:predecessor"}
			} else {
				predecessor["kind"], predecessor["status"], predecessor["expected_revision"] = "state", "active", 0
				newer["kind"], newer["status"], newer["expected_revision"] = "state", "complete", 1
			}
			queueFixture(t, s, 8, predecessor)
			if r := mustRecord(t, s, newer); r["result"] != "queued" {
				t.Fatal("newer event overtook ninth predecessor", r)
			}
			if s.queueCount("json") != 2 || s.queueCount("invalid") != 0 {
				t.Fatal("pending dependency changed or invalidated")
			}
			if _, e := s.Replay(); e != nil {
				t.Fatal(e)
			}
			records, e := s.Records("task", "", 100, 0)
			if e != nil || len(records) != 10 || records[8]["key"] != "predecessor" || records[9]["key"] != "newer" || Number(records[9]["applied"]) != 1 {
				t.Fatal(records, e)
			}
			if mode == "revision" && Number(records[9]["revision"]) != 2 {
				t.Fatal("queued revision did not advance", records[9])
			}
		})
	}
}

func TestReplayDoesNotInvalidateDependentAfterInfrastructureFailure(t *testing.T) {
	s := fixture(t)
	id, e := s.Artifact([]byte("source"), "fixture://source")
	if e != nil {
		t.Fatal(e)
	}
	first := rawEvent("first", "task")
	first["source_keys"] = []any{id}
	second := rawEvent("second", "task")
	second["source_keys"] = []any{"event:first"}
	queueFixture(t, s, 0, first)
	queueFixture(t, s, 1, second)
	db, e := s.Connect(false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("ALTER TABLE artifacts RENAME TO hidden_artifacts")
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	if out, e := s.Replay(); e != nil || len(out) != 1 || out[0]["result"] != "recovery-error" || s.queueCount("json") != 2 || s.queueCount("invalid") != 0 {
		t.Fatal("replay advanced beyond retryable predecessor", out, e)
	}
	if r := mustRecord(t, s, rawEvent("current", "task")); r["result"] != "queued" {
		t.Fatal("ingest advanced beyond retryable predecessor", r)
	}
	db, e = s.Connect(false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("ALTER TABLE hidden_artifacts RENAME TO artifacts")
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Replay(); e != nil {
		t.Fatal(e)
	}
	records, e := s.Records("task", "", 100, 0)
	if e != nil || len(records) != 3 || records[0]["key"] != "first" || records[1]["key"] != "second" || records[2]["key"] != "current" {
		t.Fatal(records, e)
	}
}

func TestBacklogDuplicateAndCollisionAreCheckedAfterPredecessors(t *testing.T) {
	s := fixture(t)
	original := rawEvent("same", "task")
	mustRecord(t, s, original)
	for i := 0; i < 9; i++ {
		queueFixture(t, s, i, rawEvent(fmt.Sprintf("old-%d", i), "task"))
	}
	collision := rawEvent("same", "task")
	collision["body"] = "changed"
	if r := mustRecord(t, s, collision); r["result"] != "queued" {
		t.Fatal(r)
	}
	duplicate := mustRecord(t, s, original)
	if duplicate["result"] != "duplicate" {
		t.Fatal(duplicate)
	}
	if s.queueCount("invalid") != 1 {
		t.Fatal("collision original not preserved")
	}
	matches, e := filepath.Glob(filepath.Join(s.Root, "pending", "*.invalid"))
	if e != nil || len(matches) != 1 {
		t.Fatal(matches, e)
	}
	raw, e := os.ReadFile(matches[0])
	if e != nil || string(raw) != string(Packed(collision)) {
		t.Fatal("collision bytes changed", e)
	}
}
