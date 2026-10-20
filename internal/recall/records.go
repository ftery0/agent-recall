package recall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// record is one stored record file: its front matter read once, and its body.
type record struct {
	path string
	raw  []byte
	mod  time.Time
	head map[string]string
}

// recordRequest is one 요청 line, kept with the request ID and raw line it came from so a find
// can point back at the person's words.
type recordRequest struct {
	at     time.Time
	id     string
	line   int
	source string
	text   string
	answer string
}

// loadRecords reads every record. A missing record folder is an empty one; anything unreadable
// is returned beside the records so each caller decides whether a partial view is enough.
func loadRecords() ([]record, []error) {
	if _, err := os.Lstat(RecordDir()); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, []error{err}
	}
	days, err := os.ReadDir(RecordDir())
	if err != nil {
		return nil, []error{err}
	}
	var out []record
	var errs []error
	for _, day := range days {
		folder := filepath.Join(RecordDir(), day.Name())
		if !day.IsDir() {
			// A file at an expected date-folder path is corruption, not an empty day.
			if _, err := time.Parse("2006-01-02", day.Name()); err == nil {
				errs = append(errs, fmt.Errorf("%s: 날짜 폴더 자리에 파일", folder))
			}
			continue
		}
		files, err := os.ReadDir(folder)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), ".md") {
				continue
			}
			path := filepath.Join(folder, file.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			info, err := file.Info()
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, record{path: path, raw: raw, mod: info.ModTime(), head: frontMatter(raw)})
		}
	}
	return out, errs
}

// frontMatter keeps the first value of each field before the first heading, as header does.
func frontMatter(raw []byte) map[string]string {
	head := map[string]string{}
	for line := range bytes.Lines(raw) {
		line = bytes.TrimSuffix(line, []byte("\n"))
		if bytes.HasPrefix(line, []byte("#")) {
			break
		}
		if key, value, ok := bytes.Cut(line, []byte(": ")); ok && !bytes.Contains(key, []byte(" ")) {
			if _, seen := head[string(key)]; !seen {
				head[string(key)] = string(value)
			}
		}
	}
	return head
}

func (r record) field(key string) string { return r.head[key] }

func (r record) title() string { return title(string(r.raw), filepath.Base(r.path)) }

// cwd unquotes the stored folder; older records left it bare.
func (r record) cwd() string {
	folder := r.field("cwd")
	if unquoted, err := strconv.Unquote(folder); err == nil {
		return unquoted
	}
	return folder
}

// ended is when the conversation stopped, or the file time when the record does not say.
func (r record) ended() time.Time { return recordTime(r.raw, r.mod) }

// started is when the conversation's first request came, or zero when the record does not say.
func (r record) started() time.Time {
	first, _, _ := strings.Cut(r.field("time"), "-")
	at, err := time.ParseInLocation("2006-01-02 15:04", r.field("date")+" "+first, time.Local)
	if err != nil {
		return time.Time{}
	}
	return at
}

// summary reads back the model's part of a record, so a record rebuilt for a newer format keeps
// its summary without another model call. ok is false unless it came from the model.
func (r record) summary() (summary, bool) {
	var m summary
	if data := r.field("summary_data"); data != "" && json.Unmarshal([]byte(data), &m) == nil && m.Title != "" {
		return m, true
	}
	text := string(r.raw)
	if r.field("summary") != "haiku" && !strings.Contains(text, "\n### 결정\n") && !strings.Contains(text, "\n### 남은 일\n") {
		return m, false
	}
	m.Title, _ = strconv.Unquote(r.field("title"))
	for _, item := range r.items("결정") {
		what, why, _ := strings.Cut(item, " — ")
		m.Decisions = append(m.Decisions, decision{What: what, Why: why})
	}
	m.Todo, m.Lessons = append(m.Todo, r.items("남은 일")...), append(m.Lessons, r.items("교훈 후보")...)
	return m, m.Title != ""
}

// requests reads the 요청 lines. Format 5 lines carry their day ("01-02 15:04"); older lines
// carry only the time, so a time earlier than the one before means the next day.
func (r record) requests() []recordRequest {
	start, err := time.ParseInLocation("2006-01-02", r.field("date"), time.Local)
	if err != nil {
		return nil
	}
	_, body, ok := strings.Cut(string(r.raw), "\n### 요청\n")
	if !ok {
		return nil
	}
	ending := ""
	if _, rest, ok := strings.Cut(body, "\n### 끝난 지점\n- "); ok {
		ending, _, _ = strings.Cut(rest, "\n")
		ending = strings.TrimSuffix(ending, " (AI 주장)")
	}
	var out []recordRequest
	last := start
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			break
		}
		if answer, ok := strings.CutPrefix(line, "  → AI(주장): "); ok && len(out) > 0 {
			out[len(out)-1].answer = answer
			continue
		}
		stamp, rest, ok := strings.Cut(strings.TrimPrefix(line, "- "), " [")
		where, text, found := strings.Cut(rest, "] ")
		if !ok || !found || !strings.HasPrefix(line, "- ") {
			continue
		}
		var at time.Time
		if len(stamp) == len(requestStamp) {
			at, err = time.ParseInLocation("2006-01-02 15:04", fmt.Sprintf("%d-%s", start.Year(), stamp), time.Local)
			if err == nil && at.Before(start) { // the conversation ran past New Year
				at = at.AddDate(1, 0, 0)
			}
		} else {
			at, err = time.ParseInLocation("2006-01-02 15:04", last.Format("2006-01-02")+" "+stamp, time.Local)
			if err == nil && at.Before(last) {
				at = at.AddDate(0, 0, 1)
			}
		}
		if err != nil {
			continue
		}
		last = at
		req := recordRequest{at: at, text: text}
		// "<요청 ID> 줄<N> <출처>"; anything else keeps only the text.
		if parts := strings.Fields(where); len(parts) == 3 && strings.HasPrefix(parts[1], "줄") {
			if n, err := strconv.Atoi(strings.TrimPrefix(parts[1], "줄")); err == nil {
				req.id, req.line, req.source = parts[0], n, parts[2]
			}
		}
		out = append(out, req)
	}
	// The last request is answered by where the conversation stopped.
	if len(out) > 0 && out[len(out)-1].answer == "" {
		out[len(out)-1].answer = ending
	}
	return out
}

// withoutSection drops one ### section from a record's body, such as the 요청 a search indexes
// one by one or the 기억 명령 that only repeat what the agent looked for.
func withoutSection(body, name string) string {
	before, rest, ok := strings.Cut(body, "\n### "+name+"\n")
	if !ok {
		return body
	}
	if i := strings.Index(rest, "\n#"); i >= 0 {
		return before + rest[i:]
	}
	return before + "\n"
}

// items are the "- " lines of one ### section, such as 바뀐 파일.
func (r record) items(name string) []string {
	_, rest, ok := strings.Cut(string(r.raw), "\n### "+name+"\n")
	if !ok {
		return nil
	}
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		item, ok := strings.CutPrefix(line, "- ")
		if !ok {
			break
		}
		out = append(out, item)
	}
	return out
}

var fullSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// RepoName is the git top folder's name, else the folder's own name. A linked worktree is named
// by its main folder: its own folder name is whatever the worktree was called.
func RepoName(cwd string) string {
	if root := worklog.RepoRoot(cwd); root != "" {
		return filepath.Base(mainRoot(root))
	}
	return filepath.Base(cwd)
}

// mainRoot is the main folder of the repository a linked worktree belongs to, read from the
// worktree's ".git" file; any other folder is its own.
func mainRoot(root string) string {
	if root == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(root, ".git"))
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir: ")
	if err != nil || !ok {
		return root
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(root, gitdir)
	}
	if main, _, ok := strings.Cut(filepath.ToSlash(gitdir), "/.git/worktrees/"); ok {
		return main
	}
	return root
}
