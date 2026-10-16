package adapters

import (
	"context"
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/policy"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func Hook(c Config, configured bool, tool string, payload memory.Object, home string) (memory.Object, error) {
	if tool != "codex" && tool != "claude" {
		return nil, errors.New("unsupported hook adapter")
	}
	result := memory.Object{}
	if configured {
		captured, e := Capture(c, tool, payload)
		if e != nil {
			result["systemMessage"] = "미확인: 기억 수집 실패: " + e.Error()
		} else {
			for k, v := range captured {
				result[k] = v
			}
		}
	}
	event := memory.Text(payload["hook_event_name"])
	if event != "PreToolUse" && event != "PostToolUse" && event != "Stop" {
		return result, nil
	}
	toolName := memory.Text(payload["tool_name"])
	if event != "Stop" && toolName != "Edit" && toolName != "Write" && toolName != "Bash" && toolName != "exec_command" && toolName != "apply_patch" {
		return result, nil
	}
	cwd := memory.Text(payload["cwd"])
	session := memory.Text(payload["session_id"])
	if cwd == "" || session == "" {
		result["systemMessage"] = "미확인: 세션 또는 경로 없음"
		return result, nil
	}
	root := ""
	if configured && c.Permitted(cwd) {
		root = c.ProjectRoot
		for _, alias := range c.Aliases {
			if memory.Within(alias, cwd) {
				root = alias
			}
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		raw, e := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
		if e != nil {
			result["systemMessage"] = "미확인: 허용된 개인 프로젝트 또는 Git 프로젝트가 아닙니다."
			return result, nil
		}
		root = strings.TrimSpace(string(raw))
	}
	task := memory.Text(payload["task_id"])
	if configured {
		s, e := c.Store(false)
		if e == nil {
			b, e := s.Binding(tool, session)
			if e == nil && b != nil {
				task = memory.Text(b["task"])
			}
		}
	}
	if task == "" {
		task = "unbound"
	}
	runtime := filepath.Join(home, "runtime", memory.Hash([]byte(root)))
	if configured {
		s, err := c.Store(false)
		if err == nil {
			runtime = filepath.Join(s.Root, "runtime")
		}
	}
	collector := policy.Collector{Root: root, Runtime: runtime, Tool: tool, Session: session, Task: task}
	checked, e := collector.Event(payload)
	if e != nil {
		result["systemMessage"] = "미확인: 편집 근거 검사 실패: " + e.Error()
		return result, nil
	}
	for _, key := range []string{"decision", "reason", "systemMessage"} {
		if v, ok := checked[key]; ok {
			if key == "systemMessage" && result[key] != nil {
				result[key] = memory.Text(result[key]) + "\n" + memory.Text(v)
			} else {
				result[key] = v
			}
		}
	}
	return result, nil
}
func Home() string {
	if v := os.Getenv("HARNESS_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local/share/harness")
}
