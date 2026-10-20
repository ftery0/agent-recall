package worklog

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type note struct {
	title  string
	folder string
	status string
	next   string
	prs    []string // pull requests the note mentions
	mod    time.Time
}

type repo struct {
	root    string
	names   []string
	git     bool
	company bool
}

// Request is one thing the person typed or selected. Line and ID refer to its raw event;
// retaining repeated text is necessary because the same answer can decide different things.
type Request struct {
	At         time.Time
	Text       string
	ID         string
	Line       int
	Source     string
	QuestionID string
	Context    string // preceding agent message, a claim that supplies context rather than authority
}

type transcript struct {
	requests []Request
	reports  []string
	saved    time.Time // last time this session wrote a work note
}

var (
	nextItem  = regexp.MustCompile(`(?m)^## 다음[ \t]*\n(?:[ \t]*\n)*[ \t]*(?:[-*]|\d+\.)[ \t]*(.+)$`)
	repoAside = regexp.MustCompile(`\(.*\)`)
)

func VaultDir() string {
	if v := os.Getenv("HARNESS_VAULT_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Documents", "Obsidian")
}

func dailyDir() string {
	if v := os.Getenv("HARNESS_DAILY_DIR"); v != "" {
		return v
	}
	return filepath.Join(VaultDir(), "company", "00_기록", "데일리")
}

const gitWait = 3 * time.Second

// Git runs git in dir within limit and gives its output without the final newline, or false
// when git failed or ran too long. The start card, the notes and recall's checks all call git here.
// An empty dir is refused: git -C with an empty path reads whatever repository this process runs in.
func Git(dir string, limit time.Duration, args ...string) (string, bool) {
	if dir == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(raw), "\n"), true
}

// RepoRoot is the top folder of the git repository holding dir, or "" outside one. It looks for the
// ".git" folder, or a worktree's ".git" file, itself: the start card asks this of every recorded
// folder, and a git process for each took most of the card's time.
func RepoRoot(dir string) string {
	if dir == "" {
		return ""
	}
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// OriginName is the repository name in dir's origin URL, or "" without one.
func OriginName(dir string) string {
	if origin, _ := Git(dir, gitWait, "remote", "get-url", "origin"); origin != "" {
		return originName(origin)
	}
	return ""
}

func originName(url string) string {
	return strings.TrimSuffix(filepath.Base(strings.TrimRight(url, "/")), ".git")
}

func findRepo(cwd string) repo {
	r := repo{root: cwd}
	if top := RepoRoot(cwd); top != "" {
		r.root, r.git = top, true
	}
	r.names = []string{strings.ToLower(filepath.Base(r.root))}
	if origin, _ := Git(r.root, gitWait, "remote", "get-url", "origin"); origin != "" {
		r.company = companyOrigin(origin)
		r.names = append(r.names, strings.ToLower(originName(origin)))
	}
	return r
}

// companyOrigin says a remote belongs to one of the organizations named in HARNESS_COMPANY_ORGS
// (comma separated); their work notes live under the vault's company/ folder.
func companyOrigin(origin string) bool {
	for _, org := range strings.Split(os.Getenv("HARNESS_COMPANY_ORGS"), ",") {
		if org = strings.TrimSpace(org); org != "" && (strings.Contains(origin, "/"+org+"/") || strings.Contains(origin, ":"+org+"/")) {
			return true
		}
	}
	return false
}

// NoteRoots are the only folders work notes live in; everything else in the vault is reference material.
func NoteRoots(vault string) []string {
	roots := []string{filepath.Join(vault, "company", "30_자료", "작업 기록")}
	personal, _ := filepath.Glob(filepath.Join(vault, "personal", "10_프로젝트", "*", "작업 기록"))
	return append(roots, personal...)
}

func findNotes(vault string, r repo) []note {
	var notes []note
	for _, root := range NoteRoots(vault) {
		rel, err := filepath.Rel(vault, root)
		if err != nil || strings.HasPrefix(rel, "company"+string(filepath.Separator)) != r.company {
			continue
		}
		filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || filepath.Ext(path) != ".md" || strings.HasPrefix(d.Name(), "_") {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			status, repos := frontMatter(string(raw))
			if status == "" || !matches(repos, r.names) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			n := note{title: strings.TrimSuffix(d.Name(), ".md"), status: status, mod: info.ModTime()}
			n.folder, _ = filepath.Rel(vault, filepath.Dir(path))
			if m := nextItem.FindStringSubmatch(string(raw)); m != nil {
				n.next = strings.TrimSpace(m[1])
			}
			n.prs = notePRs(string(raw), r.root)
			notes = append(notes, n)
			return nil
		})
	}
	sort.SliceStable(notes, func(i, j int) bool { return notes[i].mod.After(notes[j].mod) })
	return notes
}

func frontMatter(text string) (string, []string) {
	if !strings.HasPrefix(text, "---\n") {
		return "", nil
	}
	end := strings.Index(text[4:], "\n---")
	if end < 0 {
		return "", nil
	}
	status, repos, inRepos := "", []string{}, false
	for _, line := range strings.Split(text[4:4+end], "\n") {
		key, value, found := strings.Cut(line, ":")
		switch {
		case inRepos && strings.HasPrefix(strings.TrimSpace(line), "- "):
			repos = append(repos, strings.TrimPrefix(strings.TrimSpace(line), "- "))
		case found && key == "status":
			status, inRepos = strings.TrimSpace(value), false
		case found && key == "repos":
			inRepos = strings.TrimSpace(value) == ""
			repos = append(repos, strings.Split(value, ",")...)
		default:
			inRepos = false
		}
	}
	return status, repos
}

func matches(repos, names []string) bool {
	for _, item := range repos {
		item = strings.ToLower(strings.Trim(strings.TrimSpace(repoAside.ReplaceAllString(item, "")), "`'\""))
		for _, name := range names {
			if item != "" && item == name {
				return true
			}
		}
	}
	return false
}

// writesNote spots a tool call that writes a work note, so saves are judged per session
// instead of by any note's mtime in the repo.
func writesNote(call any) bool {
	raw, _ := json.Marshal(call)
	text := string(raw)
	if !strings.Contains(text, "/작업 기록/") {
		return false
	}
	for _, sign := range []string{`"name":"Edit"`, `"name":"Write"`, "write", "apply_patch", "*** Update File", "*** Add File", ">"} {
		if strings.Contains(text, sign) {
			return true
		}
	}
	return false
}

func stamp(v any) time.Time {
	s, _ := v.(string)
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// humanPrompt is a Claude row the person sent: typed, picked from a suggestion, or queued while
// the AI worked. "sdk" rows are scripted runs, except in the VS Code extension, which records what
// the person typed as "sdk" and leaves its commands unmarked. Newer rows also name their origin,
// and a task notification there is not the person. Claude Code before 2.1.220 did not mark rows
// at all, so an unmarked row from those versions counts as typed.
func humanPrompt(row map[string]any) bool {
	if row["isMeta"] == true {
		return false
	}
	if origin, marked := row["origin"].(map[string]any); marked && origin["kind"] != "human" {
		return false
	}
	switch row["promptSource"] {
	case "typed", "queued", "suggestion_accepted":
		return true
	case "sdk":
		return row["entrypoint"] == "claude-vscode"
	case nil:
		if row["entrypoint"] == "claude-vscode" {
			return true
		}
		version, _ := row["version"].(string)
		var major, minor, patch int
		if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
			return false
		}
		return (major*1000+minor)*1000+patch < 2001220
	}
	return false
}

// messageText preserves the person's formatting. Display limits belong to the card, not the reader.
func messageText(v any) string {
	if text, ok := v.(string); ok {
		return strings.TrimSpace(text)
	}
	var parts []string
	items, _ := v.([]any)
	for _, item := range items {
		block, _ := item.(map[string]any)
		if text, ok := block["text"].(string); ok && (block["type"] == "text" || block["type"] == "input_text" || block["type"] == "output_text") {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// injectedPrompt spots text a tool or app wrote into the person's turn. A command block left as
// recorded is a bare command or a built-in such as /model; a skill call with arguments has already
// become "/name arguments" by then.
func injectedPrompt(text string) bool {
	for _, prefix := range []string{"# AGENTS.md instructions", "[Request interrupted", "<environment_context", "<permissions", "<system-reminder", "<agent-report", "<agent_report", "<subagent_notification", "<turn_aborted", "<collaboration", "<skills_instructions", "<app-context", "<user_instructions", "<developer_instructions", "<INSTRUCTIONS>", "<recommended_plugins", "<external_codex_apps_open_page", "<codex_internal_context", "<hook_prompt", "<skill>", "<task-notification", "<local-command-stdout", "<local-command-stderr", "<command-name>", "<command-message>", "<in-app-browser-context", "<heartbeat", "<codex_delegation", "This session is being continued from a previous conversation", "You are performing a CONTEXT CHECKPOINT COMPACTION", "A different assistant started", "Another language model started"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

// slashCommand is a skill call written back as the person typed it, "/name arguments", from
// Claude Code's record of it. A command without arguments gives "".
func slashCommand(text string) string {
	if !strings.HasPrefix(text, "<command-name>") && !strings.HasPrefix(text, "<command-message>") {
		return ""
	}
	inner := func(tag string) string {
		_, rest, _ := strings.Cut(text, "<"+tag+">")
		value, _, _ := strings.Cut(rest, "</"+tag+">")
		return strings.TrimSpace(value)
	}
	name, args := inner("command-name"), inner("command-args")
	if name == "" || args == "" {
		return ""
	}
	return name + " " + args
}

// requestText keeps the person's words from what an app put before them: Claude's VS Code
// extension adds the open file or selection, and Codex lists attachments or the browser or IDE
// state, then puts the words under a "## My request" line. Excerpts are cut short, so a head
// left in place hides the request. With nothing under that line the words are in an attached
// file, and the head naming it is all there is.
func requestText(text string) string {
	for _, tag := range []string{"ide_opened_file", "ide_selection"} {
		if strings.HasPrefix(text, "<"+tag+">") {
			if _, rest, ok := strings.Cut(text, "</"+tag+">"); ok {
				return requestText(strings.TrimSpace(rest))
			}
		}
	}
	for _, head := range []string{"# Files mentioned by the user", "# Files pasted by the user", "# Context from my IDE setup", "# In app browser", "<in-app-browser-context"} {
		if !strings.HasPrefix(text, head) {
			continue
		}
		for _, marker := range []string{"\n## My request for Codex:", "\n## My request:"} {
			if _, rest, ok := strings.Cut(text, marker); ok && strings.TrimSpace(rest) != "" {
				return strings.TrimSpace(rest)
			}
		}
	}
	return text
}

type codexAnswer struct {
	call, question, text string
	index                int
}

// codexAnswers reads the person's reply to Codex's question tool, naming each question as the tool
// call asked it, or as the reply quotes it when the call is not in the file. It gives nil, which
// leaves the message a plain request, when the reply is not in this form.
func codexAnswers(text string, questions map[string][]string) []codexAnswer {
	body, ok := strings.CutPrefix(text, "<send_user_message_question_reply>")
	if !ok {
		return nil
	}
	body, after, ok := strings.Cut(body, "</send_user_message_question_reply>")
	if !ok || strings.TrimSpace(after) != "" {
		return nil
	}
	var items []struct {
		QuestionItemID string `json:"questionItemId"`
		Question       string `json:"question"`
		Answer         string `json:"answer"`
	}
	if json.Unmarshal([]byte(body), &items) != nil {
		return nil
	}
	var answers []codexAnswer
	for _, item := range items {
		a := codexAnswer{call: item.QuestionItemID, question: item.Question, text: strings.TrimSpace(item.Answer)}
		// The item ID is a JSON list: the tool's name, its call ID and the question's index.
		var ref []any
		if json.Unmarshal([]byte(item.QuestionItemID), &ref) == nil && len(ref) == 3 {
			call, _ := ref[1].(string)
			index, _ := ref[2].(float64)
			a.call, a.index = cmp.Or(call, a.call), int(index)
			if asked := questions[call]; a.index >= 0 && a.index < len(asked) && asked[a.index] != "" {
				a.question = asked[a.index]
			}
		}
		if a.call == "" || a.question == "" {
			return nil
		}
		if a.text != "" {
			answers = append(answers, a)
		}
	}
	return answers
}

// turn places a Claude row in its conversation: each row names the row before it.
type turn struct {
	parent string
	line   int
	user   bool
}

// liveRequests drops what the person took back. Editing a sent message, or withdrawing it to run a
// command, starts a new branch from its parent and leaves the old one in the file. The branch that
// reaches the last row is the conversation, but a request off it goes only when its branch began
// with the person's message and a later row took that place: compaction and resume also leave
// branches, and the person did send what is on them. When the chain is cut, only a later user row
// under the same parent shows a replacement.
func liveRequests(requests []Request, turns map[string]turn, rowAt map[int]string, prompts map[string]bool, leaf string) []Request {
	next := map[string]string{} // row on the live branch -> the row after it there
	row, child := leaf, ""
	for row != "" {
		current, known := turns[row]
		if _, seen := next[row]; seen || !known {
			break
		}
		next[row] = child
		row, child = current.parent, row
	}
	complete := row == ""
	latest := map[string]string{} // parent -> its last user row, used when the chain is cut
	if !complete {
		for id, t := range turns {
			if t.user && t.line > turns[latest[t.parent]].line {
				latest[t.parent] = id
			}
		}
	}
	// branch is the first row of row's branch off the live one, or "" when it never joins it.
	branch := func(row string) string {
		for range len(turns) {
			parent := turns[row].parent
			if _, live := next[parent]; live {
				return row
			}
			if _, known := turns[parent]; !known {
				return ""
			}
			row = parent
		}
		return ""
	}
	kept := requests[:0]
	for _, r := range requests {
		row := rowAt[r.Line]
		withdrawn := false
		if _, live := next[row]; !live && row != "" {
			if complete {
				start := branch(row)
				withdrawn = start != "" && prompts[start] && turns[next[turns[start].parent]].line > turns[start].line
			} else {
				parent := turns[row].parent
				withdrawn = prompts[row] && parent != "" && latest[parent] != row
			}
		}
		if !withdrawn {
			kept = append(kept, r)
		}
	}
	return kept
}

// readTranscript reads both formats: Claude rows carry message/usage, Codex rows carry payload.
// Cards can use the readable portion; durable recording uses ReadConversation and retries errors.
func readTranscript(path string) transcript {
	t, _ := readTranscriptChecked(path)
	return t
}

func readTranscriptChecked(path string) (transcript, error) {
	f, err := os.Open(path)
	if err != nil {
		return transcript{}, err
	}
	defer f.Close()
	return readTranscriptReader(f)
}

func readTranscriptReader(reader io.Reader) (transcript, error) {
	var t transcript
	var readErrors []error
	humanTurn := false
	lastAssistant := ""
	seen := map[string]string{}
	type question struct {
		text      []string
		assistant string
	}
	questions := map[string]question{}
	codexQuestions := map[string][]string{} // call ID -> the questions Codex asked in it
	codexIDs := map[string][]string{}       // call ID -> those questions' IDs, which name the answers in its output
	turns := map[string]turn{}
	rowAt := map[int]string{}    // line -> its Claude row
	prompts := map[string]bool{} // Claude rows holding the person's own message
	leaf, caveat := "", ""       // the last Claude row; the last caveat before a built-in command
	request := func(at time.Time, text, id, source, questionID string, line int) {
		if text == "" || injectedPrompt(text) {
			return
		}
		if previous, exists := seen[id]; exists {
			if previous != text {
				readErrors = append(readErrors, fmt.Errorf("read transcript line %d: conflicting human request ID", line))
			}
			return
		}
		seen[id] = text
		r := Request{At: at, Text: text, ID: id, Line: line, Source: source, QuestionID: questionID}
		if questionID == "" {
			r.Context = lastAssistant
		}
		t.requests = append(t.requests, r)
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			readErrors = append(readErrors, fmt.Errorf("read transcript line %d: %w", line, err))
			continue
		}
		if row["isSidechain"] == true || row["agent_id"] != nil || row["agentId"] != nil {
			continue
		}
		at := stamp(row["timestamp"])
		message, _ := row["message"].(map[string]any)
		payload, _ := row["payload"].(map[string]any)
		id, _ := row["uuid"].(string)
		if id == "" {
			id, _ = payload["id"].(string)
		}
		if id == "" {
			id, _ = row["id"].(string)
		}
		if id == "" {
			id = fmt.Sprintf("line:%d", line)
		}
		if uuid, _ := row["uuid"].(string); uuid != "" {
			parent, _ := row["parentUuid"].(string)
			if parent == "" {
				parent, _ = row["logicalParentUuid"].(string) // a compaction boundary keeps its link here
			}
			// A rewritten row replaces the earlier copy, as Claude Code itself reads the file.
			turns[uuid] = turn{parent, line, row["type"] == "user"}
			rowAt[line], leaf = uuid, uuid
		}
		switch {
		case row["type"] == "system" && row["subtype"] == "compact_boundary":
			lastAssistant = ""
		case row["isCompactSummary"] == true:
			lastAssistant = ""
		case row["type"] == "user":
			text := messageText(message["content"])
			if strings.HasPrefix(text, "<local-command-caveat") {
				caveat = id
			}
			// A built-in command such as /model comes right after its caveat row and stays a
			// command block, which is not a request; a skill call reads as the person typed it.
			command := slashCommand(text)
			if caveat != "" && row["parentUuid"] == caveat {
				command = ""
			}
			if command != "" {
				text = command
			}
			text = requestText(text)
			scripted := row["promptSource"] == "sdk" && row["entrypoint"] != "claude-vscode"
			// Commands carry no prompt source, and in the terminal only the person types them.
			human := humanPrompt(row) || command != "" && row["entrypoint"] == "cli"
			if text != "" {
				if human && !injectedPrompt(text) {
					humanTurn = true
					prompts[id] = true
					source, _ := row["promptSource"].(string)
					switch {
					case command != "":
						source = "command"
					case row["entrypoint"] == "claude-vscode":
						source = "vscode"
					case source == "":
						source = "legacy"
					}
					request(at, text, id, "claude:"+source, "", line)
				} else {
					lastAssistant = ""
					if scripted {
						humanTurn = false
					}
				}
			} else if scripted || row["isMeta"] == true {
				lastAssistant = ""
				if scripted {
					humanTurn = false
				}
			}
			// UI answers are tool results, rather than typed prompts. Trust the structured answers
			// only when their tool_use_id belongs to a question from the preceding human turn.
			result, _ := row["toolUseResult"].(map[string]any)
			answers, _ := result["answers"].(map[string]any)
			blocks, _ := message["content"].([]any)
			for _, b := range blocks {
				block, _ := b.(map[string]any)
				use, _ := block["tool_use_id"].(string)
				q, ok := questions[use]
				if !ok || !humanTurn || block["type"] != "tool_result" || block["is_error"] == true || scripted || row["isMeta"] == true {
					continue
				}
				if from, _ := row["sourceToolAssistantUUID"].(string); from != "" && q.assistant != "" && from != q.assistant {
					continue
				}
				for i, text := range q.text {
					var answer string
					switch value := answers[text].(type) {
					case string:
						answer = value
					case []any:
						var values []string
						for _, v := range value {
							if s, ok := v.(string); ok {
								values = append(values, s)
							}
						}
						answer = strings.Join(values, ", ")
					}
					if strings.TrimSpace(answer) != "" {
						request(at, "질문: "+text+"\n선택: "+answer, fmt.Sprintf("%s:%s:%d", id, use, i), "claude:choice", use, line)
					}
				}
			}
		case row["type"] == "assistant":
			if text := messageText(message["content"]); text != "" && humanTurn {
				t.reports = append(t.reports, text)
				lastAssistant = text
			}
			blocks, _ := message["content"].([]any)
			for _, b := range blocks {
				block, _ := b.(map[string]any)
				if block["type"] == "tool_use" && writesNote(block) {
					t.saved = at
				}
				if block["type"] == "tool_use" && block["name"] == "AskUserQuestion" && humanTurn {
					use, _ := block["id"].(string)
					input, _ := block["input"].(map[string]any)
					items, _ := input["questions"].([]any)
					q := question{assistant: id}
					for _, item := range items {
						entry, _ := item.(map[string]any)
						if text, _ := entry["question"].(string); text != "" {
							q.text = append(q.text, text)
						}
					}
					if use != "" && len(q.text) > 0 {
						questions[use] = q
					}
				}
			}
		case row["type"] == "compacted":
			lastAssistant = ""
		case row["type"] == "session_meta":
			if source, ok := payload["source"].(map[string]any); ok && source["subagent"] != nil {
				return transcript{}, errors.Join(readErrors...)
			}
		case row["type"] == "response_item" && (payload["type"] == "function_call" || payload["type"] == "custom_tool_call"):
			if writesNote(payload) {
				t.saved = at
			}
			// Codex asks through a tool; the person's answer comes back later as a user message.
			if name, _ := payload["name"].(string); strings.HasPrefix(name, "request_user_input") {
				var input struct {
					Questions []struct{ ID, Title, Question string }
				}
				arguments, _ := payload["arguments"].(string)
				json.Unmarshal([]byte(arguments), &input)
				var asked, ids []string
				for _, q := range input.Questions {
					asked, ids = append(asked, cmp.Or(q.Title, q.Question)), append(ids, q.ID)
				}
				call, _ := payload["call_id"].(string)
				codexQuestions[call], codexIDs[call] = asked, ids
			}
		case row["type"] == "response_item" && payload["type"] == "function_call_output":
			// The blocking form of the question tool returns the answers as its output, by question ID.
			call, _ := payload["call_id"].(string)
			output, _ := payload["output"].(string)
			var reply struct {
				Answers map[string]struct{ Answers []string }
			}
			if ids := codexIDs[call]; len(ids) > 0 && json.Unmarshal([]byte(output), &reply) == nil {
				for i, qid := range ids {
					if answer := strings.TrimSpace(strings.Join(reply.Answers[qid].Answers, ", ")); answer != "" {
						humanTurn = true
						request(at, "질문: "+codexQuestions[call][i]+"\n선택: "+answer, fmt.Sprintf("%s:%s:%d", id, call, i), "codex:choice", call, line)
					}
				}
			}
		case row["type"] == "response_item" && payload["type"] == "message" && payload["role"] == "assistant":
			if text := messageText(payload["content"]); text != "" && humanTurn {
				t.reports = append(t.reports, text)
				lastAssistant = text
			}
		case row["type"] == "response_item" && payload["type"] == "message" && payload["role"] == "user":
			// Codex stores injected context as user messages too: tagged blocks and the AGENTS.md preamble.
			text := requestText(messageText(payload["content"]))
			if text == "" || injectedPrompt(text) || payload["is_compact_summary"] == true {
				lastAssistant = ""
				continue
			}
			humanTurn = true
			if answers := codexAnswers(text, codexQuestions); answers != nil {
				for _, a := range answers {
					request(at, "질문: "+a.question+"\n선택: "+a.text, fmt.Sprintf("%s:%s:%d", id, a.call, a.index), "codex:choice", a.call, line)
				}
				continue
			}
			request(at, text, id, "codex:user", "", line)
		}
	}
	if err := scanner.Err(); err != nil {
		readErrors = append(readErrors, fmt.Errorf("read transcript after line %d: %w", line, err))
	}
	t.requests = liveRequests(t.requests, turns, rowAt, prompts, leaf)
	return t, errors.Join(readErrors...)
}

// previousTranscript is the session before a new one or /clear: Claude keeps one folder per
// project, Codex keeps one folder per day, so Codex candidates are matched by their recorded cwd.
func previousTranscript(tool, current, cwd string) string {
	if current == "" {
		return ""
	}
	pattern := filepath.Join(filepath.Dir(current), "*.jsonl")
	if tool == "codex" {
		sessions := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(current))))
		pattern = filepath.Join(sessions, "*", "*", "*", "rollout-*.jsonl")
	}
	candidates, _ := filepath.Glob(pattern)
	type file struct {
		path string
		mod  time.Time
	}
	var files []file
	for _, path := range candidates {
		if filepath.Base(path) == filepath.Base(current) {
			continue
		}
		if info, err := os.Stat(path); err == nil {
			files = append(files, file{path, info.ModTime()})
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for i, f := range files {
		if tool != "codex" {
			return f.path
		}
		if i >= 50 {
			break
		}
		if sessionCwd(f.path) == cwd {
			return f.path
		}
	}
	return ""
}

func sessionCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	if !scanner.Scan() {
		return ""
	}
	var row struct {
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	json.Unmarshal(scanner.Bytes(), &row)
	return row.Payload.Cwd
}

// ReadConversation returns people’s requests and agent reports without silently treating unreadable
// or malformed input as an empty conversation. Reports are claims, not verified completion evidence.
func ReadConversation(path string) ([]Request, []string, error) {
	t, err := readTranscriptChecked(path)
	return t.requests, t.reports, err
}
