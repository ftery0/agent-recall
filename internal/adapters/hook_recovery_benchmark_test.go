package adapters

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkCaptureRetrievalExclusion(b *testing.B) {
	for _, tc := range []struct {
		name, command string
		bytes         int
	}{
		{"pure-1KiB", "harness memory brief --task a", 1 << 10},
		{"pure-1MiB", "harness memory brief --task a", 1 << 20},
		{"compound-1MiB", "harness memory brief --task a; cat result.txt", 1 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			root := b.TempDir()
			c := Config{Schema: 1, Category: "personal", Project: "project", ProjectRoot: root, DataRoot: filepath.Join(root, "data"), Tasks: []string{"a"}, SourceRoots: []string{root}}
			s, e := c.Store(true)
			if e != nil {
				b.Fatal(e)
			}
			if e = s.Bind("codex", "session", "a", root); e != nil {
				b.Fatal(e)
			}
			payload := memory.Object{"hook_event_name": "PostToolUse", "session_id": "session", "cwd": root, "tool_use_id": "same-occurrence", "tool_input": memory.Object{"command": tc.command}, "tool_response": strings.Repeat("x", tc.bytes)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, e := Capture(c, "codex", payload); e != nil {
					b.Fatal(e)
				}
			}
		})
	}
}
