package memory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) Backup(directory string, config Object) (Object, error) {
	if _, e := os.Lstat(directory); !os.IsNotExist(e) {
		return nil, errors.New("backup destination exists")
	}
	unlock, e := Lock(filepath.Join(s.Root, ".write.lock"))
	if e != nil {
		return nil, e
	}
	defer unlock()
	if e = os.MkdirAll(filepath.Dir(directory), 0700); e != nil {
		return nil, e
	}
	if e = os.Mkdir(directory, 0700); e != nil {
		return nil, e
	}
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	dest := filepath.Join(directory, "memory.sqlite3")
	if _, e = db.Exec("VACUUM INTO ?", dest); e != nil {
		return nil, e
	}
	if e = os.Chmod(dest, 0600); e != nil {
		return nil, e
	}
	database, e := os.ReadFile(dest)
	if e != nil {
		return nil, e
	}
	queue := Object{}
	files, e := os.ReadDir(filepath.Join(s.Root, "pending"))
	if e != nil {
		return nil, e
	}
	for _, file := range files {
		ext := filepath.Ext(file.Name())
		if ext != ".json" && ext != ".invalid" {
			continue
		}
		path := filepath.Join(s.Root, "pending", file.Name())
		info, e := os.Lstat(path)
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("non-regular queue entry requires separate preservation; original retained")
		}
		raw, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		if e = PrivateWrite(filepath.Join(directory, "pending", file.Name()), raw); e != nil {
			return nil, e
		}
		queue[file.Name()] = Hash(raw)
	}
	raw := Packed(config)
	if e = PrivateWrite(filepath.Join(directory, "pilot.json"), raw); e != nil {
		return nil, e
	}
	manifest := Object{"project": s.Project, "created": Now(), "config_sha256": Hash(raw), "database_sha256": Hash(database), "pending": queue}
	if e = PrivateWrite(filepath.Join(directory, "manifest.json"), Packed(manifest)); e != nil {
		return nil, e
	}
	return Object{"backup": directory, "queued_records": len(queue)}, nil
}
func Restore(backup, root string) (Object, error) {
	raw, e := os.ReadFile(filepath.Join(backup, "manifest.json"))
	if e != nil {
		return nil, e
	}
	manifest, e := Decode(raw)
	if e != nil {
		return nil, e
	}
	project := Text(manifest["project"])
	if e = Identifier(project); e != nil {
		return nil, e
	}
	raw, e = os.ReadFile(filepath.Join(backup, "pilot.json"))
	if e != nil {
		return nil, e
	}
	if Hash(raw) != manifest["config_sha256"] {
		return nil, errors.New("backup config checksum mismatch")
	}
	config, e := Decode(raw)
	if e != nil {
		return nil, e
	}
	if config["project"] != project {
		return nil, errors.New("backup project mismatch")
	}
	database, e := os.ReadFile(filepath.Join(backup, "memory.sqlite3"))
	if e != nil {
		return nil, e
	}
	if Hash(database) != manifest["database_sha256"] {
		return nil, errors.New("backup database checksum mismatch")
	}
	pending := map[string][]byte{}
	entries, ok := manifest["pending"].(map[string]any)
	if !ok {
		return nil, errors.New("queue manifest missing")
	}
	for name, hash := range entries {
		if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") || (filepath.Ext(name) != ".json" && filepath.Ext(name) != ".invalid") {
			return nil, errors.New("unsafe backup entry")
		}
		path := filepath.Join(backup, "pending", name)
		info, e := os.Lstat(path)
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() {
			return nil, errors.New("non-regular backup queue entry")
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return nil, e
		}
		if Hash(b) != hash {
			return nil, errors.New("queue checksum mismatch")
		}
		pending[name] = b
	}
	probe := &Store{Path: filepath.Join(backup, "memory.sqlite3")}
	db, e := probe.Connect(true)
	if e != nil {
		return nil, e
	}
	var integrity string
	e = db.QueryRow("PRAGMA integrity_check").Scan(&integrity)
	db.Close()
	if e != nil || integrity != "ok" {
		return nil, errors.New("backup integrity failure")
	}
	resolved, e := Resolve(root)
	if e != nil {
		return nil, e
	}
	target := filepath.Join(resolved, project)
	if e = os.MkdirAll(resolved, 0700); e != nil {
		return nil, e
	}
	if e = os.Mkdir(target, 0700); e != nil {
		return nil, e
	}
	if e = PrivateWrite(filepath.Join(target, "memory.sqlite3"), database); e != nil {
		return nil, e
	}
	s, e := Open(resolved, project, true)
	if e != nil {
		return nil, e
	}
	db, e = s.Connect(false)
	if e != nil {
		return nil, e
	}
	_, e = db.Exec("DELETE FROM bindings")
	db.Close()
	if e != nil {
		return nil, e
	}
	for name, b := range pending {
		if e = PrivateWrite(filepath.Join(target, "pending", name), b); e != nil {
			return nil, e
		}
	}
	config["data_root"] = resolved
	config["project_root"] = target
	config["source_roots"] = []string{target}
	delete(config, "project_aliases")
	delete(config, "exact_cwds")
	path := filepath.Join(target, "restore-config.json")
	if e = PrivateWrite(path, Packed(config)); e != nil {
		return nil, e
	}
	return Object{"restored": target, "config": path, "bindings": "cleared; select the task again"}, nil
}
