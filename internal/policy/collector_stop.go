package policy

import (
	"context"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func validCheck(output string, code int) bool {
	return code == 0 && output == "" || code == 1 && strings.Contains(output, "[구조 검사]")
}
func checkKey(root string, b baseline) (string, error) {
	rules := map[string]string{}
	exe, e := os.Executable()
	if e != nil {
		return "", e
	}
	rules[exe], e = fingerprint(exe)
	if e != nil {
		return "", e
	}
	if override := os.Getenv("HARNESS_STRUCTURE_CHECK"); override != "" {
		rules[override], e = fingerprint(override)
		if e != nil {
			return "", e
		}
	}
	dirs := map[string][]string{}
	users := map[string][]string{}
	for path, item := range b.Files {
		parent := filepath.Dir(filepath.Join(root, path))
		if _, ok := dirs[parent]; !ok {
			rows, e := os.ReadDir(parent)
			if e != nil && !os.IsNotExist(e) {
				return "", e
			}
			names := []string{}
			for _, entry := range rows {
				if !entry.IsDir() && component.MatchString(entry.Name()) {
					names = append(names, entry.Name())
				}
			}
			sort.Strings(names)
			dirs[parent] = names
		}
		if !item.Before.Exists && scriptFile.MatchString(path) && helperDirectory.MatchString(path) && !auxiliary.MatchString(path) && item.After != "" {
			name, e := helperName(root, path)
			if e != nil {
				return "", e
			}
			if name != "" {
				rows, e := helperUsers(root, name, path)
				if e != nil {
					return "", e
				}
				users[path+":"+name] = rows
			}
		}
	}
	env := map[string]string{}
	for _, item := range os.Environ() {
		key, value, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "STRUCTURE_") {
			env[key] = value
		}
	}
	return memory.Hash(memory.Packed([]any{b, rules, dirs, users, env})), nil
}

func (c Collector) stop(state *EditState, b baseline, notes []string) (memory.Object, error) {
	paths := []string{}
	for path := range b.Files {
		paths = append(paths, filepath.Join(c.Root, path))
	}
	sort.Strings(paths)
	// Stop reports edit evidence and structure. Project checks require an explicit invocation.
	out := memory.Object{"verified_files": len(b.Files), "files": paths, "checks": []memory.Object{}, "checks_status": "not_run", "violations": []string{}, "structure": "", "unverified": notes, "edit_evidence_verified": len(notes) == 0}
	structure := ""
	if len(paths) > 0 {
		if _, e := thresholds(); e != nil {
			return nil, e
		}
		key, e := checkKey(c.Root, b)
		if e != nil {
			return nil, e
		}
		output, exitCode := "", 0
		if len(notes) == 0 && state.Cache != nil && state.Cache.Key == key {
			output, exitCode = state.Cache.Output, state.Cache.Code
		} else if override := os.Getenv("HARNESS_STRUCTURE_CHECK"); override != "" {
			baselinePath := strings.TrimSuffix(c.path(), ".json") + ".baseline.json"
			if e = memory.PrivateWrite(baselinePath, memory.Packed(b)); e != nil {
				return nil, e
			}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			cmd := exec.CommandContext(ctx, override, append([]string{c.Root}, paths...)...)
			cmd.Env = append(os.Environ(), "STRUCTURE_BASELINE_FILE="+baselinePath)
			raw, runErr := cmd.CombinedOutput()
			timed := ctx.Err() != nil
			cancel()
			if timed {
				return nil, errors.New("구조 검사 timeout")
			}
			output = string(raw)
			if runErr != nil {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) {
					return nil, runErr
				}
				exitCode = exit.ExitCode()
			}
		} else {
			v, e := violations(c.Root, b.Files, b.Directories)
			if e != nil {
				return nil, e
			}
			output = structureOutput(v)
			if output != "" {
				exitCode = 1
			}
		}
		changed := []string{}
		for path, item := range b.Files {
			hash, e := fingerprint(filepath.Join(c.Root, path))
			if e != nil {
				return nil, e
			}
			if hash != item.After {
				changed = append(changed, path)
			}
		}
		later, e := checkKey(c.Root, b)
		if e != nil {
			return nil, e
		}
		if len(changed) > 0 || later != key {
			state.Cache = nil
			note := "검사 중 파일 또는 구조 근거가 바뀌어 소유권 미확인"
			if len(changed) > 0 {
				sort.Strings(changed)
				note += ": " + strings.Join(changed, ", ")
			}
			state.addUnverified(memory.Hash(memory.Packed([]any{"structure_evidence", key, later, changed})), note)
			out["unverified"] = unique(append(notes, note))
			out["edit_evidence_verified"] = false
			out["systemMessage"] = "미확인: 검사 중 파일 또는 구조 근거가 바뀌어 이번 구조 결과를 확인하지 못했습니다."
			return out, nil
		}
		if len(notes) == 0 && validCheck(output, exitCode) {
			state.Cache = &checkCache{key, output, exitCode}
		} else {
			state.Cache = nil
		}
		if !validCheck(output, exitCode) {
			note := fmt.Sprintf("구조 검사 종료 상태 %d. %s", exitCode, output)
			state.addUnverified(memory.Hash(memory.Packed([]any{"structure_exit", key, exitCode, output})), note)
			out["unverified"] = unique(append(notes, note))
			out["edit_evidence_verified"] = false
			out["systemMessage"] = fmt.Sprintf("미확인: 구조 검사가 비정상 종료했습니다(종료 상태 %d).", exitCode)
			return out, nil
		}
		// Structure limits need judgement, so they are reported but never block the turn.
		if output != "" {
			structure = strings.TrimSpace(output)
		}
		out["structure"] = structure
		if structure != "" {
			out["systemMessage"] = structure
		}
		complete := true
		for path := range state.Files {
			if code.MatchString(path) && !skip.MatchString(path) {
				if _, ok := b.Files[path]; !ok {
					complete = false
					break
				}
			}
		}
		// A clean subset cannot resolve a notice while other structure evidence is unavailable.
		if structure != "" || complete {
			c.notice(state, out)
		}
	}
	return out, nil
}

// notice handles confirmed structure results only; unknown or failed checks cannot resolve a notice.
func (c Collector) notice(state *EditState, out memory.Object) {
	message := memory.Text(out["systemMessage"])
	if message == "" {
		state.LastNotice = ""
		return
	}
	key := memory.Hash([]byte(message))
	if state.LastNotice == key {
		delete(out, "systemMessage")
	}
	state.LastNotice = key
}

func unique(values []string) []string {
	m := map[string]bool{}
	for _, s := range values {
		if s != "" {
			m[s] = true
		}
	}
	out := []string{}
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
