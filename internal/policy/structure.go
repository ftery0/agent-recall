package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Metrics struct {
	Exists     bool `json:"exists"`
	Lines      int  `json:"lines"`
	Banners    int  `json:"banners"`
	Longs      int  `json:"longs"`
	Components int  `json:"components"`
}
type StructureMetricsRecord struct {
	Path string `json:"path"`
	Metrics
}
type Change struct {
	Before Metrics `json:"before"`
	After  string  `json:"after"`
}

func (c Change) MarshalJSON() ([]byte, error) {
	var after any = c.After
	if c.After == "" {
		after = nil
	}
	return json.Marshal(struct {
		Before Metrics `json:"before"`
		After  any     `json:"after"`
	}{c.Before, after})
}

type limits struct{ Files, Lines, Comments int }

func thresholds() (limits, error) {
	l := limits{8, 400, 6}
	for key, target := range map[string]*int{"STRUCTURE_MAX_FILES": &l.Files, "STRUCTURE_MAX_LINES": &l.Lines, "STRUCTURE_MAX_COMMENT_LINES": &l.Comments} {
		raw := os.Getenv(key)
		if raw == "" {
			continue
		}
		if !regexp.MustCompile(`^[0-9]+$`).MatchString(raw) {
			return l, fmt.Errorf("미확인: %s는 0 이상의 정수여야 합니다", key)
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return l, fmt.Errorf("미확인: %s 정수 범위를 확인하세요", key)
		}
		*target = value
	}
	return l, nil
}

var bannerPattern = regexp.MustCompile(`^\s*//\s*(={3,}|-{3,}|─{3,}|═{3,}|#{3,}|\*{3,}|~{3,})`)
var helperDirectory = regexp.MustCompile(`(^|/)(lib|libs|util|utils|helper|helpers)/`)
var scriptFile = regexp.MustCompile(`\.(ts|tsx|js|jsx|mjs|cjs)$`)
var helperIgnored = regexp.MustCompile(`^\s*($|//|/\*|\*|import\s|export\s+\*\s+from)`)
var exportedName = regexp.MustCompile(`export[ \t]+(?:async[ \t]+)?(?:function|const|let|class)[ \t]+([A-Za-z_$][A-Za-z0-9_$]*)`)

func openRegularFile(path string, flags int) (*os.File, error) {
	// A nonblocking open also handles replacement by a FIFO between lookup and open.
	f, e := os.OpenFile(path, flags|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil || !info.Mode().IsRegular() {
		f.Close()
		if e == nil {
			e = fmt.Errorf("not a regular file: %s", path)
		}
		return nil, e
	}
	return f, nil
}
func readRegularFile(path string) ([]byte, error) {
	f, e := openRegularFile(path, os.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	return io.ReadAll(f)
}
func fingerprint(path string) (string, error) {
	b, e := readRegularFile(path)
	if os.IsNotExist(e) {
		return "", nil
	}
	if e != nil {
		return "", e
	}
	return memory.Hash(b), nil
}
func componentCount(directory string) (int, error) {
	entries, e := os.ReadDir(directory)
	if os.IsNotExist(e) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	n := 0
	for _, entry := range entries {
		if component.MatchString(entry.Name()) && !auxiliary.MatchString(entry.Name()) {
			info, e := entry.Info()
			if e != nil {
				return 0, e
			}
			if info.Mode().IsRegular() {
				n++
			}
		}
	}
	return n, nil
}
func contentMetrics(raw []byte, l limits) Metrics {
	m := Metrics{Exists: true, Lines: strings.Count(string(raw), "\n")}
	run, block := 0, 0
	inBlock := false
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		trim := strings.TrimSpace(line)
		if bannerPattern.MatchString(line) {
			m.Banners++
		}
		if strings.HasPrefix(trim, "/*") {
			inBlock = true
			block = 0
		}
		if inBlock {
			block++
		}
		if inBlock && strings.Contains(line, "*/") {
			if block > l.Comments {
				m.Longs++
			}
			inBlock = false
		}
		if strings.HasPrefix(trim, "//") {
			run++
		} else {
			if run > l.Comments {
				m.Longs++
			}
			run = 0
		}
	}
	if run > l.Comments {
		m.Longs++
	}
	return m
}
func measure(path string) (Metrics, error) {
	l, e := thresholds()
	if e != nil {
		return Metrics{}, e
	}
	return measureLimits(path, l)
}
func measureLimits(path string, l limits) (Metrics, error) {
	m := Metrics{}
	if !code.MatchString(path) {
		info, e := os.Stat(path)
		if os.IsNotExist(e) {
			return m, nil
		}
		if e != nil {
			return m, e
		}
		m.Exists = info.Mode().IsRegular()
		return m, nil
	}
	n, e := componentCount(filepath.Dir(path))
	if e != nil {
		return m, e
	}
	m.Components = n
	b, e := readRegularFile(path)
	if os.IsNotExist(e) {
		return m, nil
	}
	if e != nil {
		return m, e
	}
	m = contentMetrics(b, l)
	m.Components = n
	return m, nil
}
func helperName(root, path string) (string, error) {
	raw, e := readRegularFile(filepath.Join(root, path))
	if e != nil {
		return "", e
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if !helperIgnored.MatchString(line) {
			count++
		}
	}
	if count > 5 {
		return "", nil
	}
	names := map[string]bool{}
	for _, m := range exportedName.FindAllStringSubmatch(string(raw), -1) {
		names[m[1]] = true
	}
	if len(names) != 1 {
		return "", nil
	}
	for n := range names {
		return n, nil
	}
	return "", nil
}
func helperUsers(root, name, path string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	program := "rg"
	args := []string{"-l", "-w", "-F", "--glob", "!**/node_modules/**", "--", name, "."}
	if _, e := exec.LookPath(program); e != nil {
		program = "grep"
		args = []string{"-rlwF", "--exclude-dir=node_modules", "--exclude-dir=.git", "--", name, "."}
	}
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = root
	raw, e := cmd.Output()
	if e != nil {
		var exit *exec.ExitError
		if !errors.As(e, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("helper importer metrics unavailable: %w", e)
		}
	}
	users := []string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		line = strings.TrimPrefix(line, "./")
		if line != "" && line != path && !auxiliary.MatchString(line) {
			users = append(users, line)
		}
	}
	sort.Strings(users)
	return users, nil
}
func tinyHelper(root, path string) (bool, error) {
	name, e := helperName(root, path)
	if e != nil || name == "" {
		return false, e
	}
	users, e := helperUsers(root, name, path)
	return len(users) <= 1, e
}
func violations(root string, files map[string]Change, dirs map[string]int) ([]string, error) {
	l, e := thresholds()
	if e != nil {
		return nil, e
	}
	return violationsLimits(root, files, dirs, l)
}
func violationsLimits(root string, files map[string]Change, dirs map[string]int, l limits) ([]string, error) {
	out := []string{}
	reported := map[string]bool{}
	paths := []string{}
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		change := files[path]
		if !code.MatchString(path) || skip.MatchString(path) {
			continue
		}
		after, e := measureLimits(filepath.Join(root, path), l)
		if e != nil {
			return nil, e
		}
		if !after.Exists {
			continue
		}
		before := change.Before
		dir := filepath.Dir(path)
		if !before.Exists && component.MatchString(path) && !auxiliary.MatchString(path) && !reported[dir] {
			n, known := dirs[dir]
			if !known {
				n = before.Components
			}
			for other, c := range files {
				if filepath.Dir(other) == dir && component.MatchString(other) && !auxiliary.MatchString(other) {
					if !c.Before.Exists && c.After != "" {
						n++
					}
					if c.Before.Exists && c.After == "" {
						n--
					}
				}
			}
			if n > l.Files {
				out = append(out, fmt.Sprintf("%s/ 에 컴포넌트 파일이 %d개다 (기준 %d개, 새 파일 %s). 함께 쓰이는 파일끼리 하위 폴더로 묶어라.", dir, n, l.Files, path))
				reported[dir] = true
			}
		}
		if before.Lines <= l.Lines && after.Lines > l.Lines {
			out = append(out, fmt.Sprintf("%s 이 이번에 %d줄이 됐다 (기준 %d줄). 역할별로 나눠 폴더로 묶어라.", path, after.Lines, l.Lines))
		}
		if after.Banners > before.Banners {
			out = append(out, fmt.Sprintf("%s 에 구분선 주석이 %d개 늘었다. 지워라 — 구분이 필요하면 파일을 나눈다.", path, after.Banners-before.Banners))
		}
		if after.Longs > before.Longs {
			out = append(out, fmt.Sprintf("%s 에 %d줄 넘는 주석 블록이 %d개 늘었다. 코드가 말하지 못하는 '왜' 한 줄만 남겨라.", path, l.Comments, after.Longs-before.Longs))
		}
		if !before.Exists && helperDirectory.MatchString(path) && scriptFile.MatchString(path) && !auxiliary.MatchString(path) {
			small, e := tinyHelper(root, path)
			if e != nil {
				return nil, e
			}
			if small {
				out = append(out, path+" 은 한 곳에서만 쓰는 작은 헬퍼 파일이다. 쓰는 곳에 인라인하라.")
			}
		}
	}
	return out, nil
}
func structureOutput(problems []string) string {
	if len(problems) == 0 {
		return ""
	}
	return "[구조 검사] 이번에 바뀐 파일에서 코드 구조 규칙 위반을 찾았다.\n- " + strings.Join(problems, "\n- ") + "\n이번 요청 밖의 기존 코드는 고치지 않는다. 일부러 이렇게 두는 거면 보고에 이유를 한 줄 적는다.\n"
}
