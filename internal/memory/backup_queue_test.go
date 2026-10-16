package memory

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBackupRejectsNonregularQuarantine(t *testing.T) {
	for _, kind := range []string{"directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			s := fixture(t)
			entry := filepath.Join(s.Root, "pending", "bad.invalid")
			target := filepath.Join(t.TempDir(), "outside")
			if e := os.WriteFile(target, []byte("outside bytes"), 0600); e != nil {
				t.Fatal(e)
			}
			var e error
			if kind == "directory" {
				e = os.Mkdir(entry, 0700)
			} else {
				e = os.Symlink(target, entry)
			}
			if e != nil {
				t.Fatal(e)
			}
			backup := filepath.Join(t.TempDir(), "backup")
			if _, e = s.Backup(backup, Object{"project": s.Project}); e == nil {
				t.Fatal("nonregular quarantine accepted")
			}
			if _, e = os.Lstat(entry); e != nil {
				t.Fatal("original quarantine lost", e)
			}
			if _, e = os.Lstat(filepath.Join(backup, "pending", "bad.invalid")); !os.IsNotExist(e) {
				t.Fatal("outside bytes copied into backup", e)
			}
			if got, e := os.ReadFile(target); e != nil || string(got) != "outside bytes" {
				t.Fatal("outside target changed", e)
			}
		})
	}
}

func TestRestoreRejectsSymlinkQueueBeforeCreatingDestination(t *testing.T) {
	s := fixture(t)
	bytes := []byte("original invalid bytes")
	if e := PrivateWrite(filepath.Join(s.Root, "pending", "bad.invalid"), bytes); e != nil {
		t.Fatal(e)
	}
	backup := filepath.Join(t.TempDir(), "backup")
	if _, e := s.Backup(backup, Object{"project": s.Project}); e != nil {
		t.Fatal(e)
	}
	entry := filepath.Join(backup, "pending", "bad.invalid")
	outside := filepath.Join(t.TempDir(), "outside")
	if e := os.WriteFile(outside, bytes, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(entry); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, entry); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(t.TempDir(), "restored")
	if _, e := Restore(backup, root); e == nil {
		t.Fatal("queue symlink accepted despite matching target bytes")
	}
	if _, e := os.Lstat(root); !os.IsNotExist(e) {
		t.Fatal("restore destination created before queue validation", e)
	}
}
