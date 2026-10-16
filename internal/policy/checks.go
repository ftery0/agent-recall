package policy

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Check struct {
	Name    string `json:"name"`
	Command string `json:"command"`
	Pattern string `json:"pattern"`
	Timeout int    `json:"timeout"`
}
type CheckConfig struct {
	Checks []Check `json:"checks"`
}
type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	space := 1<<20 - len(b.data)
	if space > 0 {
		if len(p) > space {
			p = p[:space]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func RunChecks(scope Scope, files []string) ([]memory.Object, error) {
	raw, e := os.ReadFile(filepath.Join(scope.Root, ".claude", "checks.json"))
	if os.IsNotExist(e) {
		return []memory.Object{}, nil
	}
	if e != nil {
		return nil, e
	}
	var config CheckConfig
	if e = json.Unmarshal(raw, &config); e != nil {
		return nil, e
	}
	out := []memory.Object{}
	for _, check := range config.Checks {
		if check.Command == "" {
			continue
		}
		pattern, e := regexp.Compile(check.Pattern)
		if e != nil {
			return nil, e
		}
		matched := []string{}
		snapshot := memory.Object{}
		for _, path := range files {
			relative := path
			if filepath.IsAbs(path) {
				relative, e = filepath.Rel(scope.Root, path)
				if e != nil {
					return nil, e
				}
			}
			full := filepath.Join(scope.Root, relative)
			if !memory.Within(scope.Root, full) {
				return nil, errors.New("check target outside project")
			}
			if pattern.MatchString(relative) {
				matched = append(matched, full)
				raw, e := os.ReadFile(full)
				if e == nil {
					snapshot[full] = memory.Hash(raw)
				} else if !os.IsNotExist(e) {
					return nil, e
				}
			}
		}
		if len(matched) == 0 {
			continue
		}
		// Listing a check in the repo's own checks.json is the project's permission to run it.
		r := memory.Object{"name": check.Name, "command_template": check.Command, "snapshot": snapshot, "executed": false}
		command := check.Command
		if strings.Contains(command, "{files}") {
			quoted := []string{}
			for _, p := range matched {
				if _, e := os.Stat(p); e == nil {
					quoted = append(quoted, "'"+strings.ReplaceAll(p, "'", "'\"'\"'")+"'")
				}
			}
			if len(quoted) == 0 {
				r["reason"] = "검사할 파일이 삭제됨"
				out = append(out, r)
				continue
			}
			command = strings.ReplaceAll(command, "{files}", strings.Join(quoted, " "))
		}
		timeout := check.Timeout
		if timeout <= 0 || timeout > 300 {
			timeout = 120
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		cmd := exec.CommandContext(ctx, "/bin/bash", "-c", command)
		cmd.Dir = scope.Root
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process != nil {
				return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
			return nil
		}
		cmd.WaitDelay = time.Second
		var output limitedOutput
		cmd.Stdout = &output
		cmd.Stderr = &output
		e = cmd.Run()
		timed := ctx.Err() != nil
		cancel()
		status := 0
		if e != nil {
			status = -1
			var exit *exec.ExitError
			if errors.As(e, &exit) {
				status = exit.ExitCode()
			}
		}
		r["executed"] = true
		r["command"] = []string{"/bin/bash", "-c", command}
		r["exit_code"] = status
		r["passed"] = e == nil
		r["timed_out"] = timed
		r["checked_at"] = memory.Now()
		r["output"] = string(output.data)
		out = append(out, r)
	}
	return out, nil
}
