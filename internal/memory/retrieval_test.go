package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func decision(key, subject, status string, revision int) Object {
	return Object{"key": key, "task": "task", "session": "session", "kind": "decision", "subject": subject, "body": "한국어 결정 " + key, "source_keys": []any{"event:evidence"}, "status": status, "scope": "task", "user_confirmed": true, "expected_revision": revision}
}

func TestBriefPreservesMeaningAndSoftBudget(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("evidence", "task"))
	mustRecord(t, s, rawEvent("evidence-two", "task"))
	d := decision("initial", "database", "adopted", 0)
	mustRecord(t, s, d)
	d = decision("revoked", "database", "revoked", 1)
	mustRecord(t, s, d)
	mustRecord(t, s, decision("new", "replacement", "adopted", 0))
	d = decision("old", "original", "adopted", 0)
	mustRecord(t, s, d)
	d = decision("replaced", "original", "replaced", 1)
	d["replacement"] = "replacement"
	mustRecord(t, s, d)
	d = decision("candidate", "style", "candidate", 0)
	d["kind"], d["user_confirmed"], d["source_keys"] = "preference", false, []any{"event:evidence", "event:evidence-two"}
	mustRecord(t, s, d)
	state := Object{"key": "state", "task": "task", "session": "session", "kind": "state", "subject": "task", "body": strings.Repeat("현재 목표🙂", 300), "status": "active", "expected_revision": 0, "next_steps": []any{"Keep custom state fields"}}
	mustRecord(t, s, state)
	full, e := s.Recall("task")
	if e != nil {
		t.Fatal(e)
	}
	brief, e := s.Brief("task", 100)
	if e != nil || brief["over_budget"] != true {
		t.Fatal(brief, e)
	}
	for i, record := range full["current"].([]Object) {
		projected := brief["current"].([]Object)[i]
		for key, value := range record {
			if contains([]string{"task", "session", "applied", "expected_revision"}, key) {
				continue
			}
			if !bytes.Equal(Packed(value), Packed(projected[key])) {
				t.Fatal("current memory changed in projection", key)
			}
		}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if e := encoder.Encode(brief); e != nil {
		t.Fatal(e)
	}
	if brief["display_chars"] != utf8.RuneCount(output.Bytes()) {
		t.Fatal("display count differs from CLI encoding", brief["display_chars"], output.Len())
	}
	if _, e = s.Brief("task", 1000000); e != nil {
		t.Fatal(e)
	}
	large, _ := s.Brief("task", 1000000)
	for _, budget := range []int{large["display_chars"].(int) - 1, large["display_chars"].(int), large["display_chars"].(int) + 1} {
		v, e := s.Brief("task", budget)
		if e != nil || v["over_budget"] != (v["display_chars"].(int) > budget) {
			t.Fatal("inconsistent boundary budget", budget, v, e)
		}
	}
}

func TestBriefVerificationAuthorityAndHashes(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("evidence", "task"))
	path := filepath.Join(t.TempDir(), "code.go")
	if e := os.WriteFile(path, []byte("current"), 0600); e != nil {
		t.Fatal(e)
	}
	for i, authority := range []string{"local-command", "local-command", "user-confirmed", "agent-reported"} {
		want := Hash([]byte("current"))
		if i == 1 {
			want = Hash([]byte("previous"))
		}
		v := Object{"key": fmt.Sprint("check", i), "task": "task", "session": "session", "kind": "verification", "subject": fmt.Sprint("test", i), "body": "test output", "source_keys": []any{"event:evidence"}, "authority": authority, "command": []any{"go", "test"}, "exit_code": 0, "passed": false, "checked_at": "2026-10-04", "snapshot": Object{path: want}, "logs": strings.Repeat("log", 2000)}
		mustRecord(t, s, v)
	}
	r, e := s.Brief("task", 100000)
	if e != nil {
		t.Fatal(e)
	}
	checks := r["checks"].([]Object)
	for i, check := range checks {
		if check["stale"] != (i == 1) || check["passed"] != (i != 2) || check["execution_recorded"] != (i != 3) || check["evidence"] == nil {
			t.Fatal(check)
		}
		for _, key := range []string{"snapshot", "command", "logs"} {
			if _, exists := check[key]; exists {
				t.Fatal("heavy field returned", key)
			}
		}
	}
	search, e := s.SearchBrief("task", "test output", 10, 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, check := range search["matches"].([]Object) {
		if check["stale"] != "not-checked" || check["authority"] == nil || check["execution_recorded"] != (check["authority"] != "agent-reported") || check["passed"] != (check["authority"] != "user-confirmed") {
			t.Fatal("search suggested checked proof", check)
		}
	}
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	r, e = s.Recall("task")
	if e != nil {
		t.Fatal(e)
	}
	for _, check := range r["checks"].([]Object) {
		if check["stale"] != true || check["snapshot"] == nil || check["command"] == nil || check["logs"] == nil {
			t.Fatal("full recall contract or missing-file staleness changed", check)
		}
	}
}

func TestLatestStateDoesNotReadVerifications(t *testing.T) {
	s := fixture(t)
	if state, e := s.LatestState("task"); e != nil || state != nil {
		t.Fatal(state, e)
	}
	for i, task := range []string{"task", "other"} {
		v := Object{"key": fmt.Sprint("state", i), "task": task, "session": "session", "kind": "state", "subject": "task", "body": task, "status": "active", "expected_revision": 0}
		mustRecord(t, s, v)
		v["key"], v["body"] = fmt.Sprint("conflict", i), "not applied"
		mustRecord(t, s, v)
	}
	state, e := s.LatestState("task")
	if e != nil || state["body"] != "task" || Number(state["revision"]) != 1 {
		t.Fatal(state, e)
	}
}

func TestSearchLatestScopedRankedAndPaged(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("evidence", "task"))
	d := decision("old", "database", "adopted", 0)
	d["body"] = "한국어 obsolete-only"
	mustRecord(t, s, d)
	d = decision("revoked", "database", "revoked", 1)
	mustRecord(t, s, d)
	d = decision("current", "runtime", "adopted", 0)
	mustRecord(t, s, d)
	d["key"], d["body"] = "conflict", "한국어 conflict-only"
	mustRecord(t, s, d)
	raw := rawEvent("raw", "task")
	raw["body"] = "한국어 raw evidence"
	mustRecord(t, s, raw)
	raw = rawEvent("other", "other")
	raw["body"] = "한국어 other private task"
	mustRecord(t, s, raw)
	for _, query := range []string{"obsolete-only", "conflict-only", "other private", "event:evidence"} {
		r, e := s.SearchBrief("task", query, 5, 0)
		if e != nil || len(r["matches"].([]Object)) != 0 {
			t.Fatal(query, r, e)
		}
	}
	want := []string{"current", "revoked", "raw"}
	for offset, key := range want {
		r, e := s.SearchBrief("task", "한국어", 1, offset)
		if e != nil {
			t.Fatal(e)
		}
		found := r["matches"].([]Object)
		if len(found) != 1 || found[0]["key"] != key || r["has_more"] != (offset < 2) {
			t.Fatal(r)
		}
		if offset < 2 && found[0]["user_confirmed"] != true || offset == 2 && found[0]["role"] != "raw-evidence" {
			t.Fatal(found)
		}
	}
}

func TestSearchUnicodeSnippetsAndSubjectMatch(t *testing.T) {
	s := fixture(t)
	raw := rawEvent("long", "task")
	raw["subject"] = "subject-match"
	raw["body"] = strings.Repeat("🙂가", 50000) + "찾을말" + strings.Repeat("끝", 50000)
	mustRecord(t, s, raw)
	for _, query := range []string{"찾을말", "subject-match"} {
		r, e := s.SearchBrief("task", query, 5, 0)
		if e != nil {
			t.Fatal(e)
		}
		match := r["matches"].([]Object)[0]
		body := Text(match["body"])
		if !utf8.ValidString(body) || utf8.RuneCountInString(body) != 320 || match["truncated"] != true || Number(match["body_chars"]) != 150003 {
			t.Fatal(match)
		}
		if query == "찾을말" && (!strings.Contains(body, query) || Number(match["snippet_offset"]) != 99880) {
			t.Fatal(match)
		}
	}
}

func TestSourceExcerptUnicodeBoundsAndIntegrity(t *testing.T) {
	s := fixture(t)
	id, e := s.Artifact([]byte("A한🙂B끝"), "source")
	if e != nil {
		t.Fatal(e)
	}
	part, e := s.SourceExcerpt(id, 1, 2)
	if e != nil || part["text"] != "한🙂" || part["next_offset"] != 3 || part["total_chars"] != 5 {
		t.Fatal(part, e)
	}
	part, e = s.SourceExcerpt(id, 5, 2)
	if e != nil || part["text"] != "" || part["next_offset"] != nil {
		t.Fatal(part, e)
	}
	if _, e = s.SourceExcerpt(id, 6, 2); e == nil {
		t.Fatal("beyond-end offset accepted")
	}
	binaryID, e := s.Artifact([]byte{0xff, 0xfe}, "binary")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SourceExcerpt(binaryID, 0, 2); e == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
	db, e := s.Connect(false)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("UPDATE artifacts SET bytes=? WHERE hash=?", []byte("tampered"), part["hash"])
	db.Close()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.SourceExcerpt(id, 0, 1); e == nil {
		t.Fatal("checksum mismatch accepted")
	}
}

func TestSearchAfterEmbeddedNUL(t *testing.T) {
	s := fixture(t)
	v := rawEvent("nul", "task")
	v["body"] = "처음\x00뒤쪽검색문구"
	mustRecord(t, s, v)
	r, e := s.SearchBrief("task", "검색", 5, 0)
	if e != nil {
		t.Fatal(e)
	}
	match := r["matches"].([]Object)[0]
	if match["body"] != v["body"] || Number(match["body_chars"]) != 9 || match["nul_fallback"] != true || match["truncated"] != false {
		t.Fatal(match)
	}
}

func FuzzSourceExcerptRoundTrip(f *testing.F) {
	for _, seed := range []string{"", "한글", "🙂👨‍👩‍👧", "e\u0301", "before\x00after"} {
		f.Add(seed, uint16(1), uint16(3))
	}
	f.Fuzz(func(t *testing.T, body string, requestedOffset, requestedLimit uint16) {
		if !utf8.ValidString(body) || len(body) > 4096 {
			t.Skip()
		}
		s := fixture(t)
		id, e := s.Artifact([]byte(body), "fuzz")
		if e != nil {
			t.Fatal(e)
		}
		runes := []rune(body)
		offset, limit := int(requestedOffset)%(len(runes)+1), int(requestedLimit)%4096+1
		part, e := s.SourceExcerpt(id, offset, limit)
		end := min(offset+limit, len(runes))
		if e != nil || !utf8.ValidString(Text(part["text"])) || part["text"] != string(runes[offset:end]) || part["hash"] != Hash([]byte(body)) {
			t.Fatal(part, e)
		}
		var joined strings.Builder
		for at := 0; ; {
			chunk, e := s.SourceExcerpt(id, at, 257)
			if e != nil {
				t.Fatal(e)
			}
			joined.WriteString(Text(chunk["text"]))
			if chunk["next_offset"] == nil {
				break
			}
			at = chunk["next_offset"].(int)
		}
		if joined.String() != body {
			t.Fatal("excerpt paging changed source")
		}
		if _, e := s.SourceExcerpt(id, len(runes)+1, 1); e == nil {
			t.Fatal("invalid offset accepted")
		}
	})
}

func TestRetrievalRejectsInvalidArguments(t *testing.T) {
	s := fixture(t)
	checks := []func() error{
		func() error { _, e := s.LatestState("../task"); return e },
		func() error { _, e := s.Brief("task", 0); return e },
		func() error { _, e := s.Brief("task", 1000001); return e },
		func() error { _, e := s.Brief("../task", 100); return e },
		func() error { _, e := s.SearchBrief("task", "", 5, 0); return e },
		func() error { _, e := s.SearchBrief("task", " \n", 5, 0); return e },
		func() error { _, e := s.SearchBrief("task", "a", 0, 0); return e },
		func() error { _, e := s.SearchBrief("task", "a", 101, 0); return e },
		func() error { _, e := s.SearchBrief("task", "a", 5, -1); return e },
		func() error { _, e := s.SearchBrief("task", "\xff", 5, 0); return e },
		func() error { _, e := s.SearchBrief("task", strings.Repeat("가", 4097), 5, 0); return e },
		func() error { _, e := s.SearchBrief("task", "a\x00", 5, 0); return e },
		func() error { _, e := s.SourceExcerpt("missing", -1, 2); return e },
		func() error { _, e := s.SourceExcerpt("missing", 0, 0); return e },
		func() error { _, e := s.SourceExcerpt("missing", 0, 100001); return e },
	}
	for i, check := range checks {
		if check() == nil {
			t.Fatal("invalid argument accepted", i)
		}
	}
}

func TestRetrievalConcurrentReadersAndWriter(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("evidence", "task"))
	mustRecord(t, s, decision("current", "runtime", "adopted", 0))
	var wg sync.WaitGroup
	errs := make(chan error, 61)
	for worker := 0; worker < 3; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, e := s.Brief("task", 6000)
				errs <- e
				_, e = s.SearchBrief("task", "한국어", 5, 0)
				errs <- e
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if _, e := s.Ingest(rawEvent(fmt.Sprint("concurrent", i), "task")); e != nil {
				errs <- e
				return
			}
		}
	}()
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
	h, e := s.Health()
	if e != nil || h["events"] != 12 || h["integrity"] != "ok" {
		t.Fatal(h, e)
	}
}
