package recall

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func same(t *testing.T, a, b string) {
	t.Helper()
	x, _ := os.ReadFile(a)
	y, err := os.ReadFile(b)
	if err != nil || string(x) != string(y) {
		t.Fatalf("%s != %s (%v)", a, b, err)
	}
}

func fixture(t *testing.T) (home, raw string) {
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_HOME", filepath.Join(home, "harness"))
	t.Setenv("TZ", "Asia/Seoul")
	return home, filepath.Join(home, "harness", "raw")
}

func TestSweepCopiesEveryTranscriptByStartDate(t *testing.T) {
	home, raw := fixture(t)
	project := filepath.Join(home, ".claude-profiles", "work", "projects", "-Users-me-repo")
	session := filepath.Join(project, "abc.jsonl")
	agent := filepath.Join(project, "abc", "subagents", "agent-1.jsonl")
	rollout := filepath.Join(home, ".codex", "sessions", "2026", "10", "06", "rollout-2026-10-06T23-59-00-x.jsonl")
	history := filepath.Join(home, ".codex", "history.jsonl")
	write(t, session, `{"type":"last-prompt"}`+"\n"+`{"timestamp":"2026-10-06T16:00:00.000Z"}`+"\n")
	write(t, agent, `{"timestamp":"2026-10-07T03:00:00.000Z"}`+"\n")
	write(t, rollout, "codex\n")
	write(t, history, "h\n")
	out, err := Sweep()
	if err != nil || out["files"] != 4 || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	// 16:00 UTC is the next day in Seoul; the subagent follows its session's date.
	same(t, session, filepath.Join(raw, "claude", "2026-10-07", "abc.jsonl"))
	same(t, agent, filepath.Join(raw, "claude", "2026-10-07", "abc", "subagents", "agent-1.jsonl"))
	same(t, rollout, filepath.Join(raw, "codex", "2026-10-06", filepath.Base(rollout)))
	same(t, history, filepath.Join(raw, "codex", "history", "default.jsonl"))
	if info, _ := os.Stat(raw); info.Mode().Perm() != 0o700 {
		t.Fatal(info.Mode())
	}
	if out, _ = Sweep(); out["copied_bytes"] != int64(0) {
		t.Fatal("second sweep copied again", out)
	}
}

func TestArchiveAppendsAndKeepsRewrittenCopy(t *testing.T) {
	home, raw := fixture(t)
	src := filepath.Join(home, ".claude", "projects", "p", "s.jsonl")
	write(t, src, `{"timestamp":"2026-10-07T01:00:00Z"}`+"\n")
	dest := Dest(raw, "claude", src)
	if n, err := Archive(src, dest); err != nil || n == 0 {
		t.Fatal(n, err)
	}
	f, _ := os.OpenFile(src, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("more\n")
	f.Close()
	if n, err := Archive(src, dest); err != nil || n != 5 {
		t.Fatal("append copied", n, err)
	}
	same(t, src, dest)
	write(t, src, `{"timestamp":"2026-10-07T01:00:00Z"}`+"\nREWRITTEN\n")
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	same(t, src, dest)
	prev, _ := filepath.Glob(dest + ".prev-*")
	if len(prev) != 1 {
		t.Fatal("old copy not kept", prev)
	}
	if raw, _ := os.ReadFile(prev[0]); !strings.HasSuffix(string(raw), "more\n") {
		t.Fatal(string(raw))
	}
	os.Remove(src)
	Sweep()
	if _, err := os.Stat(dest); err != nil {
		t.Fatal("copy lost after source deletion")
	}
}

func TestUndatedTranscriptStaysPut(t *testing.T) {
	home, raw := fixture(t)
	src := filepath.Join(home, ".claude", "projects", "p", "s.jsonl")
	write(t, src, `{"type":"summary"}`+"\n")
	if got := Dest(raw, "claude", src); got != filepath.Join(raw, "claude", "undated", "s.jsonl") {
		t.Fatal(got)
	}
}

func TestSweepReconcilesEqualSizeAndMiddleRewrite(t *testing.T) {
	home, raw := fixture(t)
	src := filepath.Join(home, ".claude", "projects", "p", "s.jsonl")
	text := `{"timestamp":"2026-10-07T01:00:00Z"}` + "\n" + strings.Repeat("a", 256<<10)
	write(t, src, text)
	dest := Dest(raw, "claude", src)
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	original := text
	b := []byte(text)
	b[len(b)/2] = 'z'
	write(t, src, string(b))
	// Even a source that preserves its stat-cache timestamp must be fully reconciled.
	c := checkpoint(dest)
	if err := os.Chtimes(src, timeFromStamp(c.From), timeFromStamp(c.From)); err != nil {
		t.Fatal(err)
	}
	out, err := Sweep()
	if err != nil || len(out["failed"].([]string)) != 0 || out["copied_bytes"] != int64(len(b)) {
		t.Fatal(out, err)
	}
	same(t, src, dest)
	previous, _ := filepath.Glob(dest + ".prev-*")
	if len(previous) != 1 {
		t.Fatal(previous)
	}
	old, _ := os.ReadFile(previous[0])
	if string(old) != original {
		t.Fatal("rewrite lost the original snapshot")
	}
}

func timeFromStamp(s archiveStamp) time.Time { return time.Unix(0, s.Mtime) }

func TestArchiveChangedSameSizeDoesNotUseCheckpoint(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, strings.Repeat("a", 256<<10))
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	b := []byte(strings.Repeat("a", 256<<10))
	b[len(b)/2] = 'b'
	write(t, src, string(b))
	changedAt := timeFromStamp(checkpoint(dest).From).Add(time.Second)
	if err := os.Chtimes(src, changedAt, changedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	same(t, src, dest)
}

func TestCorruptCheckpointFallsBackToFullComparison(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, strings.Repeat("a", 256<<10))
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	meta, _ := os.ReadFile(dest + ".checkpoint.json")
	write(t, dest+".checkpoint.json", string(meta)+"broken")
	b := []byte(strings.Repeat("a", 256<<10))
	b[len(b)/2] = 'b'
	write(t, src, string(b)+"tail")
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	same(t, src, dest)
}

func TestSweepChecksMiddleChangeDuringAppend(t *testing.T) {
	home, raw := fixture(t)
	src := filepath.Join(home, ".claude", "projects", "p", "s.jsonl")
	text := `{"timestamp":"2026-10-07T01:00:00Z"}` + "\n" + strings.Repeat("a", 256<<10)
	write(t, src, text)
	dest := Dest(raw, "claude", src)
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	b := []byte(text)
	b[len(b)/2] = 'b'
	write(t, src, string(b)+"tail\n")
	// The hook's known-append path is bounded. The next background sweep checks all bytes.
	if n, err := Archive(src, dest); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if out, err := Sweep(); err != nil || len(out["failed"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	same(t, src, dest)
	previous, _ := filepath.Glob(dest + ".prev-*")
	if len(previous) != 1 {
		t.Fatal(previous)
	}
	old, _ := os.ReadFile(previous[0])
	if !strings.HasPrefix(string(old), text) {
		t.Fatal("the pre-rewrite bytes were lost")
	}
}

func TestFullPrefixComparisonRejectsMiddleChangeAndShortRead(t *testing.T) {
	a := bytes.Repeat([]byte("a"), 256<<10)
	b := append([]byte(nil), a...)
	b[len(b)/2] = 'b'
	if samePrefix(bytes.NewReader(a), bytes.NewReader(b), int64(len(a))) {
		t.Fatal("middle difference passed full comparison")
	}
	if samePrefix(bytes.NewReader(a[:10]), bytes.NewReader(a[:10]), 11) {
		t.Fatal("short equal reads passed as a complete prefix")
	}
}

type faultCopyFile struct {
	buffer                      bytes.Buffer
	writeErr, syncErr, closeErr error
	short                       bool
	closed, synced              bool
}

func (f *faultCopyFile) Write(b []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short && len(b) > 0 {
		return f.buffer.Write(b[:len(b)-1])
	}
	return f.buffer.Write(b)
}
func (f *faultCopyFile) String() string { return f.buffer.String() }
func (f *faultCopyFile) Sync() error    { f.synced = true; return f.syncErr }
func (f *faultCopyFile) Close() error   { f.closed = true; return f.closeErr }

func TestSnapshotCopyChecksLengthWriteSyncAndCloseFailures(t *testing.T) {
	failure := errors.New("injected storage failure")
	for _, tc := range []struct {
		name string
		file faultCopyFile
		size int64
		want error
	}{
		{"short source", faultCopyFile{}, 4, io.EOF},
		{"write", faultCopyFile{writeErr: failure}, 3, failure},
		{"short write", faultCopyFile{short: true}, 3, io.ErrShortWrite},
		{"sync", faultCopyFile{syncErr: failure}, 3, failure},
		{"close", faultCopyFile{closeErr: failure}, 3, failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := copySnapshot(&tc.file, strings.NewReader("abc"), tc.size)
			if !errors.Is(err, tc.want) || !tc.file.closed {
				t.Fatal(err, "closed", tc.file.closed)
			}
		})
	}
	out := &faultCopyFile{}
	if n, err := copySnapshot(out, strings.NewReader("abc-later-growth"), 3); err != nil || n != 3 || out.String() != "abc" || !out.synced || !out.closed {
		t.Fatal(n, err, out)
	}
}

func TestShortReplacementLeavesPreviousCopyPublished(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, "new transcript")
	write(t, dest, "old transcript")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	info, _ := in.Stat()
	if err := os.Truncate(src, 3); err != nil {
		t.Fatal(err)
	}
	if n, err := replaceSnapshot(in, src, dest, archiveStat(info), true); err == nil || n != 0 {
		t.Fatal("short replacement published", n, err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "old transcript" {
		t.Fatal("prior copy changed", string(got))
	}
	staged, _ := filepath.Glob(filepath.Join(home, ".archive-*"))
	previous, _ := filepath.Glob(dest + ".prev-*")
	if len(staged)+len(previous) != 0 {
		t.Fatal("failed staging leaked files", staged, previous)
	}
}

func TestRotationDoesNotPublishOpenedOldGeneration(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, "opened source")
	write(t, dest, "prior snapshot")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	info, _ := in.Stat()
	replacement := filepath.Join(home, "replacement")
	write(t, replacement, "rotated source")
	if err := os.Rename(replacement, src); err != nil {
		t.Fatal(err)
	}
	if _, err := replaceSnapshot(in, src, dest, archiveStat(info), true); !errors.Is(err, ErrSourceChanging) {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "prior snapshot" {
		t.Fatal(string(got))
	}
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	same(t, src, dest)
}

func TestSnapshotAppendIsBoundedAndNextCallCapturesGrowth(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, "old\n")
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	write(t, src, "old\nfirst\n")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	info, _ := in.Stat()
	write(t, src, "old\nfirst\nlater\n")
	from := archiveStat(info)
	if n, err := appendSnapshot(in, src, dest, from, 4); err != nil || n != 6 {
		t.Fatal(n, err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "old\nfirst\n" {
		t.Fatal("copied beyond observed snapshot", string(got))
	}
	if err := saveCheckpoint(src, dest, from); err != nil {
		t.Fatal(err)
	}
	if n, err := Archive(src, dest); err != nil || n != 6 {
		t.Fatal(n, err)
	}
	same(t, src, dest)
}

func TestShortAppendRollsBackToPriorLength(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, "old\nnew tail\n")
	write(t, dest, "old\n")
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	info, _ := in.Stat()
	if err := os.Truncate(src, 6); err != nil {
		t.Fatal(err)
	}
	if n, err := appendSnapshot(in, src, dest, archiveStat(info), 4); err == nil || n != 0 {
		t.Fatal(n, err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "old\n" {
		t.Fatal("partial tail retained", string(got))
	}
}

func TestArchiveLockSurvivesDestinationReplacement(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	write(t, src, "new")
	write(t, dest, "old")
	lock, err := exclusive(dest + ".lock")
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	defer lock.Close()
	replacement := filepath.Join(home, "replacement")
	write(t, replacement, "replacement")
	if err := os.Rename(replacement, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := Archive(src, dest); !errors.Is(err, ErrArchiveBusy) {
		t.Fatal("replacement bypassed stable lock", err)
	}
}

func TestConcurrentArchiveRewritesKeepOnePreviousSnapshot(t *testing.T) {
	home, _ := fixture(t)
	src, dest := filepath.Join(home, "source"), filepath.Join(home, "copy")
	old := strings.Repeat("a", 256<<10)
	write(t, src, old)
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	write(t, src, strings.Repeat("b", 256<<10))
	start := make(chan struct{})
	errs := make(chan error, 24)
	var wg sync.WaitGroup
	for i := 0; i < cap(errs); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := Archive(src, dest)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrArchiveBusy) {
			t.Fatal(err)
		}
	}
	if _, err := Archive(src, dest); err != nil {
		t.Fatal(err)
	}
	same(t, src, dest)
	previous, _ := filepath.Glob(dest + ".prev-*")
	if len(previous) != 1 {
		t.Fatal(previous)
	}
	got, _ := os.ReadFile(previous[0])
	if string(got) != old {
		t.Fatal("concurrent rewrite lost prior snapshot")
	}
}

func TestVerifyDistinguishesIncompleteMismatchAndBusy(t *testing.T) {
	home, raw := fixture(t)
	src := filepath.Join(home, ".codex", "sessions", "rollout-2026-10-07T01-00-00-x.jsonl")
	write(t, src, "abcde")
	dest := Dest(raw, "codex", src)
	write(t, dest, "abc")
	out, err := Verify()
	if err != nil || out["complete"] != false || len(out["incomplete"].([]string)) != 1 || len(out["mismatched"].([]string)) != 0 {
		t.Fatal(out, err)
	}
	write(t, dest, "abxde")
	out, err = Verify()
	if err != nil || out["complete"] != false || len(out["mismatched"].([]string)) != 1 {
		t.Fatal(out, err)
	}
	write(t, dest, "abcde")
	out, err = Verify()
	if err != nil || out["complete"] != true {
		t.Fatal(out, err)
	}
	lock, err := exclusive(dest + ".lock")
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	out, err = Verify()
	lock.Close()
	if err != nil || out["complete"] != false || len(out["changing"].([]string)) != 1 {
		t.Fatal(out, err)
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	out, err = Verify()
	if err != nil || out["complete"] != false || len(out["incomplete"].([]string)) != 1 {
		t.Fatal(out, err)
	}
}

func TestSourceScanFailureIsNotReportedAsComplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a mode-000 fixture")
	}
	home, _ := fixture(t)
	for _, root := range []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".claude-profiles")} {
		write(t, filepath.Join(root, "s.jsonl"), "not readable\n")
		if err := os.Chmod(root, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
		out, err := Verify()
		if err != nil || out["complete"] != false || len(out["failed"].([]string)) == 0 {
			t.Fatal(out, err)
		}
		out, err = Sweep()
		if err != nil || len(out["failed"].([]string)) == 0 {
			t.Fatal(out, err)
		}
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}
