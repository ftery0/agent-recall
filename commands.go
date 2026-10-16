package main

import (
	"encoding/json"
	"errors"
	"flag"
	"github.com/ftery0/agent-recall/internal/adapters"
	"github.com/ftery0/agent-recall/internal/install"
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/policy"
	"github.com/ftery0/agent-recall/internal/recall"
	"os"
	"path/filepath"
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
func policyCLI(c adapters.Config, args []string) (any, error) {
	if len(args) == 0 {
		return nil, errors.New("policy command required")
	}
	f := flag.NewFlagSet("policy", flag.ContinueOnError)
	task := f.String("task", "", "")
	session := f.String("session", "", "")
	action := f.String("action", "", "")
	target := f.String("target", "", "")
	input := f.String("input", "-", "")
	if e := f.Parse(args[1:]); e != nil {
		return nil, e
	}
	if e := c.Task(*task); e != nil {
		return nil, e
	}
	s, e := c.Store(false)
	if e != nil {
		return nil, e
	}
	scope := policy.Scope{Root: c.ProjectRoot, Runtime: filepath.Join(s.Root, "runtime"), Task: *task, Session: *session}
	if args[0] == "check" {
		return scope.Check(*action, *target)
	}
	if args[0] == "checks" {
		raw, e := adapters.ReadInput(*input)
		if e != nil {
			return nil, e
		}
		v, e := memory.Decode(raw)
		if e != nil {
			return nil, e
		}
		return policy.RunChecks(scope, memory.Strings(v["files"]))
	}
	if args[0] == "status" || args[0] == "hold" || args[0] == "cancel" || args[0] == "continue" || args[0] == "new-task" {
		return scope.Update(args[0], nil)
	}
	raw, e := adapters.ReadInput(*input)
	if e != nil {
		return nil, e
	}
	v, e := memory.Decode(raw)
	if e != nil {
		return nil, e
	}
	return scope.Update(args[0], v)
}

func status(c adapters.Config) (any, error) {
	s, e := c.Store(false)
	if e != nil {
		return nil, e
	}
	health, e := s.Health()
	if e != nil {
		return nil, e
	}
	paths, e := filepath.Glob(filepath.Join(adapters.Home(), "installs", "*", "manifest.json"))
	if e != nil {
		return nil, e
	}
	var latest install.Manifest
	for _, path := range paths {
		m, e := install.Read(path)
		if e != nil {
			return nil, e
		}
		if m.Created > latest.Created {
			latest = m
		}
	}
	var installation any
	if latest.ID != "" {
		installation, e = install.Doctor(latest)
		if e != nil {
			return nil, e
		}
	}
	var migration any
	raw, e := os.ReadFile(filepath.Join(adapters.Home(), "migration-state.json"))
	if e == nil {
		state, err := memory.Decode(raw)
		if err != nil {
			return nil, err
		}
		if state["prepared_database"] == s.Path {
			if state["state"] == "prepared" {
				old, err := os.ReadFile(memory.Text(state["legacy_database"]))
				state["legacy_unchanged"] = err == nil && memory.Hash(old) == state["legacy_sha256"]
			}
			migration = state
		}
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	return memory.Object{"version": version, "project": c.Project, "database": s.Path, "memory": health, "migration": migration, "latest_install": installation, "native_hooks": "unverified: review registration and trust in the client", "claude_live": "deferred"}, nil
}
