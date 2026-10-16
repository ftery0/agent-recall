package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type Config struct {
	Schema      int      `json:"schema"`
	Category    string   `json:"category"`
	Project     string   `json:"project"`
	ProjectRoot string   `json:"project_root"`
	DataRoot    string   `json:"data_root"`
	Tasks       []string `json:"tasks"`
	SourceRoots []string `json:"source_roots"`
	Aliases     []string `json:"project_aliases,omitempty"`
	ExactCWDs   []string `json:"exact_cwds,omitempty"`
	configPath  string
}

func Load(path, project, cwd string) (Config, error) {
	raw, e := os.ReadFile(path)
	if e != nil {
		return Config{}, e
	}
	var index struct {
		Schema   int      `json:"schema"`
		Projects []Config `json:"projects"`
	}
	if e = json.Unmarshal(raw, &index); e != nil {
		return Config{}, e
	}
	var c Config
	if index.Schema == 2 {
		found := false
		for _, item := range index.Projects {
			if (project != "" && item.Project == project) || (project == "" && item.Permitted(cwd)) {
				if found {
					return c, errors.New("ambiguous project; use --project")
				}
				c = item
				found = true
			}
		}
		if !found {
			return c, errors.New("project outside configured scope")
		}
	} else {
		if e = json.Unmarshal(raw, &c); e != nil {
			return c, e
		}
		if project != "" && project != c.Project {
			return c, errors.New("project mismatch")
		}
	}
	// company 는 사용자가 직접 등록한 회사 프로젝트다. 자동 등록은 하지 않는다.
	if c.Schema != 1 || (c.Category != "personal" && c.Category != "company") {
		return c, errors.New("only selected personal or company projects are enabled")
	}
	if e = memory.Identifier(c.Project); e != nil {
		return c, e
	}
	if !filepath.IsAbs(c.ProjectRoot) || !filepath.IsAbs(c.DataRoot) {
		return c, errors.New("absolute config paths required")
	}
	for _, exact := range c.ExactCWDs {
		if !filepath.IsAbs(exact) {
			return c, errors.New("absolute exact cwd required")
		}
	}
	for _, task := range c.Tasks {
		if e = memory.Identifier(task); e != nil {
			return c, e
		}
	}
	c.configPath, e = filepath.Abs(path)
	if e != nil {
		return c, e
	}
	return c, nil
}
func (c Config) Permitted(cwd string) bool {
	if cwd == "" {
		return false
	}
	for _, root := range append([]string{c.ProjectRoot}, c.Aliases...) {
		if memory.Within(root, cwd) {
			return true
		}
	}
	resolved, e := memory.Resolve(cwd)
	if e != nil {
		return false
	}
	for _, exact := range c.ExactCWDs {
		if !filepath.IsAbs(exact) {
			continue
		}
		allowed, e := memory.Resolve(exact)
		if e == nil && allowed == resolved {
			return true
		}
	}
	return false
}

func (c Config) memoryCommand() (string, error) {
	executable, e := os.Executable()
	if e != nil {
		return "", e
	}
	executable, e = filepath.Abs(executable)
	if e != nil {
		return "", e
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	command := quote(executable)
	if c.configPath != "" {
		command += " --config " + quote(c.configPath)
	}
	return command + " --project " + quote(c.Project) + " memory", nil
}
func (c Config) Task(task string) error {
	for _, id := range c.Tasks {
		if id == task {
			return nil
		}
	}
	return fmt.Errorf("task %q outside configured scope", task)
}
func (c Config) Store(init bool) (*memory.Store, error) {
	return memory.Open(c.DataRoot, c.Project, init)
}
func (c Config) Object() memory.Object {
	raw, _ := json.Marshal(c)
	v, _ := memory.Decode(raw)
	return v
}
func (c Config) SourceAllowed(path string) bool {
	if secret.MatchString(filepath.Base(path)) || regexp.MustCompile(`^\.env`).MatchString(filepath.Base(path)) {
		return false
	}
	for _, root := range append([]string{c.ProjectRoot}, c.SourceRoots...) {
		if memory.Within(root, path) {
			return true
		}
	}
	return false
}
