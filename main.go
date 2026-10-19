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
		if len(args) != 3 || (args[1] != "claude" && args[1] != "codex") || !slices.Contains([]string{"start", "prompt", "stop", "batch", "precompact"}, args[2]) {
			return nil, errors.New("use: worklog claude|codex start|prompt|stop|batch|precompact")
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
		case "batch":
			if reminder := worklog.Batch(payload); reminder != nil {
				return reminder, nil
			}
			return nil, nil
		case "precompact":
			// Printed text, exit 0: a PreCompact hook's exit 2 would block the compaction.
			return worklog.CompactInstruction, nil
		case "prompt":
			return promptHook(payload, time.Now()), nil
		}
		if args[2] == "start" {
			recall.SweepInBackground()
			return startCard(args[1], payload), nil
		}
		if path := memory.Text(payload["transcript_path"]); path != "" {
			if _, err := recall.Archive(path, recall.Dest(recall.RawDir(), args[1], path)); err != nil {
				return "기억 원본 보관 미완료: " + err.Error() + " — 다음 수집에서 재시도한다.", nil
			}
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
			review := f.Bool("review", false, "AI proposals and conflicts waiting on the person")
			online := f.Bool("online", false, "also ask GitHub about pull requests")
			backfill := f.Bool("backfill", false, "read every recorded conversation again for its facts")
			limit := f.Int("limit", 20, "fact count")
			if err := f.Parse(flagsFirst(args[2:], "limit")); err != nil {
				return nil, err
			}
			if *backfill {
				return recall.BackfillFacts(time.Now())
			}
			return recall.FactsText(strings.Join(f.Args(), " "), *review, *online, *limit)
		}
		if args[1] == "misses" {
			m := flag.NewFlagSet("misses", flag.ContinueOnError)
			days := m.Int("days", 7, "days back")
			if err := m.Parse(args[2:]); err != nil {
				return nil, err
			}
			return recall.MissesText(*days, time.Now()), nil
		}
		if len(args) == 2 && args[1] == "import-memory" {
			return recall.ImportMemory(time.Now())
		}
		if args[1] == "relearn" {
			return relearnCLI(args[2:])
		}
		if len(args) == 3 && args[1] == "eval" {
			return recall.Eval(args[2])
		}
		if args[1] == "build" || args[1] == "sync" {
			b := flag.NewFlagSet("build", flag.ContinueOnError)
			since := b.String("since", time.Now().AddDate(0, 0, -14).Format("2006-01-02"), "first day")
			// Records come from the transcript alone; the summary model runs only when asked by hand.
			model := b.Bool("model", false, "also ask the summary model (manual)")
			b.Bool("no-model", true, "the default; kept so older commands still run")
			if e := b.Parse(args[2:]); e != nil {
				return nil, e
			}
			day, e := time.ParseInLocation("2006-01-02", *since, time.Local)
			if e != nil {
				return nil, e
			}
			if args[1] == "sync" {
				// A sweep already running means another sync is too; building twice would race on lessons.
				swept, e := recall.Sweep()
				if swept["skipped"] != nil {
					return swept, e
				}
				if e != nil {
					return swept, recall.SaveSyncStatus(swept, nil, e)
				}
				built, e := recall.Build(day, *model, time.Now())
				if built["skipped"] != nil {
					return built, e
				}
				if saved := recall.SaveSyncStatus(swept, built, e); saved != nil {
					return built, saved
				}
				return map[string]any{"archive": swept, "records": built}, e
			}
			return recall.Build(day, *model, time.Now())
		}
		if args[1] == "lang" {
			l := flag.NewFlagSet("lang", flag.ContinueOnError)
			since := l.String("since", "2000-01-01", "first day")
			sample := l.Int("sample", 0, "print this many judged blocks instead of the table")
			if e := l.Parse(args[2:]); e != nil {
				return nil, e
			}
			day, e := parseSince(*since)
			if e != nil {
				return nil, e
			}
			return recall.Lang(l.Args(), day, *sample)
		}
		return nil, errors.New("use: recall sweep|verify|find WORDS|day YYYY-MM-DD|MM-DD|오늘|어제 [WORDS]|source SESSION [WORDS]|misses [--days N]|eval FILE|import-memory|relearn [--dry-run|--expect-token TOKEN]|build [--since DAY] [--model]|sync|lang [--since DAY] [--sample N] [FILE...]")
	}
	return nil, errors.New("unknown command")
}

func parseSince(value string) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return at, nil
	}
	return time.ParseInLocation("2006-01-02", value, time.Local)
}

// promptHook adds what a request points to in the past, and once a week tells the person how
// the agents' memory lookups went. Both clients read the same JSON; nothing found prints nothing.
func promptHook(payload memory.Object, now time.Time) any {
	out := memory.Object{}
	if hint := recall.HintText(memory.Text(payload["prompt"]), memory.Text(payload["cwd"]), memory.Text(payload["session_id"]), now); hint != "" {
		out["hookSpecificOutput"] = memory.Object{"hookEventName": "UserPromptSubmit", "additionalContext": hint}
	}
	if notice := recall.MissesNotice(now); notice != "" {
		out["systemMessage"] = notice
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
