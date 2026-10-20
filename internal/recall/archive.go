// Package recall keeps the raw conversation history and builds searchable records from it.
package recall

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/adapters"
	"github.com/ftery0/agent-recall/internal/memory"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// RawDir holds a private copy of every Claude and Codex transcript. Claude deletes its own
// transcripts after cleanupPeriodDays, so this copy is the only lasting source.
func RawDir() string { return filepath.Join(adapters.Home(), "raw") }

var (
	rollout   = regexp.MustCompile(`^rollout-(\d{4}-\d\d-\d\d)T`)
	timestamp = regexp.MustCompile(`"timestamp":"([^"]+)"`)
)

func sourcesWithErrors() (map[string][]string, []string) {
	out := map[string][]string{}
	var failed []string
	home, err := os.UserHomeDir()
	if err != nil {
		return out, []string{err.Error()}
	}
	add := func(tool, root string) {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path != root || !errors.Is(err, os.ErrNotExist) {
					failed = append(failed, path+": "+err.Error())
				}
				return nil
			}
			if d.Type().IsRegular() && strings.HasSuffix(path, ".jsonl") {
				out[tool] = append(out[tool], path)
			}
			return nil
		})
	}
	addHistory := func(tool, path string) {
		info, err := os.Stat(path)
		if err == nil && info.Mode().IsRegular() {
			out[tool] = append(out[tool], path)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			failed = append(failed, path+": "+err.Error())
		}
	}
	profiles := filepath.Join(home, ".claude-profiles")
	entries, err := os.ReadDir(profiles)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		failed = append(failed, profiles+": "+err.Error())
	}
	for _, entry := range entries {
		profile := filepath.Join(profiles, entry.Name())
		info, err := os.Stat(profile)
		if err != nil {
			failed = append(failed, profile+": "+err.Error())
			continue
		}
		if info.IsDir() {
			add("claude", filepath.Join(profile, "projects"))
			addHistory("claude", filepath.Join(profile, "history.jsonl"))
		}
	}
	add("claude", filepath.Join(home, ".claude", "projects"))
	addHistory("claude", filepath.Join(home, ".claude", "history.jsonl"))
	add("codex", filepath.Join(home, ".codex", "sessions"))
	add("codex", filepath.Join(home, ".codex", "archived_sessions"))
	addHistory("codex", filepath.Join(home, ".codex", "history.jsonl"))
	return out, failed
}

// Dest is where a transcript is kept: raw/<tool>/<local date the session started>/<path in the project>.
// The date comes from content that never changes, so every later copy lands on the same file.
func Dest(raw, tool, path string) string {
	name := filepath.Base(path)
	if name == "history.jsonl" {
		profile := filepath.Base(filepath.Dir(path))
		if profile == ".claude" || profile == ".codex" {
			profile = "default"
		}
		return filepath.Join(raw, tool, "history", profile+".jsonl")
	}
	if tool == "codex" {
		if m := rollout.FindStringSubmatch(name); m != nil {
			return filepath.Join(raw, tool, m[1], name)
		}
		return filepath.Join(raw, tool, "undated", name)
	}
	i := strings.LastIndex(path, "/projects/")
	parts := strings.SplitN(path[i+1:], "/", 3)[1:]
	if i < 0 || len(parts) != 2 {
		return filepath.Join(raw, tool, "undated", name)
	}
	session := strings.TrimSuffix(strings.SplitN(parts[1], "/", 2)[0], ".jsonl")
	parent := filepath.Join(path[:len(path)-len(parts[1])], session+".jsonl")
	return filepath.Join(raw, tool, startDate(parent), filepath.FromSlash(parts[1]))
}

// startDate is the local date of the first timestamp in the first 1MB, or "undated".
func startDate(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return "undated"
	}
	defer f.Close()
	r := bufio.NewReader(io.LimitReader(f, 1<<20))
	for {
		line, err := r.ReadSlice('\n')
		if m := timestamp.FindSubmatch(line); m != nil {
			if at, e := time.Parse(time.RFC3339Nano, string(m[1])); e == nil {
				return at.Local().Format("2006-01-02")
			}
		}
		for err == bufio.ErrBufferFull {
			_, err = r.ReadSlice('\n')
		}
		if err != nil {
			return "undated"
		}
	}
}

var (
	ErrArchiveBusy    = errors.New("archive is busy; retry required")
	ErrSourceChanging = errors.New("source changed during capture; retry required")
)

// A checkpoint permits cheap, repeated Stop hooks. Append-only client transcripts need only
// their new tail copied; Sweep still compares the entire stored prefix, including its middle.
// These stat identities are a cache, never a substitute for the full reconciliation in Sweep.
type archiveStamp struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Size   int64  `json:"size"`
	Mtime  int64  `json:"mtime_ns"`
}

type archiveCheckpoint struct {
	Source string       `json:"source"`
	From   archiveStamp `json:"from"`
	Copy   archiveStamp `json:"copy"`
}

func archiveStat(info os.FileInfo) archiveStamp {
	s := archiveStamp{Size: info.Size(), Mtime: info.ModTime().UnixNano()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		s.Device, s.Inode = uint64(st.Dev), uint64(st.Ino)
	}
	return s
}

func (s archiveStamp) sameFile(t archiveStamp) bool {
	return s.Inode != 0 && s.Device == t.Device && s.Inode == t.Inode
}

func checkpoint(dest string) archiveCheckpoint {
	var c archiveCheckpoint
	if b, err := os.ReadFile(dest + ".checkpoint.json"); err == nil {
		if json.Unmarshal(b, &c) != nil {
			return archiveCheckpoint{}
		}
	}
	return c
}

func saveCheckpoint(src, dest string, from archiveStamp) error {
	info, err := os.Stat(dest)
	if err != nil {
		return err
	}
	c := archiveCheckpoint{Source: src, From: from, Copy: archiveStat(info)}
	if checkpoint(dest) == c {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return memory.PrivateWrite(dest+".checkpoint.json", b)
}

// Archive captures exactly the size observed when the source is opened. Later appends are
// collected on the next call. Rewrites are staged and the previous copy is kept before an
// atomic replacement. A stable sidecar lock also protects replacement of the destination.
func Archive(src, dest string) (int64, error) { return archive(src, dest, false) }

func archive(src, dest string, reconcile bool) (n int64, err error) {
	lock, err := exclusive(dest + ".lock")
	if err != nil {
		return 0, err
	}
	if lock == nil {
		return 0, ErrArchiveBusy
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	for attempt := 0; attempt < 2; attempt++ {
		n, err = archiveSnapshot(src, dest, reconcile)
		if !errors.Is(err, ErrSourceChanging) {
			break
		}
	}
	return n, err
}

func archiveSnapshot(src, dest string, reconcile bool) (n int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	info, err := in.Stat()
	if err != nil {
		return 0, err
	}
	from := archiveStat(info)
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("source is not a regular file: %s", src)
	}
	held, err := os.Stat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return replaceSnapshot(in, src, dest, from, false)
	}
	if err != nil {
		return 0, err
	}
	c := checkpoint(dest)
	known := c.Source == src && c.Copy == archiveStat(held) && c.From.sameFile(from)
	if !reconcile && known && c.From == from {
		return 0, nil
	}
	out, err := os.OpenFile(dest, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	have := held.Size()
	// Only a known append gets the bounded Stop-hook path. Changes with the same size,
	// unknown copies and every background sweep receive a complete prefix comparison.
	appendOnly := !reconcile && known && c.From.Size == have && from.Size > have
	prefixOK := have <= from.Size && ((!appendOnly && samePrefix(in, out, have)) || (appendOnly && sameEdges(in, out, have)))
	closeErr := out.Close()
	if closeErr != nil {
		return 0, closeErr
	}
	if !prefixOK {
		return replaceSnapshot(in, src, dest, from, true)
	}
	if have == from.Size {
		if err = sourceUnchanged(in, src, from); err != nil {
			return 0, err
		}
		return 0, saveCheckpoint(src, dest, from)
	}
	n, err = appendSnapshot(in, src, dest, from, have)
	if err == nil {
		err = saveCheckpoint(src, dest, from)
	}
	return n, err
}

// The opened inode and path must still identify the captured generation. Growth is safe for
// append-only producers; rotation, truncation and a fixed-size rewrite require another capture.
func sourceUnchanged(in *os.File, src string, before archiveStamp) error {
	a, err := in.Stat()
	if err != nil {
		return err
	}
	b, err := os.Stat(src)
	if err != nil {
		return errors.Join(ErrSourceChanging, err)
	}
	after := archiveStat(a)
	if !before.sameFile(archiveStat(b)) || after.Size < before.Size || (after.Size == before.Size && after.Mtime != before.Mtime) {
		return ErrSourceChanging
	}
	return nil
}

type syncCloser interface {
	io.Writer
	Sync() error
	Close() error
}

// Exact-length copying and both durability errors are shared by real files and fault fixtures.
func copySnapshot(out syncCloser, in io.Reader, size int64) (int64, error) {
	n, err := io.CopyN(out, in, size)
	if err == nil {
		err = out.Sync()
	}
	return n, errors.Join(err, out.Close())
}

func replaceSnapshot(in *os.File, src, dest string, from archiveStamp, preserve bool) (int64, error) {
	stage, err := os.CreateTemp(filepath.Dir(dest), ".archive-")
	if err != nil {
		return 0, err
	}
	name := stage.Name()
	defer os.Remove(name)
	n, err := copySnapshot(stage, io.NewSectionReader(in, 0, from.Size), from.Size)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			err = errors.Join(ErrSourceChanging, err)
		}
		return 0, err
	}
	if err = sourceUnchanged(in, src, from); err != nil {
		return 0, err
	}
	// A producer may change a prefix while appending. Do not publish a mixed staged copy.
	x, err := readerHash(in, 0, from.Size)
	if err != nil {
		return 0, errors.Join(ErrSourceChanging, err)
	}
	y, err := prefixHash(name, from.Size)
	if err != nil {
		return 0, err
	}
	if x != y {
		return 0, ErrSourceChanging
	}
	if err = sourceUnchanged(in, src, from); err != nil {
		return 0, err
	}
	if preserve {
		previous := fmt.Sprintf("%s.prev-%d", dest, time.Now().UnixNano())
		if err = os.Link(dest, previous); err != nil {
			return 0, err
		}
		if err = syncDirectory(filepath.Dir(dest)); err != nil {
			return 0, err
		}
	}
	if err = os.Rename(name, dest); err != nil {
		return 0, err
	}
	if err = syncDirectory(filepath.Dir(dest)); err != nil {
		return n, err
	}
	return n, saveCheckpoint(src, dest, from)
}

func appendSnapshot(in *os.File, src, dest string, from archiveStamp, have int64) (int64, error) {
	out, err := os.OpenFile(dest, os.O_WRONLY, 0)
	if err != nil {
		return 0, err
	}
	if _, err = out.Seek(have, io.SeekStart); err != nil {
		return 0, errors.Join(err, out.Close())
	}
	n, err := copySnapshot(out, io.NewSectionReader(in, have, from.Size-have), from.Size-have)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = errors.Join(ErrSourceChanging, err)
	}
	if err == nil {
		err = sourceUnchanged(in, src, from)
	}
	if err == nil {
		x, a := readerHash(in, have, from.Size-have)
		y, b := sectionHash(dest, have, from.Size-have)
		err = errors.Join(a, b)
		if err == nil && x != y {
			err = ErrSourceChanging
		}
	}
	if err == nil {
		err = sourceUnchanged(in, src, from)
	}
	if err != nil {
		// The old prefix was never overwritten. Remove a failed/unstable tail before retry.
		rollback, e := os.OpenFile(dest, os.O_WRONLY, 0)
		if e == nil {
			e = rollback.Truncate(have)
			if e == nil {
				e = rollback.Sync()
			}
			e = errors.Join(e, rollback.Close())
		}
		return 0, errors.Join(err, e)
	}
	return n, nil
}

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// samePrefix compares every byte, rather than only the first and last windows.
func samePrefix(src, dest io.ReaderAt, n int64) bool {
	a, errA := readerHash(src, 0, n)
	b, errB := readerHash(dest, 0, n)
	return errA == nil && errB == nil && a == b
}

func sameEdges(src, dest io.ReaderAt, n int64) bool {
	const window = 64 << 10
	return samePrefix(src, dest, min(n, window)) && samePrefix(io.NewSectionReader(src, max(0, n-window), min(n, window)), io.NewSectionReader(dest, max(0, n-window), min(n, window)), min(n, window))
}

// Sweep copies whatever every source has beyond its copy, so sessions that ended without a
// Stop hook are caught at the next start. Only one sweep runs at a time.
func Sweep() (map[string]any, error) {
	raw := RawDir()
	if err := os.MkdirAll(raw, 0o700); err != nil {
		return nil, err
	}
	lock, err := exclusive(filepath.Join(raw, ".sweep.lock"))
	if lock == nil {
		return map[string]any{"skipped": "another sweep is running"}, err
	}
	defer lock.Close()
	files, copied := 0, int64(0)
	sources, failed := sourcesWithErrors()
	for tool, paths := range sources {
		for _, path := range paths {
			files++
			dest := Dest(raw, tool, path)
			// Reconciliation is deliberately stronger than the incremental Stop hook:
			// equal sizes and equal stat-cache identities do not establish equal contents.
			n, err := archive(path, dest, true)
			copied += n
			if err != nil {
				failed = append(failed, path+": "+err.Error())
			}
		}
	}
	return map[string]any{"files": files, "copied_bytes": copied, "failed": failed}, nil
}

// exclusive takes a lock file without waiting; nil means someone else holds it.
func exclusive(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// RunInBackground runs this program with args in a detached process, so the hook never waits.
func RunInBackground(args ...string) {
	exe, err := os.Executable()
	if err != nil || os.Getenv("HARNESS_RECALL") == "1" {
		return
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}

// Verify distinguishes a fully captured snapshot from missing bytes, concurrent changes and
// differing content. Matching only a shorter prefix is never reported as a complete copy.
func Verify() (map[string]any, error) {
	raw := RawDir()
	files, compared := 0, int64(0)
	var bad, incomplete, changing []string
	sources, failed := sourcesWithErrors()
	for tool, paths := range sources {
		for _, path := range paths {
			files++
			dest := Dest(raw, tool, path)
			lock, err := exclusive(dest + ".lock")
			if err != nil {
				failed = append(failed, path+": "+err.Error())
				continue
			}
			if lock == nil {
				changing = append(changing, path+": capture is running")
				continue
			}
			status, n, err := verifySnapshot(path, dest)
			err = errors.Join(err, lock.Close())
			compared += n
			if err != nil {
				failed = append(failed, path+": "+err.Error())
				continue
			}
			switch status {
			case "mismatched":
				bad = append(bad, path+": content differs")
			case "incomplete":
				incomplete = append(incomplete, path+": bytes not yet captured")
			case "changing":
				changing = append(changing, path+": source is changing")
			}
		}
	}
	return map[string]any{"files": files, "compared_bytes": compared, "mismatched": bad, "incomplete": incomplete, "changing": changing, "failed": failed, "complete": len(bad)+len(incomplete)+len(changing)+len(failed) == 0}, nil
}

func verifySnapshot(src, dest string) (status string, compared int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer func() { err = errors.Join(err, in.Close()) }()
	a, err := in.Stat()
	if err != nil {
		return "", 0, err
	}
	out, err := os.Open(dest)
	if errors.Is(err, os.ErrNotExist) {
		return "incomplete", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	defer func() { err = errors.Join(err, out.Close()) }()
	b, err := out.Stat()
	if err != nil {
		return "", 0, err
	}
	n := min(a.Size(), b.Size())
	x, errA := readerHash(in, 0, n)
	y, errB := readerHash(out, 0, n)
	// A changing generation is pending work, not evidence of damaged stored bytes.
	after, e := in.Stat()
	pathAfter, p := os.Stat(src)
	copyAfter, c := out.Stat()
	copyPathAfter, d := os.Stat(dest)
	if e != nil || p != nil || c != nil || d != nil {
		return "changing", n, nil
	}
	if archiveStat(a) != archiveStat(after) || !archiveStat(a).sameFile(archiveStat(pathAfter)) || archiveStat(b) != archiveStat(copyAfter) || !archiveStat(b).sameFile(archiveStat(copyPathAfter)) {
		return "changing", n, nil
	}
	if errA != nil || errB != nil {
		return "", n, errors.Join(errA, errB)
	}
	if x != y || b.Size() > a.Size() {
		return "mismatched", n, nil
	}
	if b.Size() < a.Size() {
		return "incomplete", n, nil
	}
	return "complete", n, nil
}

func readerHash(r io.ReaderAt, offset, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.CopyN(h, io.NewSectionReader(r, offset, n), n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sectionHash(path string, offset, n int64) (result string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return readerHash(f, offset, n)
}

func prefixHash(path string, n int64) (string, error) { return sectionHash(path, 0, n) }
