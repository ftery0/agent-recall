package adapters

import (
	"errors"
	"flag"
	"github.com/ftery0/agent-recall/internal/memory"
	"io"
	"os"
	"strconv"
	"strings"
)

func ReadInput(path string) ([]byte, error) {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		r = f
	}
	raw, e := io.ReadAll(io.LimitReader(r, 64<<20+1))
	if len(raw) > 64<<20 {
		return nil, errors.New("input exceeds 64 MiB")
	}
	return raw, e
}
func MemoryCLI(c Config, args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("memory command required")
	}
	action := args[0]
	args = args[1:]
	f := flag.NewFlagSet(action, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	task := f.String("task", "", "")
	out := f.String("output", "", "")
	tool := f.String("tool", "codex", "")
	session := f.String("session", "", "")
	cwd := f.String("cwd", "", "")
	request := f.String("request", "", "")
	question := f.String("question-id", "", "")
	answer := f.String("answer-file", "", "")
	limit := f.Int("limit", 100, "")
	after := f.Int("after", 0, "")
	view := f.String("view", "", "")
	budget := f.Int("budget", 6000, "")
	offset := f.Int("offset", 0, "")
	maxChars := f.Int("max-chars", 2000, "")
	var positional []string
	var flags []string
	for i := 0; i < len(args); i++ {
		if len(args[i]) > 2 && args[i][:2] == "--" {
			flags = append(flags, args[i])
			if strings.Contains(args[i], "=") {
				return nil, errors.New("use --flag value syntax")
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, errors.New("flag value missing")
			}
			i++
			flags = append(flags, args[i])
		} else {
			positional = append(positional, args[i])
		}
	}
	if e := f.Parse(flags); e != nil {
		return nil, e
	}
	used := map[string]bool{}
	f.Visit(func(v *flag.Flag) { used[v.Name] = true })
	excerpt := action == "source" && (used["offset"] || used["max-chars"])
	if used["budget"] && action != "brief" || used["view"] && (action != "search" || *view != "brief") || used["max-chars"] && action != "source" || used["offset"] && !excerpt && !(action == "search" && *view == "brief") {
		return nil, errors.New("retrieval option is not valid for this command")
	}
	if action == "brief" || *view == "brief" || excerpt {
		allowed := map[string]bool{"task": action != "source", "budget": action == "brief", "view": action == "search", "limit": action == "search", "offset": action != "brief", "max-chars": excerpt}
		for name := range used {
			if !allowed[name] {
				return nil, errors.New("option --" + name + " is not valid for this retrieval")
			}
		}
		wantArgs := 1
		if action == "brief" {
			wantArgs = 0
		}
		if len(positional) != wantArgs {
			return nil, errors.New("invalid number of retrieval arguments")
		}
		if *budget < 1 || *budget > 1000000 || *offset < 0 || *maxChars < 1 || *maxChars > 100000 {
			return nil, errors.New("invalid retrieval budget or page")
		}
		if action == "search" {
			if !used["limit"] {
				*limit = 5
			}
			if *limit < 1 || *limit > 100 || strings.TrimSpace(positional[0]) == "" {
				return nil, errors.New("invalid brief search page or query")
			}
		}
	}
	arg := func() string {
		if len(positional) > 0 {
			return positional[0]
		}
		return ""
	}
	s, e := c.Store(action == "init")
	if e != nil {
		return nil, e
	}
	switch action {
	case "init":
		return memory.Object{"database": s.Path}, nil
	case "health":
		return s.Health()
	case "tasks":
		return Tasks(c)
	case "prepare-selection":
		return PrepareTool(c, *tool, *session, *cwd)
	case "selection":
		raw, e := ReadInput(*answer)
		if e != nil {
			return nil, e
		}
		v, e := memory.Decode(raw)
		if e != nil {
			return nil, e
		}
		return Select(c, *request, *question, v)
	case "record":
		raw, e := ReadInput(arg())
		if e != nil {
			return nil, e
		}
		event, e := memory.Decode(raw)
		if e != nil {
			return nil, e
		}
		if e = c.Task(memory.Text(event["task"])); e != nil {
			return nil, e
		}
		return s.Ingest(event)
	case "import-source":
		if !c.SourceAllowed(arg()) {
			return nil, errors.New("source outside selected scope")
		}
		raw, e := ReadInput(arg())
		if e != nil {
			return nil, e
		}
		id, e := s.Artifact([]byte(memory.Text(Redact(string(raw)))), arg())
		return memory.Object{"hash": id, "redaction": "best-effort"}, e
	case "source":
		if excerpt {
			return s.SourceExcerpt(arg(), *offset, *maxChars)
		}
		v, b, e := s.Source(arg())
		if e != nil {
			return nil, e
		}
		if *out == "" {
			v["body"] = string(b)
		} else {
			if e = memory.NewOutput(s.Root, *out); e != nil {
				return nil, e
			}
			e = memory.WriteNew(*out, b)
		}
		return v, e
	case "brief", "recall", "history", "search", "export":
		if e = c.Task(*task); e != nil {
			return nil, e
		}
		switch action {
		case "brief":
			return s.Brief(*task, *budget)
		case "recall":
			return s.Recall(*task)
		case "export":
			return s.Export(*task, *out)
		case "search":
			if *view == "brief" {
				return s.SearchBrief(*task, arg(), *limit, *offset)
			}
			return s.Records(*task, arg(), *limit, *after)
		default:
			return s.Records(*task, "", *limit, *after)
		}
	case "bind":
		if e = c.Task(*task); e != nil {
			return nil, e
		}
		if !c.Permitted(*cwd) {
			return nil, errors.New("binding outside selected project")
		}
		return memory.Object{"binding": "saved"}, s.Bind(*tool, *session, *task, *cwd)
	case "unbind":
		return memory.Object{"binding": "removed", "records": "preserved"}, s.Unbind(*tool, *session)
	case "replay":
		return s.Replay()
	case "backup":
		return s.Backup(arg(), c.Object())
	default:
		return nil, errors.New("unknown memory command " + strconv.Quote(action))
	}
}
