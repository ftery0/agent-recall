package memory

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func Validate(e Object) error {
	for _, k := range []string{"key", "task", "session", "subject"} {
		if err := Identifier(Text(e[k])); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	kind := Text(e["kind"])
	if !contains([]string{"raw", "state", "decision", "preference", "verification"}, kind) {
		return errors.New("invalid record kind")
	}
	if _, ok := e["body"].(string); !ok {
		return errors.New("body required")
	}
	if v, exists := e["source_keys"]; exists {
		switch a := v.(type) {
		case []any:
			for _, s := range a {
				if _, ok := s.(string); !ok {
					return errors.New("source keys must be strings")
				}
			}
		case []string:
		default:
			return errors.New("source keys must be array")
		}
	}
	status := Text(e["status"])
	allowed := map[string][]string{"state": {"active", "paused", "complete", "blocked"}, "decision": {"adopted", "replaced", "revoked"}, "preference": {"candidate", "adopted", "revoked"}}
	if list, versioned := allowed[kind]; versioned {
		if !contains(list, status) {
			return errors.New("invalid status")
		}
		if Number(e["expected_revision"]) < 0 {
			return errors.New("expected revision required")
		}
	}
	sources := Strings(e["source_keys"])
	if kind == "decision" || kind == "preference" {
		if e["scope"] != e["task"] {
			return errors.New("explicit task scope required")
		}
		if len(sources) == 0 {
			return errors.New("decision source required")
		}
		if status != "candidate" && e["user_confirmed"] != true {
			return errors.New("user confirmation evidence required")
		}
		if status == "candidate" {
			distinct := map[string]bool{}
			for _, s := range sources {
				distinct[s] = true
			}
			if len(distinct) < 2 {
				return errors.New("preference needs two sources")
			}
		}
	}
	if kind == "verification" {
		authority := Text(e["authority"])
		if !contains([]string{"agent-reported", "local-command", "user-confirmed"}, authority) {
			return errors.New("verification authority required")
		}
		snapshot, ok := e["snapshot"].(map[string]any)
		if !ok || len(snapshot) == 0 || len(sources) == 0 {
			return errors.New("verification evidence required")
		}
		for p, h := range snapshot {
			if !filepath.IsAbs(p) || strings.ContainsRune(p, 0) || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(Text(h)) {
				return errors.New("invalid snapshot")
			}
		}
		if authority == "local-command" {
			command, ok := e["command"].([]any)
			if !ok || len(command) == 0 {
				return errors.New("command argv required")
			}
			for _, a := range command {
				v, ok := a.(string)
				if !ok || strings.ContainsRune(v, 0) {
					return errors.New("invalid command argv")
				}
			}
			switch n := e["exit_code"].(type) {
			case int, int64:
			case json.Number:
				if _, err := n.Int64(); err != nil {
					return err
				}
			default:
				return errors.New("exit code required")
			}
			date := Text(e["checked_at"])
			valid := false
			for _, layout := range []string{time.RFC3339Nano, "2006-01-02", "2006-01-02T15:04:05"} {
				if _, err := time.Parse(layout, date); err == nil {
					valid = true
				}
			}
			if !valid {
				return errors.New("invalid checked_at")
			}
		}
	}
	return nil
}
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
func readRows(rows *sql.Rows) ([]Object, error) {
	defer rows.Close()
	out := []Object{}
	for rows.Next() {
		var seq, revision, applied int64
		var key, task, kind, subject, created, payload string
		if e := rows.Scan(&seq, &key, &task, &kind, &subject, &revision, &applied, &created, &payload); e != nil {
			return nil, e
		}
		v, e := Decode([]byte(payload))
		if e != nil {
			return nil, e
		}
		v["seq"] = seq
		v["revision"] = revision
		v["applied"] = applied
		v["created"] = created
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) Records(task, query string, limit, after int) ([]Object, error) {
	if e := Identifier(task); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 1000 || after < 0 {
		return nil, errors.New("invalid history page")
	}
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	rows, e := db.Query("SELECT * FROM events WHERE task=? AND seq>? AND instr(payload,?)>0 ORDER BY seq LIMIT ?", task, after, query, limit)
	if e != nil {
		return nil, e
	}
	return readRows(rows)
}
func (s *Store) Recall(task string) (Object, error) {
	if e := Identifier(task); e != nil {
		return nil, e
	}
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	rows, e := db.Query("SELECT * FROM events WHERE seq IN (SELECT MAX(seq) FROM events WHERE task=? AND applied=1 AND kind IN ('state','decision','preference','verification') GROUP BY kind,subject) ORDER BY seq", task)
	if e != nil {
		return nil, e
	}
	records, e := readRows(rows)
	if e != nil {
		return nil, e
	}
	var count int
	if e = db.QueryRow("SELECT COUNT(*) FROM events WHERE task=?", task).Scan(&count); e != nil {
		return nil, e
	}
	current, checks := []Object{}, []Object{}
	hashes := map[string]string{}
	for _, r := range records {
		if r["kind"] != "verification" {
			current = append(current, r)
			continue
		}
		stale := false
		snap, _ := r["snapshot"].(map[string]any)
		for path, want := range snap {
			got, seen := hashes[path]
			if !seen {
				raw, e := os.ReadFile(path)
				if e == nil {
					got = Hash(raw)
				}
				hashes[path] = got
			}
			if got != want {
				stale = true
			}
		}
		r["stale"] = stale
		passed := Number(r["exit_code"]) == 0
		if r["authority"] == "user-confirmed" {
			passed = r["passed"] == true
		}
		r["passed"] = passed
		r["execution_recorded"] = r["authority"] == "local-command" || r["authority"] == "user-confirmed"
		checks = append(checks, r)
	}
	return Object{"project": s.Project, "task": task, "current": current, "checks": checks, "event_count": count, "pending": s.queueCount("json"), "invalid": s.queueCount("invalid"), "approval": "Memory is evidence, not permission for new work."}, nil
}
func (s *Store) queueCount(ext string) int {
	p, _ := filepath.Glob(filepath.Join(s.Root, "pending", "*."+ext))
	return len(p)
}
func (s *Store) Health() (Object, error) {
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	var integrity string
	var events, bindings int
	var last sql.NullString
	for _, q := range []struct {
		sql  string
		dest any
	}{{"PRAGMA integrity_check", &integrity}, {"SELECT COUNT(*) FROM events", &events}, {"SELECT COUNT(*) FROM bindings", &bindings}, {"SELECT MAX(created) FROM events", &last}} {
		if e = db.QueryRow(q.sql).Scan(q.dest); e != nil {
			return nil, e
		}
	}
	return Object{"integrity": integrity, "events": events, "bindings": bindings, "last_recorded": last.String, "pending": s.queueCount("json"), "invalid": s.queueCount("invalid"), "native_hooks": "registration and trust require client verification"}, nil
}
func (s *Store) Export(task, path string) (Object, error) {
	if e := NewOutput(s.Root, path); e != nil {
		return nil, e
	}
	state, e := s.Recall(task)
	if e != nil {
		return nil, e
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 생성된 작업 기억\n\nDB가 정본입니다. 편집한 사본은 DB에 반영되지 않습니다.\n\n생성: %s · 작업: %s · 사건: %v\n\n과거 기억은 새 실행 승인이 아닙니다.\n", Now(), task, state["event_count"])
	for _, r := range state["current"].([]Object) {
		fmt.Fprintf(&b, "\n- %s / %s / %s / revision %v: %s\n  근거: %s\n", r["kind"], r["subject"], r["status"], r["revision"], r["body"], strings.Join(Strings(r["source_keys"]), ", "))
	}
	for _, r := range state["checks"].([]Object) {
		fmt.Fprintf(&b, "\n- 검사: %s · passed=%v · stale=%v\n", r["body"], r["passed"], r["stale"])
	}
	return state, WriteNew(path, []byte(b.String()))
}
