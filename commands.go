package main

import (
	"encoding/json"
	"errors"
	"flag"
	"github.com/ftery0/agent-recall/internal/adapters"
	"github.com/ftery0/agent-recall/internal/install"
	"github.com/ftery0/agent-recall/internal/recall"
	"os"
	"time"
)

func relearnCLI(args []string) (any, error) {
	f := flag.NewFlagSet("relearn", flag.ContinueOnError)
	dryRun := f.Bool("dry-run", false, "preview without saving")
	token := f.String("expect-token", "", "source token returned by the preview")
	if err := f.Parse(args); err != nil {
		return nil, err
	}
	provided := false
	f.Visit(func(v *flag.Flag) { provided = provided || v.Name == "expect-token" })
	if len(f.Args()) != 0 || (*dryRun && provided) || (provided && *token == "") {
		return nil, errors.New("use: recall relearn [--dry-run|--expect-token TOKEN]")
	}
	if *dryRun {
		return recall.PreviewRelearn(time.Now())
	}
	return recall.RelearnChecked(time.Now(), *token)
}

func installCLI(args []string) (any, error) {
	if len(args) < 2 {
		return nil, errors.New("install plan|apply|doctor|rollback file-or-id")
	}
	switch args[0] {
	case "plan":
		raw, e := os.ReadFile(args[1])
		if e != nil {
			return nil, e
		}
		var entries []install.Entry
		if e = json.Unmarshal(raw, &entries); e != nil {
			return nil, e
		}
		return install.Plan(entries)
	case "apply":
		m, e := install.Read(args[1])
		if e != nil {
			return nil, e
		}
		return install.Apply(adapters.Home(), m)
	case "doctor":
		m, e := install.Read(args[1])
		if e != nil {
			return nil, e
		}
		return install.Doctor(m)
	case "rollback":
		return install.Rollback(adapters.Home(), args[1])
	default:
		return nil, errors.New("unknown install action")
	}
}
