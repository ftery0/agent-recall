package policy

import (
	"crypto/rand"
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (c Collector) Event(payload memory.Object) (memory.Object, error) {
	root, err := memory.Resolve(c.Root)
	if err != nil {
		return nil, err
	}
	c.Root = root
	if e := memory.Identifier(c.Session); e != nil {
		return nil, e
	}
	if payload == nil {
		return nil, errors.New("invalid hook payload")
	}
	if input, ok := payload["tool_input"]; ok && input != nil {
		if _, ok = input.(map[string]any); !ok {
			return nil, errors.New("invalid hook input")
		}
	}
	event := memory.Text(payload["hook_event_name"])
	paths, moves, complete := Targets(payload, c.Root)
	if (event == "PreToolUse" || event == "PostToolUse") && len(paths) == 0 && complete {
		state, e := c.state()
		if e != nil {
			return nil, e
		}
		if state.Observed {
			return nil, nil
		}
	}
	unlock, e := editLock(strings.TrimSuffix(c.path(), ".json") + ".lock")
	if e != nil {
		c.recordFailure()
		return memory.Object{"systemMessage": "미확인: 편집 범위 잠금 대기 상한을 넘었습니다. 이번 검사는 차단하지 않습니다."}, nil
	}
	defer unlock()
	state, e := c.state()
	if e != nil {
		return nil, e
	}
	original := string(memory.Packed(state))
	if raw, e := readRegularFile(c.failurePath()); e == nil {
		// This identifies a newly observed failure record, not a recovered tool call.
		observation := memory.Hash([]byte(rand.Text()))
		for _, note := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if note != "" {
				state.addUnverified(observation, note)
			}
		}
		_ = os.Remove(c.failurePath())
	} else if !os.IsNotExist(e) {
		return nil, e
	}
	if event == "Clear" {
		for _, path := range []string{c.path(), strings.TrimSuffix(c.path(), ".json") + ".baseline.json", c.failurePath()} {
			if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
				return nil, e
			}
		}
		return memory.Object{"cleared": true, "scope": strings.TrimSuffix(filepath.Base(c.path()), ".json")}, nil
	}
	if event == "Export" || event == "Stop" {
		exported, notes, e := c.snapshot(&state)
		if e != nil {
			return nil, e
		}
		baselinePath := strings.TrimSuffix(c.path(), ".json") + ".baseline.json"
		var out memory.Object
		if event == "Export" {
			if e = memory.PrivateWrite(baselinePath, memory.Packed(exported)); e != nil {
				return nil, e
			}
			files := []string{}
			for path := range exported.Files {
				files = append(files, filepath.Join(c.Root, path))
			}
			sort.Strings(files)
			out = memory.Object{"files": files, "baseline": baselinePath, "unverified": notes}
		} else {
			out, e = c.stop(&state, exported, notes)
			if e != nil {
				return nil, e
			}
		}
		state.Unverified = unique(state.Unverified)
		if string(memory.Packed(state)) != original {
			if e = memory.PrivateWrite(c.path(), memory.Packed(state)); e != nil {
				return nil, e
			}
		}
		return out, nil
	}
	if event != "PreToolUse" && event != "PostToolUse" {
		return nil, nil
	}
	state.Observed = true
	if len(paths) == 0 && complete {
		if string(memory.Packed(state)) != original {
			return nil, memory.PrivateWrite(c.path(), memory.Packed(state))
		}
		return nil, nil
	}
	key := memory.Text(payload["tool_use_id"])
	if key == "" {
		key = memory.Hash(memory.Packed([]any{payload["tool_name"], payload["tool_input"], payload["turn_id"], payload["cwd"]}))
	}
	observation := ""
	if id := memory.Text(payload["tool_use_id"]); id != "" {
		observation = memory.Hash(memory.Packed([]any{"tool_call", id}))
	}
	if event == "PreToolUse" {
		if prior, ok := state.Pending[key]; ok {
			prior.Ambiguous = true
			state.Pending[key] = prior
		} else {
			if observation == "" {
				// A local observation token pairs this record; it does not prove a native call ID.
				observation = memory.Hash([]byte(rand.Text()))
			}
			call := Call{Files: map[string]Observation{}, Moves: moves, Complete: complete, ToolUseID: memory.Text(payload["tool_use_id"]), ObservationID: observation}
			for _, path := range paths {
				m, e := measure(filepath.Join(c.Root, path))
				if e != nil {
					return nil, e
				}
				hash, e := fingerprint(filepath.Join(c.Root, path))
				if e != nil {
					return nil, e
				}
				call.Files[path] = Observation{m, hash}
				dir := filepath.Dir(path)
				if _, ok := state.Directories[dir]; !ok {
					state.Directories[dir] = m.Components
				}
			}
			for destination, source := range call.Moves {
				if _, ok := call.Files[destination]; !ok {
					delete(call.Moves, destination)
					continue
				}
				if _, ok := call.Files[source]; !ok {
					delete(call.Moves, destination)
				}
			}
			state.Pending[key] = call
		}
	} else {
		before, ok := state.Pending[key]
		delete(state.Pending, key)
		if before.ObservationID != "" {
			observation = before.ObservationID
		} else if observation == "" {
			observation = memory.Hash([]byte(rand.Text()))
		}
		response, _ := payload["tool_response"].(map[string]any)
		diff, _ := response["bashEditDiff"].(map[string]any)
		failed := response["is_error"] == true || response["error"] != nil
		if exit, present := response["exit_code"]; present && memory.Number(exit) != 0 {
			failed = true
		}
		shared := diff["shared"] == true || diff["unavailable"] == true || diff["skipped"] == true
		if !ok || before.Ambiguous || !before.Complete || shared || failed || before.ToolUseID != "" && before.ToolUseID != memory.Text(payload["tool_use_id"]) {
			state.addUnverified(observation, "범위·호출 근거 부족 또는 동시 변경: "+memory.Text(payload["tool_name"]))
		} else {
			for path, item := range before.Files {
				after, e := fingerprint(filepath.Join(c.Root, path))
				if e != nil {
					return nil, e
				}
				if after == item.Digest {
					continue
				}
				previous, exists := state.Files[path]
				if exists && previous.After != item.Digest {
					// Keep the last verified change; this edit cannot extend its evidence.
					state.addUnverified(observation, "연속 편집 소유권 미확인: "+path)
					continue
				}
				metrics := item.Before
				if exists {
					metrics = previous.Before
				}
				if source, ok := before.Moves[path]; ok {
					original := before.Files[source].Before
					if previous, ok := state.Files[source]; ok {
						original = previous.Before
					}
					metrics.Lines = original.Lines
					metrics.Banners = original.Banners
					metrics.Longs = original.Longs
				}
				state.Files[path] = Change{metrics, after}
			}
		}
	}
	state.Unverified = unique(state.Unverified)
	return nil, memory.PrivateWrite(c.path(), memory.Packed(state))
}
func (c Collector) snapshot(state *EditState) (baseline, []string, error) {
	notes := append([]string{}, state.Unverified...)
	if !state.Observed {
		notes = append(notes, "편집 수집 훅 실행 근거 없음")
	}
	ready := map[string]Change{}
	for path, item := range state.Files {
		hash, e := fingerprint(filepath.Join(c.Root, path))
		if e != nil {
			return baseline{}, nil, e
		}
		if hash == item.After {
			ready[path] = item
		} else {
			notes = append(notes, "확인한 편집 후 다른 변경이 있음: "+path)
		}
	}
	if len(state.Pending) > 0 {
		for _, call := range state.Pending {
			observation := call.ObservationID
			if observation == "" {
				if call.ToolUseID != "" {
					observation = memory.Hash(memory.Packed([]any{"tool_call", call.ToolUseID}))
				} else {
					observation = memory.Hash([]byte(rand.Text()))
				}
			}
			note := "종료 시점까지 결과가 없는 호출: 취소 또는 결과 미수신"
			state.addUnverified(observation, note)
			notes = append(notes, note)
		}
		state.Pending = map[string]Call{}
	}
	return baseline{c.Root, ready, state.Directories}, unique(notes), nil
}
