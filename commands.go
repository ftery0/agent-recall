package main

import (
	"encoding/json"
	"errors"
	"github.com/ftery0/agent-recall/internal/adapters"
	"github.com/ftery0/agent-recall/internal/install"
	"os"
)

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
