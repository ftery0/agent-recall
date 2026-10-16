package memory

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

func (s *Store) bindingPath(tool, session string) string {
	return filepath.Join(s.Root, "bindings", Hash(Packed([]any{tool, session}))+".json")
}

func ValidTool(tool string) bool {
	return contains([]string{"codex", "claude", "opencode", "kiro", "cursor", "gemini", "antigravity"}, tool)
}

func (s *Store) Bind(tool, session, task, cwd string) error {
	if !ValidTool(tool) {
		return errors.New("unsupported tool")
	}
	for _, v := range []string{session, task} {
		if e := Identifier(v); e != nil {
			return e
		}
	}
	path, e := Resolve(cwd)
	if e != nil {
		return e
	}
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		return e
	}
	defer unlock()
	if e = PrivateWrite(s.bindingPath(tool, session), Packed(Object{"active": false})); e != nil {
		return e
	}
	db, e := s.Connect(false)
	if e != nil {
		return e
	}
	defer db.Close()
	if _, e = db.Exec("INSERT INTO bindings VALUES(?,?,?,?) ON CONFLICT(tool,session) DO UPDATE SET task=excluded.task,cwd=excluded.cwd", tool, session, task, path); e != nil {
		return e
	}
	return PrivateWrite(s.bindingPath(tool, session), Packed(Object{"active": true, "tool": tool, "session": session, "task": task, "cwd": path}))
}
func (s *Store) Binding(tool, session string) (Object, error) {
	raw, e := os.ReadFile(s.bindingPath(tool, session))
	if e == nil {
		v, e := Decode(raw)
		if e != nil {
			return nil, e
		}
		if v["active"] != true {
			return nil, nil
		}
		return v, nil
	}
	if !os.IsNotExist(e) {
		return nil, e
	}
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	var task, cwd string
	e = db.QueryRow("SELECT task,cwd FROM bindings WHERE tool=? AND session=?", tool, session).Scan(&task, &cwd)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return Object{"tool": tool, "session": session, "task": task, "cwd": cwd}, nil
}
func (s *Store) Unbind(tool, session string) error {
	if !ValidTool(tool) {
		return errors.New("unsupported tool")
	}
	if e := Identifier(session); e != nil {
		return e
	}
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		return e
	}
	defer unlock()
	if e = PrivateWrite(s.bindingPath(tool, session), Packed(Object{"active": false, "selected": "none", "tool": tool, "session": session})); e != nil {
		return e
	}
	db, e := s.Connect(false)
	if e != nil {
		return e
	}
	defer db.Close()
	_, e = db.Exec("DELETE FROM bindings WHERE tool=? AND session=?", tool, session)
	return e
}

// An explicit opt-out differs from an unfinished/cleared binding. It lasts for
// this tool and session only; Bind or an explicit new selection can replace it.
func (s *Store) OptedOut(tool, session string) (bool, error) {
	raw, e := os.ReadFile(s.bindingPath(tool, session))
	if os.IsNotExist(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	v, e := Decode(raw)
	if e != nil {
		return false, e
	}
	return v["active"] == false && v["selected"] == "none" && v["tool"] == tool && v["session"] == session, nil
}
