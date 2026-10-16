package policy

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func gitRoot(cwd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, e := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if e != nil {
		return "", e
	}
	return memory.Resolve(strings.TrimSpace(string(raw)))
}

// EditScope uses the existing Python state namespace and wire format. It never
// opens the memory database or changes task approval scope.
func EditScope(payload memory.Object, tool, runtime string) (memory.Object, error) {
	session := memory.Text(payload["session_id"])
	cwd := memory.Text(payload["cwd"])
	if session == "" || cwd == "" {
		return nil, errors.New("세션 또는 경로 없음")
	}
	if e := memory.Identifier(session); e != nil {
		return nil, e
	}
	if e := memory.Identifier(tool); e != nil {
		return nil, e
	}
	root, e := gitRoot(cwd)
	if e != nil {
		if memory.Text(payload["hook_event_name"]) != "Stop" {
			return nil, nil
		}
		marker := filepath.Join(runtime, "harness-edit-scope", memory.Hash([]byte(tool+":"+session+":nongit"))+".notice")
		if e = os.MkdirAll(filepath.Dir(marker), 0700); e != nil {
			return nil, e
		}
		f, e := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(e) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		if e = f.Close(); e != nil {
			return nil, e
		}
		return memory.Object{"systemMessage": "미확인: 현재 구조 검사는 Git 프로젝트의 편집 근거만 지원합니다."}, nil
	}
	c := Collector{Root: root, Runtime: runtime, Tool: tool, Session: session, Task: memory.Text(payload["task_id"]), compat: true}
	out, e := c.Event(payload)
	if e != nil {
		return nil, e
	}
	event := memory.Text(payload["hook_event_name"])
	if event == "Export" || event == "Clear" {
		return out, nil
	}
	transport := memory.Object{}
	for _, key := range []string{"decision", "reason", "systemMessage"} {
		if value, ok := out[key]; ok {
			transport[key] = value
		}
	}
	if len(transport) == 0 {
		return nil, nil
	}
	return transport, nil
}

func structurePaths(root string, paths []string) []string {
	out := []string{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		resolved, e := memory.Resolve(path)
		if e != nil || !memory.Within(root, resolved) {
			continue
		}
		path, _ = filepath.Rel(root, resolved)
		if !validRelative(path) || !code.MatchString(path) || skip.MatchString(path) {
			continue
		}
		if !has(out, path) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

func StructureMeasure(root string, paths []string) ([]StructureMetricsRecord, error) {
	l, e := thresholds()
	if e != nil {
		return nil, e
	}
	top, e := gitRoot(root)
	if e != nil {
		return []StructureMetricsRecord{}, nil
	}
	out := []StructureMetricsRecord{}
	for _, path := range structurePaths(top, paths) {
		m, e := measureLimits(filepath.Join(top, path), l)
		if e != nil {
			return nil, e
		}
		if m.Exists {
			out = append(out, StructureMetricsRecord{path, m})
		}
	}
	return out, nil
}

func StructureCheck(root string, paths []string, baselinePath string) (string, int, error) {
	l, e := thresholds()
	if e != nil {
		return "", 2, e
	}
	top, e := gitRoot(root)
	if e != nil {
		return "", 0, nil
	}
	selected := structurePaths(top, paths)
	b := baseline{Root: top, Files: map[string]Change{}, Directories: map[string]int{}}
	if baselinePath != "" {
		raw, e := readRegularFile(baselinePath)
		if e != nil {
			return "", 2, e
		}
		m, e := object(raw)
		if e != nil {
			return "", 2, e
		}
		var baselineRoot string
		if json.Unmarshal(m["root"], &baselineRoot) != nil || baselineRoot != top {
			return "", 2, errors.New("미확인: 편집 기준의 프로젝트가 다릅니다")
		}
		if e = validateFiles(m["files"], "after"); e != nil {
			return "", 2, e
		}
		if raw, ok := m["directories"]; ok {
			if e = validateDirectories(raw); e != nil {
				return "", 2, e
			}
		}
		if e = json.Unmarshal(raw, &b); e != nil {
			return "", 2, e
		}
		files := map[string]Change{}
		for _, path := range selected {
			item, ok := b.Files[path]
			if !ok {
				if _, e = os.Stat(filepath.Join(top, path)); os.IsNotExist(e) {
					continue
				}
				return "", 2, errors.New("미확인: 편집 기준을 읽지 못했습니다: " + path)
			}
			files[path] = item
		}
		b.Files = files
	} else {
		for _, path := range selected {
			after, e := fingerprint(filepath.Join(top, path))
			if e != nil {
				return "", 2, e
			}
			if after == "" {
				continue
			}
			before := Metrics{}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			raw, err := exec.CommandContext(ctx, "git", "-C", top, "show", "HEAD:"+path).Output()
			cancel()
			if err == nil {
				before = contentMetrics(raw, l)
			}
			b.Files[path] = Change{before, after}
			dir := filepath.Dir(path)
			if _, known := b.Directories[dir]; !known {
				n, e := componentCount(filepath.Join(top, dir))
				if e != nil {
					return "", 2, e
				}
				b.Directories[dir] = n
			}
			// HEAD-based manual mode uses today's actual component count, not an owned delta.
			if !before.Exists && component.MatchString(path) && !auxiliary.MatchString(path) {
				b.Directories[dir]--
			}
		}
	}
	for path, item := range b.Files {
		hash, e := fingerprint(filepath.Join(top, path))
		if e != nil {
			return "", 2, e
		}
		if hash != item.After {
			return "", 2, errors.New("미확인: 확인한 편집 후 다른 변경이 있음: " + path)
		}
	}
	key, e := checkKey(top, b)
	if e != nil {
		return "", 2, e
	}
	problems, e := violationsLimits(top, b.Files, b.Directories, l)
	if e != nil {
		return "", 2, e
	}
	for path, item := range b.Files {
		hash, e := fingerprint(filepath.Join(top, path))
		if e != nil {
			return "", 2, e
		}
		if hash != item.After {
			return "", 2, errors.New("미확인: 검사 중 파일이 바뀌어 소유권을 확인하지 못했습니다: " + path)
		}
	}
	later, e := checkKey(top, b)
	if e != nil {
		return "", 2, e
	}
	if later != key {
		return "", 2, errors.New("미확인: 검사 중 구조 근거가 바뀌었습니다")
	}
	output := structureOutput(problems)
	if output != "" {
		return output, 1, nil
	}
	return "", 0, nil
}
