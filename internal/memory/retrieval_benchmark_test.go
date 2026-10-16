package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic public-ingest setup is excluded from each measured retrieval.
func BenchmarkRetrieval(b *testing.B) {
	s, e := Open(b.TempDir(), "project", true)
	if e != nil {
		b.Fatal(e)
	}
	add := func(v Object) {
		b.Helper()
		r, e := s.Ingest(v)
		if e != nil || r["result"] != "recorded" {
			b.Fatal(r, e)
		}
	}
	for i := 0; i < 1000; i++ {
		v := rawEvent(fmt.Sprint("raw", i), "task")
		v["body"] = strings.Repeat("synthetic log 한국어 ", 100) + " needle " + fmt.Sprint(i)
		add(v)
	}
	add(Object{"key": "state", "task": "task", "session": "session", "kind": "state", "subject": "task", "body": "current goal", "status": "active", "expected_revision": 0})
	for i := 0; i < 10; i++ {
		v := decision(fmt.Sprint("decision", i), fmt.Sprint("subject", i), "adopted", 0)
		v["source_keys"] = []any{"event:raw0"}
		add(v)
	}
	snapshot := Object{}
	for i := 0; i < 10; i++ {
		path := filepath.Join(b.TempDir(), "code.go")
		data := []byte(strings.Repeat("synthetic source\n", 1000))
		if e := os.WriteFile(path, data, 0600); e != nil {
			b.Fatal(e)
		}
		snapshot[path] = Hash(data)
	}
	for i := 0; i < 20; i++ {
		add(Object{"key": fmt.Sprint("check", i), "task": "task", "session": "session", "kind": "verification", "subject": fmt.Sprint("test", i), "body": "synthetic test", "source_keys": []any{"event:raw0"}, "authority": "local-command", "command": []any{"go", "test"}, "exit_code": 0, "checked_at": "2026-10-04", "snapshot": snapshot})
	}
	for _, candidate := range []struct {
		name string
		read func() (Object, error)
	}{
		{"Recall", func() (Object, error) { return s.Recall("task") }},
		{"Brief", func() (Object, error) { return s.Brief("task", 6000) }},
		{"LatestState", func() (Object, error) { return s.LatestState("task") }},
		{"SearchBrief", func() (Object, error) { return s.SearchBrief("task", "needle", 5, 0) }},
	} {
		b.Run(candidate.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, e := candidate.read(); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
