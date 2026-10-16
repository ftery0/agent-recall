package policy

import (
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
)

type Scope struct{ Root, Runtime, Task, Session string }

func (s Scope) path() string {
	return filepath.Join(s.Runtime, "approval-"+memory.Hash(memory.Packed([]any{s.Root, s.Task, s.Session}))+".json")
}
func (s Scope) Read() (memory.Object, error) {
	raw, e := os.ReadFile(s.path())
	if os.IsNotExist(e) {
		return memory.Object{"status": "empty", "grants": []any{}}, nil
	}
	if e != nil {
		return nil, e
	}
	return memory.Decode(raw)
}
func (s Scope) Update(operation string, request memory.Object) (memory.Object, error) {
	for _, v := range []string{s.Task, s.Session} {
		if e := memory.Identifier(v); e != nil {
			return nil, e
		}
	}
	unlock, e := memory.Lock(s.path() + ".lock")
	if e != nil {
		return nil, e
	}
	defer unlock()
	state, e := s.Read()
	if e != nil {
		return nil, e
	}
	switch operation {
	case "status", "continue":
		return state, nil
	case "hold":
		state["status"] = "held"
	case "cancel", "new-task":
		state = memory.Object{"status": "empty", "grants": []any{}}
	case "grant":
		source := memory.Text(request["source"])
		action := memory.Text(request["action"])
		if source == "" || action == "" {
			return nil, errors.New("explicit request source and action required")
		}
		if request["task"] != s.Task || request["session"] != s.Session {
			return nil, errors.New("grant belongs to another task/session")
		}
		paths := memory.Strings(request["paths"])
		for _, path := range paths {
			if !filepath.IsAbs(path) || !memory.Within(s.Root, path) {
				return nil, errors.New("grant path outside project")
			}
		}
		if action == "edit" && len(paths) == 0 {
			return nil, errors.New("edit paths required")
		}
		if action != "edit" && memory.Text(request["command"]) == "" {
			return nil, errors.New("exact command required")
		}
		grants, _ := state["grants"].([]any)
		request["assurance"] = "caller-recorded user evidence; native permission remains required"
		state["grants"] = append(grants, request)
		state["status"] = "active"
	case "resume":
		if memory.Text(request["question_id"]) == "" || request["task"] != s.Task || request["session"] != s.Session {
			return nil, errors.New("matching actual answer required")
		}
		if state["status"] == "held" {
			state["status"] = "active"
		}
	default:
		return nil, errors.New("unknown scope operation")
	}
	state["updated"] = memory.Now()
	return state, memory.PrivateWrite(s.path(), memory.Packed(state))
}
func (s Scope) Check(action, target string) (memory.Object, error) {
	state, e := s.Read()
	if e != nil {
		return nil, e
	}
	if state["status"] == "active" {
		grants, _ := state["grants"].([]any)
		for _, g := range grants {
			grant, _ := g.(map[string]any)
			if grant["action"] != action || grant["task"] != s.Task || grant["session"] != s.Session {
				continue
			}
			allowed := grant["command"] == target && action != "edit"
			if action == "edit" {
				for _, path := range memory.Strings(grant["paths"]) {
					p, _ := memory.Resolve(path)
					t, _ := memory.Resolve(target)
					if p == t {
						allowed = true
					}
				}
			}
			if allowed {
				return memory.Object{"decision": "in-scope", "source": grant["source"], "native_permission": "required independently"}, nil
			}
		}
	}
	return memory.Object{"decision": "question-required", "reason": "현재 작업의 명시된 범위를 확인할 근거가 없습니다. 기존 승인은 보존됩니다.", "choices": []string{"현재 범위 유지", "새 범위 승인", "해당 작업 보류"}}, nil
}
