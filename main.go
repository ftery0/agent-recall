package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/ftery0/agent-recall/internal/adapters"
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/policy"
	"github.com/ftery0/agent-recall/internal/recall"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"slices"
	"strings"
	"time"
)

const version = "0.1.0"

type structureOutput struct {
	text   string
	code   int
	stderr bool
}

type structureMeasures []policy.StructureMetricsRecord

func run(args []string) (any, error) {
	f := flag.NewFlagSet("harness", flag.ContinueOnError)
	// Registered edit-scope hooks still pass the old memory database's configuration; nothing reads it.
	f.String("config", "", "unused")
	if e := f.Parse(args); e != nil {
		return nil, e
	}
	args = f.Args()
	if len(args) == 0 {
		return nil, errors.New("use: harness worklog|recall|edit-scope|structure-check|install|version")
	}
	if args[0] == "version" {
		return memory.Object{"version": version, "runtime": "go"}, nil
	}
	if args[0] == "install" {
		return installCLI(args[1:])
	}
	if args[0] == "edit-scope" {
		if len(args) < 2 || len(args) > 3 || !memory.ValidTool(args[1]) {
			return nil, errors.New("use: edit-scope TOOL [export|clear|stop]")
		}
		raw, err := adapters.ReadInput("-")
		if err != nil {
			return memory.Object{"systemMessage": "미확인: 편집 이벤트 읽기 실패: " + err.Error()}, nil
		}
		payload, err := memory.Decode(raw)
		if err != nil {
			return memory.Object{"systemMessage": "미확인: 편집 이벤트 해석 실패: " + err.Error()}, nil
		}
		if len(args) == 3 {
			switch args[2] {
			case "export":
				payload["hook_event_name"] = "Export"
			case "clear":
				payload["hook_event_name"] = "Clear"
			case "stop":
				payload["hook_event_name"] = "Stop"
			default:
				return nil, errors.New("unknown edit-scope action")
			}
		}
		if args[1] == "codex" && len(args) == 2 {
			if deny := adapters.AttributionHook(payload); deny != nil {
				return deny, nil
			}
		}
		output, err := adapters.StructureHook(args[1], payload)
		// Codex has one hook before its tools and one after; the memory of the repositories a command
		// touches rides on the first, a failed command's past fix on the second. The edit scope only
		// ever answers with a decision, a reason or a system message.
		if args[1] == "codex" && len(args) == 2 {
			added := actionHook(payload)
			if memory.Text(payload["hook_event_name"]) == "PostToolUse" && memory.Text(payload["tool_name"]) == "Bash" && codexFailed(payload["tool_response"]) {
				added = failureHook(payload)
			}
			if action, ok := added.(memory.Object); ok {
				if output == nil {
					output = memory.Object{}
				}
				output["hookSpecificOutput"] = action["hookSpecificOutput"]
			}
		}
		if output == nil {
			return nil, err
		}
		return output, err
	}
	if args[0] == "structure-check" {
		measure := len(args) > 1 && args[1] == "--measure"
		if measure {
			args = append([]string{args[0]}, args[2:]...)
		}
		if len(args) < 2 {
			return nil, errors.New("use: structure-check [--measure] ROOT FILE...")
		}
		if measure {
			records, err := policy.StructureMeasure(args[1], args[2:])
			if err != nil {
				return structureOutput{"미확인: 구조 측정 실패: " + err.Error() + "\n", 2, true}, nil
			}
			return structureMeasures(records), nil
		}
		output, code, err := policy.StructureCheck(args[1], args[2:], os.Getenv("STRUCTURE_BASELINE_FILE"))
		if err != nil {
			if output == "" {
				output = "미확인: 구조 검사 실패: " + err.Error() + "\n"
			}
			return structureOutput{output, 2, true}, nil
		}
		return structureOutput{output, code, false}, nil
	}
	if args[0] == "worklog" {
		if len(args) != 3 || (args[1] != "claude" && args[1] != "codex") || !slices.Contains([]string{"start", "stop", "precompact", "action", "failure"}, args[2]) {
			return nil, errors.New("use: worklog claude|codex start|stop|precompact|action|failure")
		}
		// A broken payload must never fail the client's hook, so it simply produces no card or block.
		raw, err := adapters.ReadInput("-")
		if err != nil || os.Getenv("HARNESS_RECALL") == "1" {
			return nil, nil
		}
		payload, err := memory.Decode(raw)
		if err != nil {
			return nil, nil
		}
		switch args[2] {
		case "precompact":
			// The summary may drop what the action hook said; it may say it again after.
			recall.ForgetActions(memory.Text(payload["session_id"]))
			// Printed text, exit 0: a PreCompact hook's exit 2 would block the compaction.
			return worklog.CompactInstruction, nil
		case "action":
			return actionHook(payload), nil
		case "failure":
			return failureHook(payload), nil
		}
		if args[2] == "start" {
			// A sync copies what is missing, then records it.
			recall.RunInBackground("recall", "sync")
			return startCard(args[1], payload), nil
		}
		if path := memory.Text(payload["transcript_path"]); path != "" {
			dest := recall.Dest(recall.RawDir(), args[1], path)
			if _, err := recall.Archive(path, dest); err != nil {
				return "기억 원본 보관 미완료: " + err.Error() + " — 다음 수집에서 재시도한다.", nil
			}
			// What this turn did is in the record and the ledger before the next conversation starts.
			recall.RunInBackground("recall", "build", "--only", dest)
		}
		return nil, nil
	}
	if args[0] == "recall" {
		if len(args) < 2 {
			return nil, errors.New("use: recall sweep|verify|find|day|source|facts|misses|eval|build|sync")
		}
		if len(args) == 2 && args[1] == "sweep" {
			return recall.Sweep()
		}
		if len(args) == 2 && args[1] == "verify" {
			return recall.Verify()
		}
		if len(args) >= 2 && args[1] == "find" {
			f := flag.NewFlagSet("find", flag.ContinueOnError)
			repo := f.String("repo", "", "only this repository")
			since := f.String("since", "", "only from this day (YYYY-MM-DD)")
			limit := f.Int("limit", 5, "result count")
			if err := f.Parse(flagsFirst(args[2:], "repo", "since", "limit")); err != nil {
				return nil, err
			}
			filter := recall.FindFilter{Repo: *repo}
			if *since != "" {
				day, err := time.ParseInLocation("2006-01-02", *since, time.Local)
				if err != nil {
					return nil, fmt.Errorf("--since 는 YYYY-MM-DD 로 쓴다: %q", *since)
				}
				filter.Since = day
			}
			return recall.FindText(strings.Join(f.Args(), " "), *limit, filter), nil
		}
		if len(args) >= 3 && args[1] == "day" {
			return recall.DayText(args[2], strings.Join(args[3:], " "), 8, time.Now())
		}
		if len(args) >= 3 && args[1] == "source" {
			f := flag.NewFlagSet("source", flag.ContinueOnError)
			id := f.String("request", "", "original request ID")
			offset := f.Int("offset", 0, "first excerpt")
			limit := f.Int("limit", 20, "excerpt count")
			if err := f.Parse(flagsFirst(args[3:], "request", "offset", "limit")); err != nil {
				return nil, err
			}
			return recall.SourcePage(args[2], strings.Join(f.Args(), " "), *id, *offset, *limit)
		}
		if args[1] == "facts" {
			f := flag.NewFlagSet("facts", flag.ContinueOnError)
			online := f.Bool("online", false, "also ask GitHub about pull requests")
			backfill := f.Bool("backfill", false, "read every recorded conversation again for its facts")
			rebuild := f.Bool("rebuild", false, "start the ledger again from every conversation, keeping the old one")
			limit := f.Int("limit", 20, "fact count")
			if err := f.Parse(flagsFirst(args[2:], "limit")); err != nil {
				return nil, err
			}
			if *backfill || *rebuild {
				return recall.BackfillFacts(time.Now(), *rebuild)
			}
			return recall.FactsText(strings.Join(f.Args(), " "), *online, *limit)
		}
		if args[1] == "misses" {
			m := flag.NewFlagSet("misses", flag.ContinueOnError)
			days := m.Int("days", 7, "days back")
			if err := m.Parse(args[2:]); err != nil {
				return nil, err
			}
			return recall.MissesText(*days, time.Now()), nil
		}
		if len(args) == 3 && args[1] == "eval" {
			return recall.Eval(args[2])
		}
		if args[1] == "build" || args[1] == "sync" {
			b := flag.NewFlagSet("build", flag.ContinueOnError)
			since := b.String("since", time.Now().AddDate(0, 0, -14).Format("2006-01-02"), "first day")
			only := b.String("only", "", "record this one archived transcript")
			if e := b.Parse(args[2:]); e != nil {
				return nil, e
			}
			if *only != "" {
				return recall.BuildConversation(*only, time.Now())
			}
			day, e := time.ParseInLocation("2006-01-02", *since, time.Local)
			if e != nil {
				return nil, e
			}
			if args[1] == "sync" {
				// A sweep already running means another sync is too; building twice would race on the ledger.
				swept, e := recall.Sweep()
				if swept["skipped"] != nil {
					return swept, e
				}
				if e != nil {
					return swept, recall.SaveSyncStatus(swept, nil, e)
				}
				built, e := recall.Build(day, time.Now())
				if built["skipped"] != nil {
					return built, e
				}
				if saved := recall.SaveSyncStatus(swept, built, e); saved != nil {
					return built, saved
				}
				return map[string]any{"archive": swept, "records": built}, e
			}
			return recall.Build(day, time.Now())
		}
		return nil, errors.New("use: recall sweep|verify|find WORDS|day YYYY-MM-DD|MM-DD|오늘|어제 [WORDS]|source SESSION [WORDS]|misses [--days N]|eval FILE|build [--since DAY]|sync")
	}
	return nil, errors.New("unknown command")
}

// actionHook adds, before a shell command that commits, pushes or changes GitHub, what memory holds
// about the repositories it touches. Nothing unusual adds nothing.
func actionHook(payload memory.Object) any {
	// Codex runs the same hook after its tools too; by then the command has run.
	if event := memory.Text(payload["hook_event_name"]); event != "" && event != "PreToolUse" {
		return nil
	}
	input, _ := payload["tool_input"].(map[string]any)
	command := memory.Text(input["command"]) + memory.Text(input["cmd"])
	if command == "" {
		return nil
	}
	text := recall.ActionText(command, memory.Text(payload["cwd"]), memory.Text(payload["session_id"]))
	if text == "" {
		return nil
	}
	return memory.Object{"hookSpecificOutput": memory.Object{"hookEventName": "PreToolUse", "additionalContext": text}}
}

// failureHook adds, after a shell command failed, how a failure like it was got past before. A
// failure with nothing like it in memory adds nothing, and the hook never blocks.
func failureHook(payload memory.Object) any {
	if payload["is_interrupt"] == true {
		return nil
	}
	input, _ := payload["tool_input"].(map[string]any)
	command := memory.Text(input["command"]) + memory.Text(input["cmd"])
	if command == "" {
		return nil
	}
	text := recall.FailureText(command, memory.Text(payload["cwd"]), memory.Text(payload["session_id"]))
	if text == "" {
		return nil
	}
	event := memory.Text(payload["hook_event_name"])
	if event == "" {
		event = "PostToolUseFailure"
	}
	return memory.Object{"hookSpecificOutput": memory.Object{"hookEventName": event, "additionalContext": text}}
}

// codexFailed reads a Codex shell result. Codex hands the hook only the text the model saw, without
// the exit code, so a failure is known by the error the program printed.
func codexFailed(response any) bool {
	return recall.OutputFailed(memory.Text(response))
}

// flagsFirst moves flags ahead of the words, so "source ID 말 --limit 2" reads like
// "source ID --limit 2 말"; the flag package stops at the first word and would drop the rest.
func flagsFirst(args []string, valued ...string) []string {
	var flags, words []string
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(args[i], "-")
		switch {
		case args[i] == "--":
			words = append(words, args[i+1:]...)
			i = len(args)
		case !strings.HasPrefix(args[i], "-") || name == "":
			words = append(words, args[i])
		default:
			flags = append(flags, args[i])
			if slices.Contains(valued, name) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		}
	}
	return append(append(flags, "--"), words...)
}

func main() {
	result, e := run(os.Args[1:])
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if result != nil {
		if records, ok := result.(structureMeasures); ok {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetEscapeHTML(false)
			for _, record := range records {
				if err := encoder.Encode(record); err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
			}
			return
		}
		if output, ok := result.(structureOutput); ok {
			if output.stderr {
				fmt.Fprint(os.Stderr, output.text)
			} else {
				fmt.Fprint(os.Stdout, output.text)
			}
			if output.code != 0 {
				os.Exit(output.code)
			}
			return
		}
		if output, ok := result.(string); ok {
			fmt.Fprintln(os.Stdout, output)
			return
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if e = encoder.Encode(result); e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}
}
