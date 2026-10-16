package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

type Entry struct {
	Source     string  `json:"source"`
	Target     string  `json:"target"`
	Mode       uint32  `json:"mode"`
	Before     *string `json:"before"`
	After      string  `json:"after"`
	BeforeMode uint32  `json:"before_mode"`
}
type Manifest struct {
	ID      string  `json:"id"`
	Created string  `json:"created"`
	Status  string  `json:"status"`
	Files   []Entry `json:"files"`
}

func readRegular(path string) ([]byte, os.FileInfo, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, nil, e
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("not a regular file: %s", path)
	}
	raw, e := io.ReadAll(f)
	return raw, info, e
}
func inspect(path string) (*string, uint32, error) {
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return nil, 0, nil
	}
	if e != nil {
		return nil, 0, e
	}
	if !info.Mode().IsRegular() {
		return nil, 0, errors.New("target must be a regular file, not a link/directory")
	}
	raw, info, e := readRegular(path)
	if e != nil {
		return nil, 0, e
	}
	hash := memory.Hash(raw)
	return &hash, uint32(info.Mode().Perm()), nil
}
func same(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
func Plan(entries []Entry) (Manifest, error) {
	m := Manifest{ID: memory.ID(), Created: memory.Now(), Status: "planned", Files: entries}
	seen := map[string]bool{}
	for i := range m.Files {
		f := &m.Files[i]
		if !filepath.IsAbs(f.Source) || !filepath.IsAbs(f.Target) || filepath.Clean(f.Target) != f.Target || seen[f.Target] {
			return m, errors.New("absolute unique source/target paths required")
		}
		seen[f.Target] = true
		raw, _, e := readRegular(f.Source)
		if e != nil {
			return m, e
		}
		f.After = memory.Hash(raw)
		f.Before, f.BeforeMode, e = inspect(f.Target)
		if e != nil {
			return m, e
		}
		if f.Mode == 0 {
			f.Mode = 0600
			if f.Before != nil {
				f.Mode = f.BeforeMode
			}
		}
		if f.Mode > 0777 {
			return m, errors.New("invalid mode")
		}
	}
	return m, nil
}
func writeFile(path string, raw []byte, mode uint32) error {
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".install-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(os.FileMode(mode)); e == nil {
		_, e = f.Write(raw)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func Apply(home string, m Manifest) (Manifest, error) {
	if e := memory.Identifier(m.ID); e != nil {
		return m, e
	}
	if m.Status != "planned" {
		return m, errors.New("only a reviewed plan can apply")
	}
	unlock, e := memory.Lock(filepath.Join(home, "installs", ".lock"))
	if e != nil {
		return m, e
	}
	defer unlock()
	directory := filepath.Join(home, "installs", m.ID)
	if e = os.Mkdir(directory, 0700); e != nil {
		return m, e
	}
	payloads := make([][]byte, len(m.Files))
	seen := map[string]bool{}
	for i, f := range m.Files {
		if !filepath.IsAbs(f.Target) || !filepath.IsAbs(f.Source) || seen[f.Target] || f.Mode > 0777 {
			return m, errors.New("invalid manifest")
		}
		seen[f.Target] = true
		current, mode, e := inspect(f.Target)
		if e != nil {
			return m, e
		}
		if !same(current, f.Before) || mode != f.BeforeMode {
			return m, fmt.Errorf("concurrent change: %s", f.Target)
		}
		raw, _, e := readRegular(f.Source)
		if e != nil {
			return m, e
		}
		if memory.Hash(raw) != f.After {
			return m, errors.New("source changed since plan")
		}
		payloads[i] = raw
		if current != nil {
			old, _, e := readRegular(f.Target)
			if e != nil {
				return m, e
			}
			if e = memory.PrivateWrite(filepath.Join(directory, fmt.Sprintf("%d.before", i)), old); e != nil {
				return m, e
			}
		}
	}
	m.Status = "applying"
	if e = memory.PrivateWrite(filepath.Join(directory, "manifest.json"), memory.Packed(m)); e != nil {
		return m, e
	}
	for i, f := range m.Files {
		current, mode, e := inspect(f.Target)
		if e != nil || !same(current, f.Before) || mode != f.BeforeMode {
			m.Status = "partial"
			_ = memory.PrivateWrite(filepath.Join(directory, "manifest.json"), memory.Packed(m))
			return m, fmt.Errorf("concurrent change during install: %s", f.Target)
		}
		if e = writeFile(f.Target, payloads[i], f.Mode); e != nil {
			m.Status = "partial"
			_ = memory.PrivateWrite(filepath.Join(directory, "manifest.json"), memory.Packed(m))
			return m, e
		}
	}
	m.Status = "applied"
	return m, memory.PrivateWrite(filepath.Join(directory, "manifest.json"), memory.Packed(m))
}
func Read(path string) (Manifest, error) {
	var m Manifest
	raw, _, e := readRegular(path)
	if e == nil {
		e = json.Unmarshal(raw, &m)
	}
	return m, e
}
func Doctor(m Manifest) (memory.Object, error) {
	mismatches := []string{}
	for _, f := range m.Files {
		hash, mode, e := inspect(f.Target)
		if e != nil || hash == nil || *hash != f.After || mode != f.Mode {
			mismatches = append(mismatches, f.Target)
		}
	}
	return memory.Object{"install": m.ID, "status": m.Status, "files": len(m.Files), "mismatches": mismatches, "matches": len(mismatches) == 0, "hook_trust": "native client review required; not inferred from file hashes"}, nil
}
func Rollback(home, id string) (Manifest, error) {
	var m Manifest
	if e := memory.Identifier(id); e != nil {
		return m, e
	}
	unlock, e := memory.Lock(filepath.Join(home, "installs", ".lock"))
	if e != nil {
		return m, e
	}
	defer unlock()
	directory := filepath.Join(home, "installs", id)
	m, e = Read(filepath.Join(directory, "manifest.json"))
	if e != nil {
		return m, e
	}
	if m.ID != id || m.Status == "rolled-back" {
		return m, errors.New("invalid rollback state")
	}
	restore := make([]bool, len(m.Files))
	originals := make([][]byte, len(m.Files))
	for i, f := range m.Files {
		current, mode, e := inspect(f.Target)
		if e != nil {
			return m, e
		}
		if same(current, f.Before) && mode == f.BeforeMode {
			continue
		}
		if current == nil || *current != f.After || mode != f.Mode {
			return m, fmt.Errorf("user changes preserved; rollback refused: %s", f.Target)
		}
		restore[i] = true
		if f.Before != nil {
			raw, _, e := readRegular(filepath.Join(directory, fmt.Sprintf("%d.before", i)))
			if e != nil {
				return m, e
			}
			if memory.Hash(raw) != *f.Before {
				return m, errors.New("backup checksum mismatch")
			}
			originals[i] = raw
		}
	}
	for i := len(m.Files) - 1; i >= 0; i-- {
		if !restore[i] {
			continue
		}
		f := m.Files[i]
		current, mode, e := inspect(f.Target)
		if e != nil || current == nil || *current != f.After || mode != f.Mode {
			return m, errors.New("concurrent change during rollback")
		}
		if f.Before == nil {
			e = os.Remove(f.Target)
		} else {
			e = writeFile(f.Target, originals[i], f.BeforeMode)
		}
		if e != nil {
			return m, e
		}
	}
	m.Status = "rolled-back"
	return m, memory.PrivateWrite(filepath.Join(directory, "manifest.json"), memory.Packed(m))
}
