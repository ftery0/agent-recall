package main

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBriefCLIHelperProcess(t *testing.T) {
	if os.Getenv("HARNESS_BRIEF_TEST_PROCESS") != "1" {
		return
	}
	for i, argument := range os.Args {
		if argument == "--" {
			os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestBriefCLIEncodingAndLegacyRecall(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	data := filepath.Join(root, "data")
	if e := os.WriteFile(config, memory.Packed(memory.Object{"schema": 1, "category": "personal", "project": "p", "project_root": root, "data_root": data, "tasks": []string{"t"}}), 0600); e != nil {
		t.Fatal(e)
	}
	s, e := memory.Open(data, "p", true)
	if e != nil {
		t.Fatal(e)
	}
	body := strings.Repeat("한국어 <content>🙂\x00", 100)
	if _, e = s.Ingest(memory.Object{"key": "state", "task": "t", "session": "original", "kind": "state", "subject": "task", "body": body, "status": "active", "expected_revision": 0}); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"brief", "recall"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBriefCLIHelperProcess$", "--", "--config", config, "memory", action, "--task", "t")
		cmd.Env = append(os.Environ(), "HARNESS_BRIEF_TEST_PROCESS=1")
		cmd.Dir = root
		output, e := cmd.Output()
		if e != nil {
			t.Fatal(action, e)
		}
		result, e := memory.Decode(output)
		if e != nil {
			t.Fatal(e)
		}
		record := result["current"].([]any)[0].(map[string]any)
		if record["body"] != body || !strings.Contains(string(output), "<content>") {
			t.Fatal("CLI changed body or HTML encoding", action)
		}
		if action == "brief" {
			if strings.Count(string(output), "\n") != 1 || memory.Number(result["display_chars"]) != int64(utf8.RuneCount(output)) || record["session"] != nil || record["created"] == nil {
				t.Fatal("compact display or metadata contract changed", result)
			}
		} else if !strings.Contains(string(output), "\n  \"") || record["session"] != "original" || memory.Number(record["expected_revision"]) != 0 || memory.Number(record["applied"]) != 1 {
			t.Fatal("legacy raw recall output changed", result)
		}
	}
}
