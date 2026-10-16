package adapters

import (
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
)

func Tasks(c Config) ([]memory.Object, error) {
	s, e := c.Store(false)
	if e != nil {
		return nil, e
	}
	out := []memory.Object{}
	for _, task := range c.Tasks {
		latest, e := s.LatestState(task)
		if e != nil {
			return nil, e
		}
		if latest["status"] == "complete" {
			continue
		}
		status := latest["status"]
		if status == nil {
			status = "active"
		}
		summary := []rune(memory.Text(latest["body"]))
		truncated := len(summary) > 160
		if truncated {
			summary = summary[:160]
		}
		out = append(out, memory.Object{"task": task, "status": status, "updated": latest["created"], "summary": string(summary), "summary_truncated": truncated})
	}
	return out, nil
}
func Prepare(c Config, session, cwd string) (memory.Object, error) {
	return prepare(c, session, cwd, false)
}

func prepare(c Config, session, cwd string, reuse bool) (memory.Object, error) {
	return prepareTool(c, "codex", session, cwd, reuse)
}

// Existing Codex request paths remain usable; Claude requests have a separate
// namespace even when the two clients happen to use the same session ID.
func selectionCurrentPath(s *memory.Store, tool, session string) string {
	identity := []byte(session)
	if tool != "codex" {
		identity = memory.Packed([]any{tool, session})
	}
	return filepath.Join(s.Root, "selections", memory.Hash(identity)+".current")
}

func PrepareTool(c Config, tool, session, cwd string) (memory.Object, error) {
	return prepareTool(c, tool, session, cwd, false)
}

func prepareTool(c Config, tool, session, cwd string, reuse bool) (memory.Object, error) {
	if !memory.ValidTool(tool) {
		return nil, errors.New("unsupported selection tool")
	}
	if e := memory.Identifier(session); e != nil {
		return nil, e
	}
	if !c.Permitted(cwd) {
		return nil, errors.New("selection outside project")
	}
	s, e := c.Store(false)
	if e != nil {
		return nil, e
	}
	unlock, e := memory.Lock(filepath.Join(s.Root, "selections", ".lock"))
	if e != nil {
		return nil, e
	}
	defer unlock()
	// A real answer may have committed while SessionStart waited for this
	// lock. Never create another hook prompt after that explicit choice.
	if reuse {
		binding, e := s.Binding(tool, session)
		if e != nil {
			return nil, e
		}
		optedOut, e := s.OptedOut(tool, session)
		if e != nil {
			return nil, e
		}
		if binding != nil || optedOut {
			return nil, nil
		}
	}
	choices, e := Tasks(c)
	if e != nil {
		return nil, e
	}
	cwd, e = memory.Resolve(cwd)
	if e != nil {
		return nil, e
	}
	dir := filepath.Join(s.Root, "selections")
	currentPath := selectionCurrentPath(s, tool, session)
	if reuse {
		current, e := os.ReadFile(currentPath)
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if e == nil {
			id := string(current)
			if e = memory.Identifier(id); e != nil {
				return nil, e
			}
			raw, e := os.ReadFile(filepath.Join(dir, id+".json"))
			if e != nil && !os.IsNotExist(e) {
				return nil, e
			}
			if e == nil {
				r, e := memory.Decode(raw)
				if e != nil {
					return nil, e
				}
				if r["request"] == id && r["tool"] == tool && r["session"] == session && r["cwd"] == cwd && string(memory.Packed(r["choices"])) == string(memory.Packed(choices)) {
					return r, nil
				}
			}
		}
	}
	r := memory.Object{"request": memory.ID(), "tool": tool, "session": session, "cwd": cwd, "choices": choices, "created": memory.Now()}
	if e = memory.PrivateWrite(filepath.Join(dir, memory.Text(r["request"])+".json"), memory.Packed(r)); e != nil {
		return nil, e
	}
	if e = memory.PrivateWrite(currentPath, []byte(memory.Text(r["request"]))); e != nil {
		return nil, e
	}
	return r, nil
}
func Select(c Config, id, question string, answer memory.Object) (memory.Object, error) {
	if e := memory.Identifier(id); e != nil {
		return nil, e
	}
	if question == "" {
		return nil, errors.New("actual question ID required")
	}
	command, e := c.memoryCommand()
	if e != nil {
		return nil, e
	}
	s, e := c.Store(false)
	if e != nil {
		return nil, e
	}
	dir := filepath.Join(s.Root, "selections")
	unlock, e := memory.Lock(filepath.Join(dir, ".lock"))
	if e != nil {
		return nil, e
	}
	defer unlock()
	path := filepath.Join(dir, id+".json")
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	r, e := memory.Decode(raw)
	if e != nil {
		return nil, e
	}
	session := memory.Text(r["session"])
	tool := memory.Text(r["tool"])
	if !memory.ValidTool(tool) {
		return nil, errors.New("unsupported selection tool")
	}
	if answer["request"] != id || answer["question_id"] != question || answer["session"] != session {
		return nil, errors.New("answer does not match selection")
	}
	current, e := os.ReadFile(selectionCurrentPath(s, tool, session))
	if e != nil || string(current) != id {
		return nil, errors.New("selection superseded")
	}
	if !c.Permitted(memory.Text(r["cwd"])) {
		return nil, errors.New("project scope changed")
	}
	choices, ok := r["choices"].([]any)
	if !ok {
		return nil, errors.New("invalid selection choices: array required")
	}
	for _, choice := range choices {
		v, ok := choice.(map[string]any)
		if !ok || memory.Identifier(memory.Text(v["task"])) != nil {
			return nil, errors.New("invalid selection choices: task object required")
		}
	}
	selected := memory.Text(answer["selected"])
	if selected != "none" {
		offered := false
		for _, choice := range choices {
			v, _ := choice.(map[string]any)
			if v["task"] == selected {
				offered = true
			}
		}
		active, e := Tasks(c)
		if e != nil {
			return nil, e
		}
		valid := false
		for _, v := range active {
			if v["task"] == selected {
				valid = true
			}
		}
		if !offered || !valid {
			return nil, errors.New("task not offered or no longer active")
		}
	}
	source, e := s.Artifact(memory.Packed(answer), "selection://"+id+"/"+question)
	if e != nil {
		return nil, e
	}
	if selected == "none" {
		e = s.Unbind(tool, session)
	} else {
		e = s.Bind(tool, session, selected, memory.Text(r["cwd"]))
	}
	if e != nil {
		return nil, e
	}
	if e = os.Rename(path, filepath.Join(dir, id+".consumed")); e != nil {
		return nil, e
	}
	result := memory.Object{"selection": selected, "source": source, "proof": "caller-recorded response, not authenticated identity"}
	if selected != "none" {
		result["instruction"] = command + " brief --task " + selected + "로 현재 작업 기억을 읽으세요. 과거 승인은 새 실행 허가가 아닙니다."
	}
	return result, nil
}
