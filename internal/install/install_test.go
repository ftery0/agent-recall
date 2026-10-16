package install

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestApplyRollbackAndDrift(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	os.WriteFile(src, []byte("new"), 0600)
	os.WriteFile(target, []byte("old"), 0644)
	m, e := Plan([]Entry{{Source: src, Target: target, Mode: 0644}})
	if e != nil {
		t.Fatal(e)
	}
	home := filepath.Join(root, "home")
	m, e = Apply(home, m)
	if e != nil {
		t.Fatal(e)
	}
	v, _ := Doctor(m)
	if v["matches"] != true {
		t.Fatal(v)
	}
	os.WriteFile(target, []byte("user change"), 0644)
	if _, e = Rollback(home, m.ID); e == nil {
		t.Fatal("overwrote user change")
	}
	os.WriteFile(target, []byte("new"), 0644)
	if _, e = Rollback(home, m.ID); e != nil {
		t.Fatal(e)
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "old" {
		t.Fatal(string(raw))
	}
}
func TestPlanConcurrentChangeAndUnlisted(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	untouched := filepath.Join(root, "untouched")
	os.WriteFile(src, []byte("new"), 0600)
	os.WriteFile(dst, []byte("old"), 0600)
	os.WriteFile(untouched, []byte("keep"), 0600)
	m, e := Plan([]Entry{{Source: src, Target: dst}})
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(dst, []byte("user"), 0600)
	if _, e = Apply(filepath.Join(root, "home"), m); e == nil {
		t.Fatal("stale plan applied")
	}
	raw, _ := os.ReadFile(untouched)
	if string(raw) != "keep" {
		t.Fatal("unlisted changed")
	}
}
func TestNewFileRollback(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	os.WriteFile(src, []byte("new"), 0600)
	m, _ := Plan([]Entry{{Source: src, Target: dst}})
	home := filepath.Join(root, "home")
	m, e := Apply(home, m)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Rollback(home, m.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(dst); !os.IsNotExist(e) {
		t.Fatal("new install file remained")
	}
}
func TestRejectDuplicateAndSymlink(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	dst := filepath.Join(root, "dst")
	os.WriteFile(src, []byte("new"), 0600)
	if _, e := Plan([]Entry{{Source: src, Target: dst}, {Source: src, Target: dst}}); e == nil {
		t.Fatal("duplicate accepted")
	}
	os.Symlink(src, dst)
	if _, e := Plan([]Entry{{Source: src, Target: dst}}); e == nil {
		t.Fatal("symlink accepted")
	}
}

func TestRejectNonRegularInstallInput(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	target := filepath.Join(root, "target")
	if e := os.WriteFile(source, []byte("new"), 0600); e != nil {
		t.Fatal(e)
	}
	m, e := Plan([]Entry{{Source: source, Target: target}})
	if e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(source); e != nil {
		t.Fatal(e)
	}
	if e := syscall.Mkfifo(source, 0600); e != nil {
		t.Fatal(e)
	}
	for name, check := range map[string]func() error{
		"plan":     func() error { _, e := Plan([]Entry{{Source: source, Target: target}}); return e },
		"apply":    func() error { _, e := Apply(filepath.Join(root, "home"), m); return e },
		"manifest": func() error { _, e := Read(source); return e },
	} {
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- check() }()
			select {
			case e := <-done:
				if e == nil {
					t.Fatal("nonregular input accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("nonregular input blocked install")
			}
		})
	}
	if _, e := os.Lstat(target); !os.IsNotExist(e) {
		t.Fatal("target changed on failed input")
	}
}
