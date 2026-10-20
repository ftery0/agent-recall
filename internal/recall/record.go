package recall

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/worklog"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	idleBefore   = 30 * time.Minute // a session untouched this long is treated as finished
	maxFiles     = 30
	maxCommands  = 15
	maxCompact   = 10000 // characters of the tool's own compaction summary kept in a record
	recordFolder = "기록"
	recordFormat = 11            // raise when reading rules change; older records are rebuilt
	requestStamp = "01-02 15:04" // request lines carry their day; a conversation can run for days
	endStamp     = "2006-01-02 15:04"
)

var (
	patchFile = regexp.MustCompile(`\*\*\* (?:Update|Add|Delete) File: ([^\n\\"]+)`)
	exitCode  = regexp.MustCompile(`(?:exited with code|Exit code:?)\s*(\d+)`)
	// A Codex command still running when the call returned; write_stdin calls read the rest.
	stillRunning = regexp.MustCompile(`Process running with session ID (\d+)`)
	// Lines saying what git or gh made, kept past the cut of a long output.
	madeLine  = regexp.MustCompile(`(?m)^(?:\[[^\]\n]+ [0-9a-f]{7,40}\] .*|To \S+|\s*[ +*]\s*(?:[0-9a-f]{7,40}\.\.\.?[0-9a-f]{7,40}|\[new [a-z]+\])\s+\S+\s+->\s+\S+.*|ok [0-9a-f]{7,40}\b.*|.*https://github\.com/[\w.-]+/[\w.-]+/pull/\d+.*)$`)
	checkLike = regexp.MustCompile(`\b(test|lint|tsc|vet|build|eslint|pytest|jest|vitest|typecheck)\b`)
	pipe      = regexp.MustCompile(`(^|[^|])\|([^|]|$)`)
	// A memory lookup the agent ran, by name, through rtk, or by the installed path, where a command
	// begins; the same words inside a script or a quoted text are not a lookup.
	recallCommand = regexp.MustCompile(`(?m)(?:^|&&|\|\||[;|]|\$\()\s*(?:rtk\s+(?:proxy\s+)?)?['"]?(?:[^\s'"]*/)?harness(?:-[\d.]+)?['"]?\s+recall\s+(?:find|day|source)\b`)
	heredoc       = regexp.MustCompile(`<<-?\s*['"]?([A-Za-z_]\w*)['"]?`)
	// The subject line of `git commit -m "…"`, including the heredoc form and `git -C <dir> commit`.
	// commitFile is a commit reading its message from a file, "-" being its own input.
	commitFile    = regexp.MustCompile(`git(?:\s+-[cC]\s+(?:"[^"]+"|'[^']+'|\S+))*\s+commit\b[^\n]*?\s(?:-[a-zA-Z]*F\s*|--file[= ])(\S+)`)
	commitMessage = regexp.MustCompile(`git(?:\s+-[cC]\s+(?:"[^"]+"|'[^']+'|\S+))*\s+commit\b[^\n]*?\s-[a-zA-Z]*m\s+(?:"\$\(cat <<-?'?EOF'?\s*\n)?["']?([^"'\n]+)`)
	tagged        = regexp.MustCompile(`(?s)<[a-z_][a-z0-9_-]*[^>]*>(.*?)</[a-z_][a-z0-9_-]*[^>]*>`)
	imageRef      = regexp.MustCompile(`\[Image #\d+\]`)
	// Tags a nested block leaves behind once the outer pair is removed.
	tagToken = regexp.MustCompile(`</?[a-zA-Z_][\w:-]*[^>]*>`)
	// Common secret shapes are masked before anything is written or sent to the model;
	// a leading group, when present, is the key name that stays readable.
	secrets = []*regexp.Regexp{
		regexp.MustCompile(`(?i)()\b(?:sk|pk|rk)-[a-z0-9_-]{16,}`),
		regexp.MustCompile(`()\b(?:ghp|gho|ghs|ghu|github_pat)_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`()\bxox[abposr]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`()\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`()\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{16,}`),
		regexp.MustCompile(`(?i)((?:password|passwd|secret|token|api[_-]?key|access[_-]?key)["']?\s*[:=]\s*["']?)[^\s"',]{6,}`),
		regexp.MustCompile(`()-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	}
)

func mask(s string) string {
	for _, re := range secrets {
		s = re.ReplaceAllString(s, "${1}[가림]")
	}
	return s
}

type command struct {
	text   string
	source string
	result string // 성공, 실패, 끝 명령 성공 (a pipeline), or empty when the transcript does not say
	piped  bool   // a pipeline's exit is its last command's; a check before a pipe may have failed
	output string // what git and gh printed names what they made
	dir    string // the folder the tool call named, when it named one
	check  bool   // runs a test, a linter, a type check or a build
	github bool   // changes GitHub: a pull request or a repository
	at     time.Time
	done   time.Time // when its result came; git's reflog is read between the two
}

type session struct {
	tool, id, path, cwd, branch string
	start, end                  time.Time
	size                        int64
	requests                    []worklog.Request
	hash                        string
	cursor                      int
	summaryState, summaryError  string
	summaryAttempt              *summaryAttempt
	reports                     []string
	files                       []string
	omitted                     int       // changed files left out of files, the oldest
	commands                    []command // checks, and git, gh and copy calls
	tries                       []tried   // every shell command, for the failures it got past
	prLinks                     []string  // pull requests the client linked to the conversation
	recalls                     []command // memory lookups the agent ran and how each came back
	compact                     string    // the tool's own summary at its latest compaction
	events                      []gitEvent
	copies                      [][2]string // repository names files were copied from and to
}

type decision struct {
	What     string `json:"what"`
	Why      string `json:"why"`
	Source   string `json:"source,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Status   string `json:"status,omitempty"`
}

// summary is what the model adds on top of the fixed fields.
type summary struct {
	Title       string       `json:"title"`
	Decisions   []decision   `json:"decisions"`
	Todo        []string     `json:"todo"`
	Lessons     []string     `json:"lessons"`
	Resolutions []resolution `json:"resolutions,omitempty"`
}

// readSession pulls the fixed fields out of a raw transcript. Sub-agent threads and sessions
// with nothing typed by a person are not sessions of their own.
func readSession(tool, path string) (session, bool) {
	s, ok, _ := readSessionChecked(tool, path)
	return s, ok
}

func readSessionChecked(tool, path string) (session, bool, error) {
	s := session{tool: tool, path: path}
	f, err := os.Open(path)
	if err != nil {
		return s, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return s, false, err
	}
	s.size = info.Size()
	note := func(file string) {
		if file = strings.TrimSpace(file); file != "" && file != "/dev/null" && !scratchRepo(file) {
			s.files = append(s.files, file)
		}
	}
	pending, asked, ran := map[string]int{}, map[string]int{}, map[string]int{}
	edits := 0
	run := func(id, text, dir string, at time.Time) {
		_, seenCommand := pending[id]
		_, seenLookup := asked[id]
		_, seenTry := ran[id]
		if id != "" && (seenCommand || seenLookup || seenTry) {
			return // a tool call written twice is one run
		}
		// Words inside a heredoc are text handed to another program, not a command run.
		text = strings.TrimSpace(text)
		ran[id] = len(s.tries)
		s.tries = append(s.tries, tried{text: text, source: "tool:" + id, edits: edits, at: at})
		shell := shellOnly(text)
		if recallCommand.MatchString(shell) {
			asked[id] = len(s.recalls)
			s.recalls = append(s.recalls, command{text: text, source: "tool:" + id, at: at})
			return
		}
		c := command{text: text, source: "tool:" + id, piped: pipe.MatchString(shell), at: at, dir: dir}
		keep := false
		for _, step := range shellSteps(resolveDir(s.cwd, dir), text) {
			switch {
			case step.kind == "write":
				note(step.to)
			case step.kind == "check":
				c.check, keep = true, true
			case ghWrites[step.kind]:
				c.github, keep = true, true
			case step.writesHistory(), step.kind == "copy":
				keep = true
			}
		}
		if keep {
			pending[id] = len(s.commands)
			s.commands = append(s.commands, c)
		}
	}
	finish := func(id string, failed bool, output string, at time.Time) {
		if i, ok := ran[id]; ok {
			s.tries[i].done, s.tries[i].failed = true, failed || failedOutput.MatchString(output)
		}
		if i, ok := pending[id]; ok {
			c := &s.commands[i]
			c.result, c.output, c.done = map[bool]string{false: "성공", true: "실패"}[failed], keepOutput(output), at
			if !failed && c.piped {
				c.result = "끝 명령 성공"
			}
		}
	}
	answered := func(id string, failed bool, output string) {
		if i, ok := asked[id]; ok {
			s.recalls[i].result = recallResult(failed, output)
		}
	}
	// Claude's tool calls, in the conversation and in the sub-agents it started.
	blocks := func(message map[string]any, at time.Time) {
		content, _ := message["content"].([]any)
		for _, b := range content {
			block, _ := b.(map[string]any)
			input, _ := block["input"].(map[string]any)
			id, _ := block["id"].(string)
			switch {
			case block["type"] == "tool_use" && (block["name"] == "Edit" || block["name"] == "Write" || block["name"] == "MultiEdit" || block["name"] == "NotebookEdit"):
				file, _ := input["file_path"].(string)
				if file == "" {
					file, _ = input["notebook_path"].(string)
				}
				note(file)
				edits++
			case block["type"] == "tool_use" && block["name"] == "Bash":
				text, _ := input["command"].(string)
				run(id, text, "", at)
			case block["type"] == "tool_result":
				use, _ := block["tool_use_id"].(string)
				finish(use, block["is_error"] == true, flatText(block["content"]), at)
				answered(use, block["is_error"] == true, flatText(block["content"]))
			}
		}
	}
	scripted := map[string][]scriptCall{}
	// A Codex command still running when its call returned ends in later write_stdin calls.
	running, continued, earlier := map[string]string{}, map[string]string{}, map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(f, s.size))
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	line := 0
	for scanner.Scan() {
		line++
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return s, false, fmt.Errorf("%s:%d: 대화 행 해석 실패", path, line)
		}
		at := stamp(row["timestamp"])
		if row["isSidechain"] == true {
			// An older client wrote sub-agent turns into the conversation: their edits count, their clock does not.
			message, _ := row["message"].(map[string]any)
			blocks(message, at)
			continue
		}
		if !at.IsZero() {
			if s.start.IsZero() {
				s.start = at
			}
			s.end = at
		}
		payload, _ := row["payload"].(map[string]any)
		if tool == "claude" {
			if v, _ := row["cwd"].(string); s.cwd == "" {
				s.cwd = v
			}
			if v, _ := row["gitBranch"].(string); v != "" {
				s.branch = v
			}
			if v, _ := row["sessionId"].(string); v != "" {
				s.id = v
			}
			if url, _ := row["prUrl"].(string); row["type"] == "pr-link" && url != "" && !slices.Contains(s.prLinks, url) {
				s.prLinks = append(s.prLinks, url)
			}
			message, _ := row["message"].(map[string]any)
			// Claude writes its compaction summary as plain text; each one restates the ones before.
			if text, _ := message["content"].(string); row["isCompactSummary"] == true && text != "" {
				s.compact = text
			}
			blocks(message, s.end)
			continue
		}
		switch {
		case row["type"] == "session_meta":
			if src, ok := payload["source"].(map[string]any); ok && src["subagent"] != nil {
				return s, false, nil
			}
			s.id, _ = payload["id"].(string)
			s.cwd, _ = payload["cwd"].(string)
			if git, ok := payload["git"].(map[string]any); ok {
				s.branch, _ = git["branch"].(string)
			}
		case row["type"] == "response_item" && (payload["type"] == "function_call" || payload["type"] == "custom_tool_call"):
			args, _ := payload["arguments"].(string)
			if args == "" {
				args, _ = payload["input"].(string)
			}
			for _, m := range patchFile.FindAllStringSubmatch(args, -1) {
				note(m[1])
				edits++
			}
			var call struct {
				Cmd       string   `json:"cmd"`
				Command   []string `json:"command"`
				Workdir   string   `json:"workdir"`
				SessionID any      `json:"session_id"`
			}
			json.Unmarshal([]byte(args), &call)
			id, _ := payload["call_id"].(string)
			if origin, ok := running[fmt.Sprint(call.SessionID)]; ok && payload["name"] == "write_stdin" {
				continued[id] = origin
			}
			if call.Cmd != "" {
				run(id, call.Cmd, call.Workdir, s.end)
			} else {
				run(id, strings.Join(call.Command, " "), call.Workdir, s.end)
			}
			// The app also runs commands from a script, several at once.
			scripted[id] = scriptCommands(args)
			for i, c := range scripted[id] {
				run(fmt.Sprintf("%s#%d", id, i), c.cmd, c.workdir, s.end)
			}
		case row["type"] == "response_item" && (payload["type"] == "function_call_output" || payload["type"] == "custom_tool_call_output"):
			id, _ := payload["call_id"].(string)
			results := scriptResults(payload["output"], len(scripted[id]))
			if origin, ok := continued[id]; ok {
				id = origin
			}
			output := flatText(payload["output"])
			exit := exitCode.FindStringSubmatch(output)
			if exit != nil {
				finish(id, exit[1] != "0", earlier[id]+output, s.end)
			} else if m := stillRunning.FindStringSubmatch(output); m != nil {
				running[m[1]], earlier[id] = id, earlier[id]+output+"\n"
			}
			answered(id, exit != nil && exit[1] != "0", output)
			for _, r := range results {
				finish(fmt.Sprintf("%s#%d", id, r.index), r.failed, r.output, s.end)
				answered(fmt.Sprintf("%s#%d", id, r.index), r.failed, r.output)
			}
		}
	}
	if s.id == "" {
		s.id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if err := scanner.Err(); err != nil {
		return s, false, err
	}
	if tool == "claude" {
		// Sub-agents keep their own transcripts beside the conversation's. One being written may end
		// in half a line; what it did before stays readable.
		agents, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents", "*.jsonl"))
		for _, agent := range agents {
			raw, err := os.ReadFile(agent)
			if err != nil {
				continue
			}
			for line := range bytes.Lines(raw) {
				var row map[string]any
				if json.Unmarshal(line, &row) == nil {
					message, _ := row["message"].(map[string]any)
					blocks(message, stamp(row["timestamp"]))
				}
			}
		}
	}
	s.requests, s.reports, err = worklog.ReadConversation(path)
	if err != nil {
		return s, false, err
	}
	after, err := os.Stat(path)
	if err != nil {
		return s, false, err
	}
	if !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
		return s, false, fmt.Errorf("%s: 읽는 중 원본 변경, 다음 수집에서 재시도", path)
	}
	s.hash, err = prefixHash(path, s.size)
	if err != nil {
		return s, false, err
	}
	s.files = relative(s.cwd, s.files)
	s.omitted = max(0, len(s.files)-maxFiles)
	s.files = s.files[s.omitted:]
	return s, len(s.requests) > 0, nil
}

func stamp(v any) time.Time {
	text, _ := v.(string)
	at, _ := time.Parse(time.RFC3339Nano, text)
	return at
}

// relative shortens paths inside the session folder and drops repeats, newest last.
func relative(cwd string, files []string) []string {
	seen := map[string]bool{}
	var out []string
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		if rel, err := filepath.Rel(cwd, file); err == nil && cwd != "" && !strings.HasPrefix(rel, "..") {
			file = rel
		}
		if !seen[file] {
			seen[file] = true
			out = append([]string{file}, out...)
		}
	}
	return out
}

// short is the 8 characters that tell sessions apart in file names. Codex ids are UUIDv7, whose
// front is a timestamp shared by sessions started in the same minute, so they use the random end.
func short(id string) string {
	if len(id) > 36 {
		id = id[len(id)-36:]
	}
	if len(id) == 36 && id[14] == '7' {
		return id[28:]
	}
	return id[:min(8, len(id))]
}

// requestTitle names a session by what the person typed. Pasted blocks, app tags and image
// markers are not their words, so the first request with words of its own wins.
func requestTitle(requests []worklog.Request) string {
	words := func(text string) string {
		return strings.Join(strings.Fields(imageRef.ReplaceAllString(tagToken.ReplaceAllString(text, ""), "")), " ")
	}
	for _, r := range requests {
		if text := words(tagged.ReplaceAllString(r.Text, "")); text != "" {
			return cut(text, 60)
		}
	}
	// Only pasted text: its opening still names the work.
	for _, r := range requests {
		for _, m := range tagged.FindAllStringSubmatch(r.Text, -1) {
			if text := words(m[1]); text != "" {
				return cut(text, 60)
			}
		}
	}
	if len(requests) == 0 {
		return ""
	}
	return cut(words(requests[0].Text), 60)
}

// gist keeps the opening of an agent message, where answers put their conclusion. Headings,
// fences and table rules carry no words of their own.
func gist(text string, limit int) string {
	var parts []string
	n := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "```") || strings.Trim(line, "|-: ") == "" {
			continue
		}
		line = strings.ReplaceAll(strings.TrimLeft(line, "-*> "), "**", "")
		parts = append(parts, line)
		if n += len([]rune(line)) + 1; n >= limit {
			break
		}
	}
	return cut(mask(strings.Join(parts, " ")), limit)
}

// lastAnswer is the agent's latest message once it follows the person's last request; a request
// still waiting for its answer has none yet.
func lastAnswer(requests []worklog.Request, reports []string) string {
	if len(reports) == 0 || (len(requests) > 0 && requests[len(requests)-1].Context == reports[len(reports)-1]) {
		return ""
	}
	return reports[len(reports)-1]
}

// compactBody drops the tool's framing around a compaction summary and keeps the summary.
func compactBody(text string) string {
	if _, rest, ok := strings.Cut(text, "\nSummary:\n"); ok {
		text = rest
	}
	if before, _, ok := strings.Cut(text, "If you need specific details from before compaction"); ok {
		text = before
	}
	return strings.TrimSpace(text)
}

// render writes a record: 제목, 내용(요청과 AI 답 요지·끝난 지점·결정·남은 일·압축 요약),
// 자료(파일·검사·커밋), 소스(원본 위치). Everything comes from the transcript; the model part is optional.
func render(s session, m summary, summarized bool) string {
	title := m.Title
	if title == "" {
		title = requestTitle(s.requests)
	}
	title = strings.ReplaceAll(mask(title), "\n", " ")
	var b strings.Builder
	fmt.Fprintf(&b, "---\ntitle: %q\ndate: %s\ntime: %s-%s\nend: %s\ntool: %s\nsession: %s\nrepo: %s\nbranch: %s\nsummary: %s\nraw: %s\nraw_bytes: %d\nformat: %d\n",
		title, s.start.Local().Format("2006-01-02"), s.start.Local().Format("15:04"), s.end.Local().Format("15:04"), s.end.Local().Format(endStamp),
		s.tool, s.id, RepoName(s.cwd), s.branch, map[bool]string{true: "haiku", false: "없음"}[summarized], s.path, s.size, recordFormat)
	state, cursor := s.summaryState, s.cursor
	if state == "" {
		state = "pending"
		if summarized {
			state = "ready"
			cursor = len(fragments(s))
		}
	}
	fmt.Fprintf(&b, "cwd: %q\nraw_sha256: %s\nsummary_state: %s\nsummary_cursor: %d\nsummary_prefix: %s\nsummary_error: %q\n", s.cwd, s.hash, state, cursor, requestPrefix(s, cursor), s.summaryError)
	if s.summaryAttempt != nil {
		fmt.Fprintf(&b, "last_summary_attempt: %s\n", mask(string(memory.Packed(s.summaryAttempt))))
	}
	if summarized {
		fmt.Fprintf(&b, "summary_data: %s\n", mask(string(memory.Packed(m))))
	}
	b.WriteString("---\n")
	fmt.Fprintf(&b, "\n# %s\n\n## 내용\n\n### 요청\n", title)
	for i, r := range s.requests {
		fmt.Fprintf(&b, "- %s [%s 줄%d %s] %s\n", r.At.Local().Format(requestStamp), r.ID, r.Line, r.Source, strings.ReplaceAll(cut(mask(r.Text), 400), "\n", " "))
		// The next request's preceding agent message answers this one; an equal context means
		// no answer came between them.
		if i+1 < len(s.requests) {
			if next := s.requests[i+1].Context; next != "" && next != r.Context {
				fmt.Fprintf(&b, "  → AI(주장): %s\n", gist(next, 200))
			}
		}
	}
	if last := lastAnswer(s.requests, s.reports); last != "" {
		fmt.Fprintf(&b, "\n### 끝난 지점\n- %s (AI 주장)\n", gist(last, 400))
	}
	section := func(name string, items []string) {
		if len(items) > 0 {
			b.WriteString("\n### " + name + "\n")
			for _, item := range items {
				b.WriteString("- " + strings.ReplaceAll(mask(item), "\n", " ") + "\n")
			}
		}
	}
	var decisions, history, todo []string
	for _, d := range m.Decisions {
		text := d.What
		if d.Why != "" {
			text += " — " + d.Why
		}
		if d.Source != "" {
			text += " [" + d.Status + " " + d.Source + ": " + d.Evidence + "]"
		} else {
			text += " [요약·근거 미확인]"
		}
		if r, ok := resolved(m, "decision", d.What); ok {
			history = append(history, text+" → "+r.State+" ["+r.Source+": "+r.Evidence+"]")
		} else {
			decisions = append(decisions, text)
		}
	}
	for _, v := range m.Todo {
		if r, ok := resolved(m, "todo", v); ok {
			history = append(history, v+" → "+r.State+" ["+r.Source+": "+r.Evidence+"]")
		} else {
			todo = append(todo, v)
		}
	}
	for _, r := range m.Resolutions {
		if r.Stale {
			history = append(history, r.What+" → 과거 "+r.State+"·현재 원문 근거 변경 ["+r.Source+": "+r.Evidence+"]")
		}
	}
	section("결정", decisions)
	section("남은 일", todo)
	section("완료·취소·교체 이력", history)
	section("교훈 후보", m.Lessons)
	if state == "pending" || state == "failed" {
		fmt.Fprintf(&b, "\n요약 갱신: %s (%d/%d 원문 구간). 이전 기억은 유지하며 남은 구간은 재처리한다.\n", state, cursor, len(fragments(s)))
	}
	if s.compact != "" {
		// Quoted so its own headings and list items never read as this record's sections.
		b.WriteString("\n### 압축 요약 (AI 작성·주장)\n")
		for _, line := range strings.Split(cut(mask(compactBody(s.compact)), maxCompact), "\n") {
			b.WriteString("> " + line + "\n")
		}
	}
	b.WriteString("\n## 자료\n")
	section("바뀐 파일", s.files)
	if s.omitted > 0 {
		fmt.Fprintf(&b, "(그 앞에 바꾼 파일 %d개는 원문에 있다)\n", s.omitted)
	}
	var checks, github []string
	for _, c := range s.commands {
		line := "`" + cut(c.text, 200) + "`"
		if c.result != "" {
			line += " → " + c.result
		}
		switch {
		case c.github:
			// "PR #12" is how a person asks for it later.
			if m := prLinkRe.FindStringSubmatch(c.output); m != nil {
				line += " · PR #" + m[3] + " " + m[0]
			}
			github = append(github, line+" ["+c.source+"]")
		case c.check:
			checks = append(checks, line+" ["+c.source+"]")
		}
	}
	for _, url := range s.prLinks {
		if m := prLinkRe.FindStringSubmatch(url); m != nil && !slices.ContainsFunc(github, func(line string) bool { return strings.Contains(line, m[0]) }) {
			github = append(github, "PR #"+m[3]+" "+m[0]+" (대화에 연결된 PR)")
		}
	}
	// git's own record of what was made; the shell line may hide it behind -q, a pipe or a variable.
	var commits, pushes, copies []string
	for _, e := range s.events {
		where := strings.TrimSpace(e.repo + " " + e.ref)
		if e.root != "" && e.origin == "" {
			where += ", 원격 없음"
		}
		hash := e.hash[:min(8, len(e.hash))]
		switch {
		case e.kind == "commit" && hash != "":
			commits = append(commits, hash+" "+e.subject+" ("+where+") ["+e.source+"]")
		case e.kind == "commit":
			commits = append(commits, e.subject+" ("+where+", 해시 모름) ["+e.source+"]")
		case hash != "":
			pushes = append(pushes, where+" ← "+hash+" ["+e.source+"]")
		default:
			pushes = append(pushes, where+" (새 브랜치) ["+e.source+"]")
		}
	}
	for _, pair := range s.copies {
		copies = append(copies, pair[0]+" → "+pair[1])
	}
	// Checks keep their latest; what was made is never cut.
	section("검사", checks[max(0, len(checks)-maxCommands):])
	section("커밋", commits)
	section("push", pushes)
	section("GitHub", github)
	section("레포 사이 복사", copies)
	var recalls []string
	for _, c := range s.recalls[max(0, len(s.recalls)-maxCommands):] {
		line := "`" + cut(c.text, 200) + "`"
		if !c.at.IsZero() {
			line = c.at.Local().Format(requestStamp) + " " + line
		}
		if c.result != "" {
			line += " → " + c.result
		}
		recalls = append(recalls, line+" ["+c.source+"]")
	}
	section("기억 명령", recalls)
	fmt.Fprintf(&b, "\n## 소스\n- 원본: `%s`\n- 세션: `%s` (%s)\n", s.path, s.id, s.cwd)
	return b.String()
}

var (
	scriptTool    = regexp.MustCompile(`\btools\.(\w+)\(`)
	scriptCommand = regexp.MustCompile(`(?:\bcmd|"cmd"|'cmd')\s*:\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'|` + "`[^`]*`" + `)`)
	scriptWorkdir = regexp.MustCompile(`(?:\bworkdir|"workdir"|'workdir')\s*:\s*("(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*')`)
)

type scriptCall struct{ cmd, workdir string }

// scriptLiteral is a string as the script spells it, quotes removed.
func scriptLiteral(quoted string) string {
	if text, err := strconv.Unquote(quoted); err == nil && quoted[0] == '"' {
		return text
	}
	return quoted[1 : len(quoted)-1]
}

// scriptCommands are the tool calls a Codex app script makes, in order. Results are numbered by
// place, so a call that is not exec_command, or whose command is not written out, keeps its place empty.
func scriptCommands(script string) []scriptCall {
	calls := scriptTool.FindAllStringSubmatchIndex(script, -1)
	var out []scriptCall
	for i, call := range calls {
		end := len(script)
		if i+1 < len(calls) {
			end = calls[i+1][0]
		}
		var c scriptCall
		if m := scriptCommand.FindStringSubmatch(script[call[1]:end]); m != nil && script[call[2]:call[3]] == "exec_command" {
			c.cmd = scriptLiteral(m[1])
			if w := scriptWorkdir.FindStringSubmatch(script[call[1]:end]); w != nil {
				c.workdir = scriptLiteral(w[1])
			}
		}
		out = append(out, c)
	}
	return out
}

// keepOutput cuts a long output, keeping past the cut the lines that say what git or gh made.
func keepOutput(output string) string {
	kept := cut(output, 4000)
	if kept == output {
		return output
	}
	rest := string([]rune(output)[4000:])
	if made := madeLine.FindAllString(rest, -1); len(made) > 0 {
		kept += "\n" + strings.Join(made, "\n")
	}
	return kept
}

type scriptResult struct {
	index  int
	failed bool
	output string
}

// readScriptResult reads one result, with or without allSettled's status wrapper or a "result" key.
func readScriptResult(index int, item any) (scriptResult, bool) {
	r, _ := item.(map[string]any)
	if inner, ok := r["result"].(map[string]any); ok {
		r = inner
	}
	failed := r["status"] == "rejected"
	if value, ok := r["value"].(map[string]any); ok {
		r = value
	}
	output, hasOutput := r["output"].(string)
	code, hasCode := r["exit_code"].(float64)
	return scriptResult{index, failed || (hasCode && code != 0), output}, hasOutput || hasCode || failed
}

// scriptResults are the results a Codex app script prints for its calls. The agent writes each
// script, so results come numbered ("index" or "i", one a line), as one array in order, or printed
// one block a call; anything else is left unread rather than guessed.
func scriptResults(v any, calls int) []scriptResult {
	items, _ := v.([]any)
	var numbered []scriptResult
	var blocks []string
	for _, item := range items {
		block, _ := item.(map[string]any)
		text, _ := block["text"].(string)
		if strings.HasPrefix(text, "Script ") && strings.Contains(text, "Wall time") {
			continue
		}
		blocks = append(blocks, text)
		for _, line := range strings.Split(text, "\n") {
			var parsed any
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &parsed) != nil {
				continue
			}
			switch got := parsed.(type) {
			case []any:
				for i, each := range got {
					if r, ok := readScriptResult(i, each); ok {
						numbered = append(numbered, r)
					}
				}
			case map[string]any:
				index, ok := got["index"].(float64)
				if !ok {
					index, ok = got["i"].(float64)
				}
				if r, read := readScriptResult(int(index), got); ok && read {
					numbered = append(numbered, r)
				}
			}
		}
	}
	if len(numbered) > 0 || len(blocks) != calls {
		return numbered
	}
	var inOrder []scriptResult
	for i, text := range blocks {
		var parsed any
		if json.Unmarshal([]byte(strings.TrimSpace(text)), &parsed) == nil {
			if r, ok := readScriptResult(i, parsed); ok {
				inOrder = append(inOrder, r)
				continue
			}
		}
		inOrder = append(inOrder, scriptResult{i, false, text})
	}
	return inOrder
}

// commitSubjects are the -m subjects of the commits a command runs. An unquoted one ends where the
// shell ends the word, so "-m init && cd x" is "init".
func commitSubjects(command string) []string {
	var out []string
	given := map[int]bool{} // where a commit with -m starts
	for _, m := range commitMessage.FindAllStringSubmatchIndex(command, -1) {
		subject := command[m[2]:m[3]]
		if before := command[m[2]-1]; before != '"' && before != '\'' && before != '\n' {
			if end := strings.IndexAny(subject, " \t;&|)"); end >= 0 {
				subject = subject[:end]
			}
		}
		given[m[0]] = true
		out = append(out, strings.TrimSpace(subject))
	}
	for _, m := range commitFile.FindAllStringSubmatchIndex(command, -1) {
		if !given[m[0]] {
			if subject := heredocSubject(command, m[1], strings.Trim(command[m[2]:m[3]], `'"`)); subject != "" {
				out = append(out, subject)
			}
		}
	}
	return out
}

// heredocSubject is the first line of the heredoc that gives a commit ending at end its message:
// one on the commit's own line when it reads its input, else one written to its message file.
func heredocSubject(command string, end int, file string) string {
	for _, m := range heredoc.FindAllStringSubmatchIndex(command, -1) {
		lineStart := strings.LastIndexByte(command[:m[0]], '\n') + 1
		nl := strings.IndexByte(command[m[1]:], '\n')
		if nl < 0 {
			break
		}
		head := command[lineStart : m[1]+nl]
		if file == "-" && (m[0] < end || strings.Contains(command[end:m[0]], "\n")) ||
			file != "-" && !strings.Contains(strings.ReplaceAll(head, " ", ""), ">"+file) {
			continue
		}
		word := command[m[2]:m[3]]
		for _, line := range strings.Split(command[m[1]+nl+1:], "\n") {
			if strings.TrimLeft(line, "\t") == word {
				break
			}
			if line = strings.TrimSpace(line); line != "" {
				return line
			}
		}
	}
	return ""
}

// unquoted drops what is inside quotes: there the words are an argument, like a pattern handed to
// grep, not a command run.
func unquoted(shell string) string {
	var b strings.Builder
	var quote rune
	escaped := false
	for _, r := range shell {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// shellOnly drops heredoc bodies from a command: they are text handed to another program, so a
// lookup written inside one is not run.
func shellOnly(text string) string {
	var b strings.Builder
	for {
		m := heredoc.FindStringSubmatchIndex(text)
		if m == nil {
			break
		}
		nl := strings.IndexByte(text[m[1]:], '\n')
		if nl < 0 {
			break
		}
		b.WriteString(text[:m[1]+nl+1])
		word, body := text[m[2]:m[3]], text[m[1]+nl+1:]
		text = ""
		for off := 0; off < len(body); {
			line, _, _ := strings.Cut(body[off:], "\n")
			if strings.TrimLeft(line, "\t") == word {
				text = body[off:]
				break
			}
			off += len(line) + 1
		}
	}
	return b.String() + text
}

// recallResult is how a memory lookup came back. An empty answer is the sign a question was put
// in words the records do not hold.
func recallResult(failed bool, output string) string {
	switch {
	case failed || strings.Contains(output, "command not found") || strings.Contains(output, "Failed to execute command") || strings.Contains(output, "use: recall"):
		return "오류"
	case strings.Contains(output, "찾은 기록 없음") || strings.Contains(output, "요청이 없다:") || strings.Contains(output, "구간 0:0 / 0"):
		return "빈 결과"
	}
	return "결과 있음"
}

// lastActive is the last timestamp in the final 64KB of a transcript: when the session last did anything.
func lastActive(path string) time.Time {
	at, _ := lastActiveChecked(path)
	return at
}

func lastActiveChecked(path string) (time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return time.Time{}, err
	}
	tail := make([]byte, min(64<<10, info.Size()))
	if _, err := f.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
		return time.Time{}, err
	}
	all := timestamp.FindAllSubmatch(tail, -1)
	for i := len(all) - 1; i >= 0; i-- {
		if at := stamp(string(all[i][1])); !at.IsZero() {
			return at, nil
		}
	}
	return info.ModTime(), nil
}

// Dir holds recall's own state: the model work folder and the daily model count.
func Dir() string { return filepath.Join(filepath.Dir(RawDir()), "recall") }

// RecordDir holds the dated records: 기록/YYYY-MM-DD/<시각>-<레포>-<세션 8자리>.md.
func RecordDir() string { return filepath.Join(Dir(), recordFolder) }

// existing uses the full raw path, never a short filename prefix that can collide.
func existing() (map[string]record, error) {
	records, errs := loadRecords()
	if len(errs) > 0 {
		return nil, errs[0]
	}
	out := map[string]record{}
	for _, rec := range records {
		if raw := rec.field("raw"); raw != "" {
			out[raw] = rec
		}
	}
	return out, nil
}

// header reads a single front-matter field; summary_data is JSON rather than parsed prose.
// It stops at the first heading instead of splitting the whole file.
func header(raw []byte, key string) string {
	for line := range bytes.Lines(raw) {
		line = bytes.TrimSuffix(line, []byte("\n"))
		if bytes.HasPrefix(line, []byte("#")) {
			break
		}
		if v, ok := bytes.CutPrefix(line, []byte(key+": ")); ok {
			return string(v)
		}
	}
	return ""
}

func summaryDiagnostics(raw []byte) (string, *summaryAttempt) {
	message := header(raw, "summary_error")
	if decoded, err := strconv.Unquote(message); err == nil {
		message = decoded
	}
	if header(raw, "summary_state") == "failed" && message == "" {
		message = "이전 요약 실패; 원인 미확인"
	}
	var attempt summaryAttempt
	if data := header(raw, "last_summary_attempt"); data != "" {
		if json.Unmarshal([]byte(data), &attempt) != nil {
			return "이전 요약 시도 상태 해석 실패; 진단 미확인", nil
		}
		switch attempt.Outcome {
		case "success", "start", "timeout", "exit", "response", "execution", "range", "unknown":
		default:
			return "이전 요약 시도 상태 해석 실패; 진단 미확인", nil
		}
		if attempt.Cursor < 0 || (attempt.Outcome == "exit" && attempt.ExitCode == nil) {
			return "이전 요약 시도 상태 해석 실패; 진단 미확인", nil
		}
		return mask(message), &attempt
	}
	if message != "" && header(raw, "summary_state") == "failed" {
		// Old failed records know the input and cursor, but not the attempt time or cause.
		cursor, _ := strconv.Atoi(header(raw, "summary_cursor"))
		return mask(message), &summaryAttempt{RawSHA: header(raw, "raw_sha256"), Cursor: cursor, Outcome: "unknown"}
	}
	return mask(message), nil
}

// Build records the conversations that changed, keeping each one's last model summary as it was.
func Build(since time.Time, now time.Time) (map[string]any, error) {
	return buildWithRecordWrite(since, now, memory.PrivateWrite, "")
}

// BuildConversation records one archived conversation now, as its Stop hook keeps it: the next start
// card and the ledger need not wait for the next session's sweep.
func BuildConversation(raw string, now time.Time) (map[string]any, error) {
	return buildWithRecordWrite(time.Time{}, now, memory.PrivateWrite, raw)
}

// The write seam exercises failures both before and after atomic replacement. only, when set, is
// the one archived transcript to record.
func buildWithRecordWrite(since time.Time, now time.Time, writeRecord func(string, []byte) error, only string) (map[string]any, error) {
	if os.Getenv("HARNESS_RECALL") == "1" {
		return nil, nil
	}
	lock, err := exclusive(filepath.Join(Dir(), ".build.lock"))
	if lock == nil {
		return map[string]any{"skipped": "another build is running"}, err
	}
	defer lock.Close()
	have, err := existing()
	if err != nil {
		return nil, err
	}
	type item struct {
		tool, path string
		at         time.Time
	}
	type priorRecord struct {
		tool    string
		size    int64
		written time.Time
		pending bool
	}
	prior := map[string]priorRecord{}
	for path, rec := range have {
		size, _ := strconv.ParseInt(rec.field("raw_bytes"), 10, 64)
		// A summary an older build left pending is turned off by the next look.
		state := rec.field("summary_state")
		pending := state != "ready" && state != "off"
		prior[path] = priorRecord{tool: rec.field("tool"), size: size, written: rec.mod, pending: pending}
	}
	var items []item
	var discoveryFailed []string
	seen := map[string]bool{}
	add := func(tool, path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		at, err := lastActiveChecked(path)
		if err != nil {
			discoveryFailed = append(discoveryFailed, path+": "+err.Error())
			return
		}
		p, known := prior[path]
		if at.Before(since) && !p.pending {
			// An old ready session can receive an append or a rewrite without a new
			// timestamp. Stat changes make it a candidate; its full hash decides whether
			// there is anything to rebuild. Old unchanged ready records stay untouched.
			if !known {
				return
			}
			info, err := os.Stat(path)
			if err != nil {
				discoveryFailed = append(discoveryFailed, path+": "+err.Error())
				return
			}
			if info.Size() == p.size && !info.ModTime().After(p.written) {
				return
			}
		}
		items = append(items, item{tool: tool, path: path, at: at})
	}
	if only != "" {
		// raw/<tool>/<day>/<file>
		add(filepath.Base(filepath.Dir(filepath.Dir(only))), only)
	}
	for _, tool := range []string{"claude", "codex"} {
		if only != "" {
			break
		}
		days, err := os.ReadDir(filepath.Join(RawDir(), tool))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, day := range days {
			if _, err := time.Parse("2006-01-02", day.Name()); err != nil {
				continue
			}
			folder := filepath.Join(RawDir(), tool, day.Name())
			files, err := os.ReadDir(folder)
			if err != nil {
				discoveryFailed = append(discoveryFailed, folder+": "+err.Error())
				continue
			}
			for _, file := range files {
				if strings.HasSuffix(file.Name(), ".jsonl") {
					add(tool, filepath.Join(folder, file.Name()))
				}
			}
		}
	}
	// Old pending records still need retries after they leave the discovery window.
	for path, p := range prior {
		if !seen[path] && p.pending && only == "" {
			if p.tool == "claude" || p.tool == "codex" {
				add(p.tool, path)
			}
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.After(items[j].at) })
	skipPath := filepath.Join(Dir(), fmt.Sprintf("not-sessions-%d.json", recordFormat))
	skip := map[string]string{}
	if raw, err := os.ReadFile(skipPath); err == nil {
		if err = json.Unmarshal(raw, &skip); err != nil {
			return nil, fmt.Errorf("정상 제외 캐시 해석 실패: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	written, failed, retired := []string{}, discoveryFailed, []string{}
	var git *gitCache // one for the whole build: conversations share repositories
	var facts *factWriter
	var factsErr error
	storageUncertain := false
	for _, it := range items {
		info, err := os.Stat(it.path)
		if err != nil {
			failed = append(failed, it.path+": "+err.Error())
			continue
		}
		hash, err := prefixHash(it.path, info.Size())
		if err != nil {
			failed = append(failed, it.path+": "+err.Error())
			continue
		}
		if skip[it.path] == hash {
			continue
		}
		key := short(strings.TrimSuffix(filepath.Base(it.path), ".jsonl"))
		prior := have[it.path]
		prev := prior.path
		old, hadSummary := prior.summary()
		if data := prior.field("summary_data"); data != "" {
			var stored summary
			if json.Unmarshal([]byte(data), &stored) != nil || stored.Title == "" {
				failed = append(failed, prev+": 기억 JSON해석 실패; 기존 파일 유지")
				continue
			}
		}
		changed := prior.field("raw_sha256") != hash
		stale := prior.field("format") != strconv.Itoa(recordFormat)
		cursor, _ := strconv.Atoi(prior.field("summary_cursor"))
		if state := prior.field("summary_state"); !changed && !stale && (state == "ready" || state == "off") {
			continue
		}
		parsed, ok, err := readSessionChecked(it.tool, it.path)
		if err != nil {
			failed = append(failed, it.path+": "+err.Error())
			continue
		}
		if !ok {
			skip[it.path] = hash
			// The reader no longer finds the person's words here, as in a transcript of /clear alone.
			// A record made from the transcript alone would only clutter search; a model summary stays.
			if prev != "" && prior.field("summary_data") == "" && os.Remove(prev) == nil {
				retired = append(retired, prev)
			}
			continue
		}
		if parsed.hash != hash {
			failed = append(failed, it.path+": 수집 중 원본 변경; 재시도 대기")
			continue
		}
		total := len(fragments(parsed))
		// Verify the already-processed request prefix before reusing its cursor after a rewrite.
		if cursor > total || prior.field("summary_prefix") != requestPrefix(parsed, cursor) {
			cursor = 0
		}
		parsed.cursor = cursor
		parsed.summaryError, parsed.summaryAttempt = summaryDiagnostics(prior.raw)
		parsed.summaryState = "ready"
		if !hadSummary || cursor < total || changed {
			parsed.summaryState = "pending"
		}
		if !changed && parsed.summaryError != "" {
			parsed.summaryState = "failed"
		}
		if parsed.summaryState != "ready" {
			// The fixed fields are the record now. Nothing waits on a model, so its past failures
			// are no longer pending work; a complete model summary stays as it was.
			parsed.summaryState, parsed.summaryError, parsed.summaryAttempt = "off", "", nil
		}
		m := mergeSummary(parsed, old, summary{})
		if git == nil {
			git = newGitCache()
		}
		git.readGit(&parsed)
		dest := prev
		if dest == "" {
			dest = filepath.Join(RecordDir(), parsed.start.Local().Format("2006-01-02"), parsed.start.Local().Format("1504")+"-"+RepoName(parsed.cwd)+"-"+key+".md")
			if raw, err := os.ReadFile(dest); err == nil && header(raw, "raw") != it.path {
				pathHash := sha256.Sum256([]byte(it.path))
				dest = strings.TrimSuffix(dest, ".md") + fmt.Sprintf("-%x.md", pathHash[:8])
			} else if err != nil && !os.IsNotExist(err) {
				failed = append(failed, dest+": "+err.Error())
				continue
			}
		}
		if err = writeRecord(dest, []byte(render(parsed, m, hadSummary))); err != nil {
			storageUncertain = true
			failed = append(failed, dest+": 기억 저장 실패; 저장 결과 미확인")
			continue
		}
		if facts == nil && factsErr == nil {
			facts, factsErr = openFacts(now)
		}
		if factsErr == nil {
			factsErr = facts.add(parsed)
		}
		written = append(written, dest)
		have[it.path] = record{path: dest}
	}
	if factsErr != nil {
		// The records stand without it; `harness recall facts --backfill` fills the ledger again.
		failed = append(failed, factsPath()+": 사실 장부 갱신 실패: "+factsErr.Error())
	}
	if err = memory.PrivateWrite(skipPath, memory.Packed(skip)); err != nil {
		return nil, err
	}
	var pending, unresolved any
	if storedPending, storedUnresolved := storedSummaryCounts(); storedPending != nil {
		pending, unresolved = *storedPending, *storedUnresolved
	}
	return map[string]any{"written": len(written), "unresolved": unresolved, "pending": pending, "failed": failed, "files": written, "retired": retired, "storage_uncertain": storageUncertain}, nil
}
