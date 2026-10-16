package policy

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"path/filepath"
	"regexp"
	"strings"
)

var code = regexp.MustCompile(`\.(ts|tsx|js|jsx|mjs|cjs|vue|svelte|go|py|gd|swift|kt|dart|rs|java|rb|php)$`)
var skip = regexp.MustCompile(`(^|/)(node_modules|dist|build|out|coverage|vendor|generated|__generated__|migrations|\.next)/|\.d\.ts$|\.min\.js$|(_gen|\.pb)\.go$`)
var component = regexp.MustCompile(`\.(tsx|jsx|vue|svelte)$`)
var auxiliary = regexp.MustCompile(`(^|/)index\.[a-z]+$|\.(test|spec|stories)\.[a-z]+$`)

func Targets(payload memory.Object, root string) ([]string, map[string]string, bool) {
	if normalized, err := memory.Resolve(root); err == nil {
		root = normalized
	}
	inputs, _ := payload["tool_input"].(map[string]any)
	tool := memory.Text(payload["tool_name"])
	cwd := memory.Text(payload["cwd"])
	var paths []string
	moves := map[string]string{}
	complete := true
	switch tool {
	case "Edit", "Write":
		paths = []string{memory.Text(inputs["file_path"])}
	case "apply_patch":
		command := memory.Text(inputs["command"])
		if command == "" {
			command = memory.Text(inputs["patch"])
		}
		original := ""
		for _, line := range strings.Split(command, "\n") {
			for _, prefix := range []string{"*** Add File: ", "*** Update File: ", "*** Delete File: ", "*** Move to: "} {
				if strings.HasPrefix(line, prefix) {
					path := strings.TrimPrefix(line, prefix)
					paths = append(paths, path)
					if prefix == "*** Update File: " {
						original = path
					}
					if prefix == "*** Move to: " && original != "" {
						moves[path] = original
					}
				}
			}
		}
		complete = len(paths) > 0
	case "Bash", "exec_command":
		command := memory.Text(inputs["command"])
		if command == "" {
			command = memory.Text(inputs["cmd"])
		}
		paths, complete = shellTargets(command)
	default:
		return []string{}, moves, false
	}
	resolved := []string{}
	normalize := func(path string) string {
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		p, e := memory.Resolve(path)
		if e != nil || !memory.Within(root, p) {
			return ""
		}
		rel, _ := filepath.Rel(root, p)
		return rel
	}
	for _, path := range paths {
		if path == "" || strings.ContainsAny(path, "\n\x00") {
			complete = false
			continue
		}
		rel := normalize(path)
		if rel == "" {
			continue
		}
		if !skip.MatchString(rel) {
			if !has(resolved, rel) {
				resolved = append(resolved, rel)
			}
		}
	}
	normalizedMoves := map[string]string{}
	for dest, source := range moves {
		d, s := normalize(dest), normalize(source)
		if d != "" && s != "" {
			normalizedMoves[d] = s
		}
	}
	return resolved, normalizedMoves, complete
}
func has(a []string, v string) bool {
	for _, s := range a {
		if s == v {
			return true
		}
	}
	return false
}
func shellTargets(command string) ([]string, bool) {
	// Shell programs are not a security boundary; only literal, single commands are attributed.
	if strings.ContainsAny(command, "\n;&|`$") || strings.Contains(command, "<<") {
		return nil, false
	}
	words, ok := literalWords(command)
	if !ok || len(words) == 0 {
		return nil, false
	}
	program := filepath.Base(words[0])
	targets := []string{}
	for i, w := range words {
		if w == ">" || w == ">>" {
			if i+1 == len(words) {
				return nil, false
			}
			if !strings.HasPrefix(words[i+1], "/dev/") {
				targets = append(targets, words[i+1])
			}
		}
	}
	switch program {
	case "cat", "printf", "echo":
	case "pwd", "ls", "rg", "grep", "head", "tail", "wc", "git":
		if program == "git" {
			for _, option := range words[1:] {
				if option == "--output" || strings.HasPrefix(option, "--output=") {
					return targets, false
				}
			}
		}
		if len(targets) == 0 {
			return nil, program != "git" || len(words) > 1 && (words[1] == "status" || words[1] == "diff" || words[1] == "log" || words[1] == "show")
		}
	case "tee":
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "-") {
				continue
			}
			targets = append(targets, w)
		}
	case "sed":
		// sed scripts can write additional files or execute commands. Without a
		// complete script parser, even a single apparent target is unverified.
		return targets, false
	default:
		return targets, false
	}
	for _, p := range targets {
		if strings.ContainsAny(p, "*?{}~") {
			return targets, false
		}
	}
	return targets, true
}
func literalWords(command string) ([]string, bool) {
	words := []string{}
	var b strings.Builder
	quote := rune(0)
	escape := false
	active := false
	flush := func() {
		if active {
			words = append(words, b.String())
			b.Reset()
			active = false
		}
	}
	for _, r := range command {
		if escape {
			b.WriteRune(r)
			escape = false
			active = true
			continue
		}
		if r == '\\' && quote != '\'' {
			escape = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				b.WriteRune(r)
			}
			active = true
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			active = true
			continue
		}
		if r == ' ' || r == '\t' {
			flush()
			continue
		}
		if r == '>' {
			flush()
			if len(words) > 0 && words[len(words)-1] == ">" {
				words[len(words)-1] = ">>"
			} else {
				words = append(words, ">")
			}
			continue
		}
		b.WriteRune(r)
		active = true
	}
	flush()
	return words, quote == 0 && !escape
}
