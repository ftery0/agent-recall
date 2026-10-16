package memory

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBriefDisplayKeepsContentAndRecoverableMetadata(t *testing.T) {
	s := fixture(t)
	mustRecord(t, s, rawEvent("evidence", "task"))
	v := decision("current", "runtime", "revoked", 0)
	v["replacement"] = "new-runtime"
	v["custom"] = Object{"next": "full <content> 한국어🙂\x00 retained", "severity": false}
	mustRecord(t, s, v)
	path := filepath.Join(t.TempDir(), "checked.go")
	if e := os.WriteFile(path, []byte("current"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, authority := range []string{"local-command", "agent-reported", "user-confirmed"} {
		mustRecord(t, s, Object{
			"key": "check-" + authority, "task": "task", "session": "session", "kind": "verification", "subject": authority,
			"body": "검사 <output>🙂", "authority": authority, "checked_at": "2026-10-04", "command": []any{"test"}, "exit_code": 1,
			"passed": true, "source_keys": []any{"event:evidence"}, "snapshot": Object{path: Hash([]byte("previous"))}, "logs": strings.Repeat("raw-log", 1000),
		})
	}
	full, e := s.Recall("task")
	if e != nil {
		t.Fatal(e)
	}
	brief, e := s.Brief("task", 1)
	if e != nil || brief["over_budget"] != true {
		t.Fatal(brief, e)
	}
	for i, record := range full["current"].([]Object) {
		projected := brief["current"].([]Object)[i]
		for key, value := range record {
			if contains([]string{"task", "session", "applied", "expected_revision"}, key) {
				if _, exists := projected[key]; exists {
					t.Fatal("shared or ingestion metadata repeated", key)
				}
				continue
			}
			if !reflect.DeepEqual(value, projected[key]) {
				t.Fatal("semantic content changed", key, value, projected[key])
			}
		}
	}
	if brief["task"] != "task" || len(brief["checks"].([]Object)) != len(full["checks"].([]Object)) {
		t.Fatal("task or verification count lost")
	}
	for i, check := range full["checks"].([]Object) {
		projected := brief["checks"].([]Object)[i]
		for _, key := range []string{"key", "subject", "body", "created", "authority", "checked_at", "exit_code", "passed", "execution_recorded", "stale"} {
			if !reflect.DeepEqual(check[key], projected[key]) {
				t.Fatal("verification meaning changed", key)
			}
		}
		_, raw, e := s.Source(Text(projected["evidence"]))
		if e != nil {
			t.Fatal("verification evidence not recoverable", e)
		}
		recovered, e := Decode(raw)
		if e != nil || recovered["session"] != check["session"] || recovered["logs"] != check["logs"] || !reflect.DeepEqual(recovered["snapshot"], check["snapshot"]) || !reflect.DeepEqual(recovered["source_keys"], check["source_keys"]) {
			t.Fatal("full evidence changed", recovered, e)
		}
	}
	var compact, oldDisplay bytes.Buffer
	encoder := json.NewEncoder(&compact)
	encoder.SetEscapeHTML(false)
	if e = encoder.Encode(brief); e != nil {
		t.Fatal(e)
	}
	encoder = json.NewEncoder(&oldDisplay)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if e = encoder.Encode(full); e != nil {
		t.Fatal(e)
	}
	if brief["display_chars"] != utf8.RuneCount(compact.Bytes()) || compact.Len() >= oldDisplay.Len() || !bytes.Contains(compact.Bytes(), []byte("<output>")) {
		t.Fatal("display count, reduction, or HTML encoding changed")
	}
}
