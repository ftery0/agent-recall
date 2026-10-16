package memory

import (
	"database/sql"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Store struct{ Root, Project, Path string }

var errInvalidEvent = errors.New("invalid event")

const schema = `
CREATE TABLE IF NOT EXISTS events(seq INTEGER PRIMARY KEY,key TEXT UNIQUE NOT NULL,task TEXT NOT NULL,kind TEXT NOT NULL,subject TEXT NOT NULL,revision INTEGER NOT NULL,applied INTEGER NOT NULL,created TEXT NOT NULL,payload TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS event_subject ON events(task,kind,subject,applied,seq);
CREATE TABLE IF NOT EXISTS artifacts(hash TEXT PRIMARY KEY,origin TEXT NOT NULL,captured TEXT NOT NULL,bytes BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS sources(id TEXT PRIMARY KEY,hash TEXT NOT NULL,origin TEXT NOT NULL,captured TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS bindings(tool TEXT NOT NULL,session TEXT NOT NULL,task TEXT NOT NULL,cwd TEXT NOT NULL,PRIMARY KEY(tool,session));`

func Open(root, project string, initialize bool) (*Store, error) {
	if e := Identifier(project); e != nil {
		return nil, e
	}
	resolved, e := Resolve(root)
	if e != nil {
		return nil, e
	}
	s := &Store{Root: filepath.Join(resolved, project), Project: project}
	s.Path = filepath.Join(s.Root, "memory.sqlite3")
	if initialize {
		for _, p := range []string{s.Root, filepath.Join(s.Root, "pending")} {
			if e = os.MkdirAll(p, 0700); e != nil {
				return nil, e
			}
			if e = os.Chmod(p, 0700); e != nil {
				return nil, e
			}
		}
		unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
		if e != nil {
			return nil, e
		}
		defer unlock()
		db, e := s.Connect(false)
		if e != nil {
			return nil, e
		}
		defer db.Close()
		if _, e = db.Exec("PRAGMA journal_mode=DELETE"); e != nil {
			return nil, e
		}
		if _, e = db.Exec(schema); e != nil {
			return nil, e
		}
		if e = os.Chmod(s.Path, 0600); e != nil {
			return nil, e
		}
	}
	if _, e = os.Stat(s.Path); e != nil {
		return nil, fmt.Errorf("memory not initialized: %w", e)
	}
	return s, nil
}
func (s *Store) Connect(readonly bool) (*sql.DB, error) {
	mode := "rw"
	if readonly {
		mode = "ro"
	} else if _, e := os.Stat(s.Path); os.IsNotExist(e) {
		mode = "rwc"
	}
	u := url.URL{Scheme: "file", Path: s.Path}
	q := u.Query()
	q.Set("mode", mode)
	q.Add("_pragma", "busy_timeout(100)")
	if !readonly {
		q.Add("_pragma", "synchronous(FULL)")
	}
	u.RawQuery = q.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	if e = db.Ping(); e != nil {
		db.Close()
		return nil, e
	}
	var version int
	if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
		db.Close()
		return nil, e
	}
	if version != 0 {
		db.Close()
		return nil, fmt.Errorf("unsupported database schema version %d", version)
	}
	return db, nil
}
func (s *Store) Artifact(data []byte, origin string) (string, error) {
	if origin == "" {
		return "", errors.New("source origin required")
	}
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		return "", e
	}
	defer unlock()
	db, e := s.Connect(false)
	if e != nil {
		return "", e
	}
	defer db.Close()
	tx, e := db.Begin()
	if e != nil {
		return "", e
	}
	defer tx.Rollback()
	hash := Hash(data)
	id := Hash(Packed([]any{hash, origin}))
	if _, e = tx.Exec("INSERT OR IGNORE INTO artifacts VALUES(?,?,?,?)", hash, origin, Now(), data); e != nil {
		return "", e
	}
	if _, e = tx.Exec("INSERT OR IGNORE INTO sources VALUES(?,?,?,?)", id, hash, origin, Now()); e != nil {
		return "", e
	}
	return id, tx.Commit()
}
func (s *Store) Source(key string) (Object, []byte, error) {
	db, e := s.Connect(true)
	if e != nil {
		return nil, nil, e
	}
	defer db.Close()
	var hash, origin, captured string
	var data []byte
	if strings.HasPrefix(key, "event:") {
		e = db.QueryRow("SELECT payload,created FROM events WHERE key=?", key[6:]).Scan(&data, &captured)
		if e != nil {
			return nil, nil, e
		}
		return Object{"hash": Hash(data), "origin": key, "captured": captured}, data, nil
	}
	var sourceHash string
	e = db.QueryRow("SELECT hash,origin,captured FROM sources WHERE id=?", key).Scan(&sourceHash, &origin, &captured)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, nil, e
	}
	hasSource := e == nil
	if !hasSource {
		sourceHash = key
	}
	var oldOrigin, oldCaptured string
	e = db.QueryRow("SELECT hash,origin,captured,bytes FROM artifacts WHERE hash=?", sourceHash).Scan(&hash, &oldOrigin, &oldCaptured, &data)
	if e != nil {
		return nil, nil, e
	}
	if Hash(data) != hash {
		return nil, nil, errors.New("source checksum mismatch")
	}
	if !hasSource {
		origin = oldOrigin
		captured = oldCaptured
	}
	v := Object{"hash": hash, "origin": origin, "captured": captured}
	if hasSource {
		v["source_id"] = key
	}
	return v, data, nil
}
func (s *Store) Ingest(event Object) (Object, error) {
	if e := Validate(event); e != nil {
		return nil, e
	}
	raw, e := Canonical(event)
	if e != nil {
		return nil, e
	}
	path := filepath.Join(s.Root, "pending", fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), Hash(raw)))
	if e = PrivateWrite(path, raw); e != nil {
		return nil, e
	}
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if errors.Is(e, ErrBusy) {
		return Object{"result": "queued"}, nil
	}
	if e != nil {
		return nil, e
	}
	defer unlock()
	// Include the caller in the same ordered batch. Skipping it and applying
	// it afterward would let it overtake a predecessor beyond the batch limit,
	// or let a concurrent writer's later event overtake this caller.
	replayed, recoveryErr := s.replayLocked(8, 100*time.Millisecond, "")
	recoveryIssues := []Object{}
	var result Object
	for _, r := range replayed {
		if r["file"] == filepath.Base(path) {
			result = r
			continue
		}
		if r["result"] == "invalid" || r["result"] == "recovery-error" {
			recoveryIssues = append(recoveryIssues, r)
		}
	}
	if recoveryErr != nil {
		recoveryIssues = append(recoveryIssues, Object{"result": "recovery-error", "reason": recoveryErr.Error()})
	}
	if result == nil {
		if _, e = os.Lstat(path); e == nil {
			result = Object{"result": "queued"}
		} else if os.IsNotExist(e) {
			// Another writer may already have recovered this exact occurrence.
			// apply only returns duplicate here, or rejects a key collision.
			result, e = s.apply(event, raw)
			if e != nil {
				return nil, e
			}
		} else {
			return nil, e
		}
	} else if result["result"] == "invalid" || result["result"] == "recovery-error" {
		return nil, errors.New(Text(result["reason"]))
	}
	delete(result, "file")
	if len(recoveryIssues) > 0 {
		result["recovery_issues"] = recoveryIssues
	}
	return result, nil
}
func busy(e error) bool {
	return e != nil && (strings.Contains(e.Error(), "SQLITE_BUSY") || strings.Contains(e.Error(), "SQLITE_LOCKED"))
}
func (s *Store) apply(event Object, raw []byte) (Object, error) {
	if e := Validate(event); e != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidEvent, e)
	}
	db, e := s.Connect(false)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	tx, e := db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var old string
	var seq int64
	e = tx.QueryRow("SELECT seq,payload FROM events WHERE key=?", event["key"]).Scan(&seq, &old)
	if e == nil {
		if old != string(raw) {
			return nil, fmt.Errorf("%w: same key with different payload", errInvalidEvent)
		}
		return Object{"result": "duplicate", "seq": seq}, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	for _, key := range Strings(event["source_keys"]) {
		var n int
		if strings.HasPrefix(key, "event:") {
			e = tx.QueryRow("SELECT COUNT(*) FROM events WHERE key=? AND task=?", key[6:], event["task"]).Scan(&n)
		} else {
			e = tx.QueryRow("SELECT COUNT(*) FROM (SELECT hash FROM artifacts WHERE hash=? UNION SELECT id FROM sources WHERE id=?)", key, key).Scan(&n)
		}
		if e != nil {
			return nil, e
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: missing source", errInvalidEvent)
		}
	}
	var revision int64
	e = tx.QueryRow("SELECT revision FROM events WHERE task=? AND kind=? AND subject=? AND applied=1 ORDER BY seq DESC LIMIT 1", event["task"], event["kind"], event["subject"]).Scan(&revision)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	kind := Text(event["kind"])
	versioned := kind == "state" || kind == "decision" || kind == "preference"
	applied := !versioned || Number(event["expected_revision"]) == revision
	if applied && kind == "decision" && event["status"] == "replaced" {
		var replacement string
		e = tx.QueryRow("SELECT payload FROM events WHERE task=? AND kind='decision' AND subject=? AND applied=1 ORDER BY seq DESC LIMIT 1", event["task"], event["replacement"]).Scan(&replacement)
		v, _ := Decode([]byte(replacement))
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if e != nil || v["status"] != "adopted" || event["replacement"] == event["subject"] {
			return nil, fmt.Errorf("%w: replacement must be a different adopted decision", errInvalidEvent)
		}
	}
	if applied && versioned {
		revision++
	}
	flag := 0
	if applied {
		flag = 1
	}
	r, e := tx.Exec("INSERT INTO events(key,task,kind,subject,revision,applied,created,payload) VALUES(?,?,?,?,?,?,?,?)", event["key"], event["task"], event["kind"], event["subject"], revision, flag, Now(), string(raw))
	if e != nil {
		return nil, e
	}
	seq, e = r.LastInsertId()
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	status := "recorded"
	if !applied {
		status = "conflict"
	}
	return Object{"result": status, "seq": seq}, nil
}
func (s *Store) Replay() ([]Object, error) {
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		return nil, e
	}
	defer unlock()
	results, e := s.replayLocked(0, 0, "")
	for _, r := range results {
		if r["result"] == "recorded" || r["result"] == "duplicate" || r["result"] == "conflict" {
			delete(r, "file")
		}
	}
	return results, e
}

// Zero limits are reserved for the explicit replay command. Automatic ingest
// recovery checks the time budget between records and handles at most limit.
func (s *Store) replayLocked(limit int, budget time.Duration, skip string) ([]Object, error) {
	paths, e := filepath.Glob(filepath.Join(s.Root, "pending", "*.json"))
	if e != nil {
		return nil, e
	}
	results := []Object{}
	started := time.Now()
	examined := 0
	for _, path := range paths {
		if path == skip {
			continue
		}
		if (limit > 0 && examined >= limit) || (budget > 0 && time.Since(started) >= budget) {
			break
		}
		examined++
		info, e := os.Lstat(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || !info.Mode().IsRegular() {
			reason := "pending entry is not a regular file"
			if e != nil {
				reason = e.Error()
			}
			quarantined := quarantinePending(path, reason)
			results = append(results, quarantined)
			if quarantined["result"] == "recovery-error" {
				break
			}
			continue
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			quarantined := quarantinePending(path, e.Error())
			results = append(results, quarantined)
			if quarantined["result"] == "recovery-error" {
				break
			}
			continue
		}
		event, e := Decode(raw)
		malformed := e != nil
		var result Object
		if e == nil {
			result, e = s.apply(event, raw)
		}
		if busy(e) {
			break
		}
		if e != nil {
			if malformed || errors.Is(e, errInvalidEvent) {
				quarantined := quarantinePending(path, e.Error())
				results = append(results, quarantined)
				if quarantined["result"] == "recovery-error" {
					break
				}
				continue
			}
			// Keep a valid predecessor for retry and stop. Applying later files
			// could quarantine their still-pending source or corrupt revisions.
			results = append(results, Object{"result": "recovery-error", "file": filepath.Base(path), "reason": e.Error(), "preserved": "pending"})
			break
		}
		if e = os.Remove(path); e != nil {
			results = append(results, Object{"result": "recovery-error", "file": filepath.Base(path), "reason": e.Error()})
			break
		}
		result["file"] = filepath.Base(path)
		results = append(results, result)
	}
	return results, nil
}

// Rename the original entry, including its mode or symlink, without replacing
// an older quarantine. If preservation cannot be confirmed, leave it in place.
func quarantinePending(path, reason string) Object {
	target := strings.TrimSuffix(path, ".json") + ".invalid"
	for {
		_, e := os.Lstat(target)
		if os.IsNotExist(e) {
			break
		}
		if e != nil {
			return Object{"result": "recovery-error", "file": filepath.Base(path), "reason": reason, "quarantine": "destination cannot be inspected; original retained"}
		}
		target = strings.TrimSuffix(path, ".json") + "-" + ID() + ".invalid"
	}
	if e := os.Rename(path, target); e != nil {
		return Object{"result": "recovery-error", "file": filepath.Base(path), "reason": reason, "quarantine": e.Error()}
	}
	return Object{"result": "invalid", "file": filepath.Base(path), "reason": reason}
}
func Strings(v any) []string {
	if a, ok := v.([]string); ok {
		return a
	}
	a, _ := v.([]any)
	r := []string{}
	for _, s := range a {
		r = append(r, Text(s))
	}
	return r
}
