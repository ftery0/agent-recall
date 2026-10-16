package policy

import (
	"encoding/json"
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Observation struct {
	Before Metrics `json:"before"`
	Digest string  `json:"digest"`
}

func (v Observation) MarshalJSON() ([]byte, error) {
	var digest any = v.Digest
	if v.Digest == "" {
		digest = nil
	}
	return json.Marshal(struct {
		Before Metrics `json:"before"`
		Digest any     `json:"digest"`
	}{v.Before, digest})
}

type Call struct {
	Files         map[string]Observation `json:"files"`
	Moves         map[string]string      `json:"moves"`
	Complete      bool                   `json:"complete"`
	Ambiguous     bool                   `json:"ambiguous,omitempty"`
	ToolUseID     string                 `json:"tool_use_id,omitempty"`
	ObservationID string                 `json:"observation_id,omitempty"`
}
type Collector struct {
	Root, Runtime, Tool, Session, Task string
	compat                             bool
}
type checkCache struct {
	Key    string `json:"key"`
	Output string `json:"output"`
	Code   int    `json:"code"`
}
type EditState struct {
	Root             string            `json:"root"`
	Observed         bool              `json:"observed"`
	Files            map[string]Change `json:"files"`
	Pending          map[string]Call   `json:"pending"`
	Directories      map[string]int    `json:"directories"`
	Unverified       []string          `json:"unverified"`
	UnverifiedEvents map[string]string `json:"unverified_events,omitempty"`
	Blocked          map[string]bool   `json:"blocked,omitempty"`
	Turns            map[string]int    `json:"turns,omitempty"`
	Cache            *checkCache       `json:"check_cache,omitempty"`
	LastNotice       string            `json:"last_notice,omitempty"`
}
type baseline struct {
	Root        string            `json:"root"`
	Files       map[string]Change `json:"files"`
	Directories map[string]int    `json:"directories"`
}

func (c Collector) path() string {
	if root, err := memory.Resolve(c.Root); err == nil {
		c.Root = root
	}
	if c.compat {
		return filepath.Join(c.Runtime, "harness-edit-scope", memory.Hash([]byte(c.Tool+":"+c.Session+":"+c.Root+":"+c.Task))+".json")
	}
	return filepath.Join(c.Runtime, "edits-"+memory.Hash(memory.Packed([]any{c.Root, c.Tool, c.Session, c.Task}))+".json")
}
func freshState(root string) EditState {
	return EditState{Root: root, Files: map[string]Change{}, Pending: map[string]Call{}, Directories: map[string]int{}, Unverified: []string{}, UnverifiedEvents: map[string]string{}, Blocked: map[string]bool{}, Turns: map[string]int{}}
}

func (state *EditState) addUnverified(observation, note string) {
	state.Unverified = append(state.Unverified, note)
	if observation != "" {
		if state.UnverifiedEvents == nil {
			state.UnverifiedEvents = map[string]string{}
		}
		state.UnverifiedEvents[memory.Hash(memory.Packed([]any{observation, note}))] = note
	}
}
func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var v map[string]json.RawMessage
	e := json.Unmarshal(raw, &v)
	if e != nil || v == nil {
		return nil, errors.New("invalid edit state object")
	}
	return v, nil
}
func validRelative(path string) bool {
	return path != "" && !filepath.IsAbs(path) && !strings.ContainsAny(path, "\n\x00") && path != ".." && !strings.HasPrefix(filepath.Clean(path), "../")
}
func validDigest(raw json.RawMessage) bool {
	if string(raw) == "null" {
		return true
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(s)
}
func validateMetrics(raw json.RawMessage) error {
	v, e := object(raw)
	if e != nil {
		return e
	}
	var exists bool
	if json.Unmarshal(v["exists"], &exists) != nil || string(v["exists"]) == "null" {
		return errors.New("invalid file existence")
	}
	for _, key := range []string{"lines", "banners", "longs", "components"} {
		var n int
		if json.Unmarshal(v[key], &n) != nil || string(v[key]) == "null" || n < 0 {
			return errors.New("invalid file metric")
		}
	}
	return nil
}
func validateFiles(raw json.RawMessage, digestKey string) error {
	files, e := object(raw)
	if e != nil {
		return e
	}
	for path, raw := range files {
		if !validRelative(path) {
			return errors.New("invalid file path")
		}
		item, e := object(raw)
		if e != nil {
			return e
		}
		if e = validateMetrics(item["before"]); e != nil {
			return e
		}
		if !validDigest(item[digestKey]) {
			return errors.New("invalid file digest")
		}
	}
	return nil
}
func validateDirectories(raw json.RawMessage) error {
	directories, e := object(raw)
	if e != nil {
		return e
	}
	for path, raw := range directories {
		var count int
		if !validRelative(path) || json.Unmarshal(raw, &count) != nil || string(raw) == "null" || count < 0 {
			return errors.New("invalid directory count")
		}
	}
	return nil
}
func validateState(raw []byte, root string) error {
	v, e := object(raw)
	if e != nil {
		return e
	}
	var actual string
	if json.Unmarshal(v["root"], &actual) != nil || actual != root {
		return errors.New("invalid edit state root")
	}
	if e = validateFiles(v["files"], "after"); e != nil {
		return e
	}
	pending, e := object(v["pending"])
	if e != nil {
		return e
	}
	for _, raw := range pending {
		item, e := object(raw)
		if e != nil {
			return e
		}
		if e = validateFiles(item["files"], "digest"); e != nil {
			return e
		}
		var complete bool
		if json.Unmarshal(item["complete"], &complete) != nil || string(item["complete"]) == "null" {
			return errors.New("invalid pending call completeness")
		}
		if raw, ok := item["observation_id"]; ok {
			if !validDigest(raw) || string(raw) == "null" {
				return errors.New("invalid call observation")
			}
		}
		if raw, ok := item["moves"]; ok {
			moves, err := object(raw)
			if err != nil {
				return err
			}
			files, _ := object(item["files"])
			for destination, raw := range moves {
				var source string
				if json.Unmarshal(raw, &source) != nil || files[destination] == nil || files[source] == nil {
					return errors.New("invalid rename evidence")
				}
			}
		}
	}
	if raw, ok := v["directories"]; ok {
		if e = validateDirectories(raw); e != nil {
			return e
		}
	}
	if raw, ok := v["observed"]; ok {
		var observed bool
		if json.Unmarshal(raw, &observed) != nil || string(raw) == "null" {
			return errors.New("invalid collector observation")
		}
	}
	var notes []string
	if json.Unmarshal(v["unverified"], &notes) != nil || notes == nil {
		return errors.New("invalid edit state evidence")
	}
	if raw, ok := v["unverified_events"]; ok {
		events, err := object(raw)
		if err != nil {
			return err
		}
		stored := map[string]bool{}
		for _, note := range notes {
			stored[note] = true
		}
		for id, raw := range events {
			var note string
			if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) || json.Unmarshal(raw, &note) != nil || note == "" {
				return errors.New("invalid unverified event")
			}
			if !stored[note] {
				return errors.New("unverified event without evidence")
			}
		}
	}
	if cache, ok := v["check_cache"]; ok {
		m, e := object(cache)
		if e != nil {
			return e
		}
		var key, output string
		var code int
		if json.Unmarshal(m["key"], &key) != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) || json.Unmarshal(m["output"], &output) != nil || string(m["output"]) == "null" || json.Unmarshal(m["code"], &code) != nil || string(m["code"]) == "null" || !validCheck(output, code) {
			return errors.New("invalid check cache")
		}
	}
	if notice, ok := v["last_notice"]; ok {
		var key string
		if json.Unmarshal(notice, &key) != nil || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(key) {
			return errors.New("invalid notice evidence")
		}
	}
	return nil
}
func (c Collector) state() (EditState, error) {
	v := freshState(c.Root)
	raw, e := readRegularFile(c.path())
	if os.IsNotExist(e) {
		return v, nil
	}
	if e != nil {
		return v, e
	}
	if e = validateState(raw, c.Root); e != nil {
		return v, e
	}
	e = json.Unmarshal(raw, &v)
	if v.Files == nil || v.Pending == nil || v.Directories == nil || v.Blocked == nil || v.Turns == nil || v.Unverified == nil {
		return v, errors.New("invalid edit state")
	}
	return v, e
}
func editLock(path string) (func(), error) {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	f, e := openRegularFile(path, os.O_CREATE|os.O_RDWR)
	if e != nil {
		return nil, e
	}
	deadline := time.Now().Add(250 * time.Millisecond)
	for {
		e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			f.Close()
			return nil, e
		}
		if !time.Now().Before(deadline) {
			f.Close()
			return nil, memory.ErrBusy
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func (c Collector) failurePath() string { return strings.TrimSuffix(c.path(), ".json") + ".unverified" }
func (c Collector) recordFailure() {
	if os.MkdirAll(filepath.Dir(c.path()), 0700) != nil {
		return
	}
	f, e := openRegularFile(c.failurePath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND)
	if e == nil {
		_, _ = f.WriteString("편집 범위 잠금 대기 상한 초과\n")
		_ = f.Sync()
		_ = f.Close()
	}
}
