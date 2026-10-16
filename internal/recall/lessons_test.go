package recall

import (
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepeatedModelLessonsRemainCandidates(t *testing.T) {
	var all []Lesson
	add := func(rule, session string) {
		all = merge(all, Lesson{Rule: rule, Scope: "repo", Origin: "기록"}, session, "2026-10-07")
	}
	add("기능을 지운 뒤 관련 주석도 함께 고친다", "s1")
	add("기능을 지운 뒤 관련 주석도 함께 고친다", "s1")
	if len(all) != 1 || all[0].Status != "후보" || len(all[0].Sessions) != 1 {
		t.Fatal("same session counted twice", all)
	}
	add("기능을 지운 뒤에는 관련 주석도 함께 고친다", "s2")
	if len(all) != 1 || all[0].Status != "후보" {
		t.Fatal("rephrased lesson not merged or became a rule", all)
	}
	add("커밋 전에 tsc 를 돌린다", "s3")
	if len(all) != 2 {
		t.Fatal(all)
	}
}

func TestImportedMemoryNeedsAPerson(t *testing.T) {
	home, _ := fixture(t)
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(home, "vault"))
	for i, name := range []string{"a.md", "b.md"} {
		write(t, filepath.Join(home, ".claude-profiles", "work", "projects", "p"+string(rune('0'+i)), "memory", name),
			"---\nname: x\ndescription: 응답은 한국어로 짧게 쓴다\nmetadata:\n  type: feedback\n---\nbody\n")
	}
	out, err := ImportMemory(time.Date(2026, 10, 7, 0, 0, 0, 0, time.Local))
	if err != nil || out["total"] != 2 || out["active"] != 0 {
		t.Fatal(out, err)
	}
	all, _ := loadLessons()
	// Different memory identities remain separate candidates even with identical descriptions.
	if len(all) != 2 {
		t.Fatal(all)
	}
	for _, lesson := range all {
		if lesson.Status != "후보" || lesson.Confirmed || lesson.Scope != "미확인" || len(lesson.Sessions) != 1 || len(lesson.Sources) != 1 {
			t.Fatal("imported memories merged or activated on their own", all)
		}
	}
	if lines := CardLines(home); len(lines) != 1 {
		t.Fatal("candidate reached the card", lines)
	}
	all[0].Status, all[0].Confirmed = "활성", true
	saveLessons(all)
	if lines := CardLines(home); len(lines) != 1 {
		t.Fatal("confirmation alone inferred the memory scope", lines)
	}
	all[0].Scope = "전체"
	saveLessons(all)
	if lines := CardLines(home); len(lines) != 3 || !strings.Contains(lines[1], "한국어") {
		t.Fatal(lines)
	}
	if raw, err := os.ReadFile(filepath.Join(Dir(), "배운것.md")); err != nil || !strings.Contains(string(raw), "한국어") {
		t.Fatal(string(raw), err)
	}
}

func TestRelearnCountsEachSessionOnce(t *testing.T) {
	home, _ := fixture(t)
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(home, "vault"))
	saveLessons([]Lesson{{Rule: "같은 세션을 두 이름으로 센 교훈", Scope: "repo", Origin: "기록", Sessions: []string{"01a0f9ee", "47dfcad4"}, Status: "활성"}})
	record := func(name, id, rule string) {
		s := session{tool: "codex", id: id, cwd: "/work/repo", start: time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC), requests: []worklog.Request{{Text: "요청"}}}
		write(t, filepath.Join(RecordDir(), "2026-10-06", name), render(s, summary{Title: "t", Lessons: []string{rule}}, true))
	}
	record("0100-repo-a.md", "01a0f9ee-0000-7000-8000-00000000aaaa", "커밋 전에 tsc 를 돌린다")
	record("0200-repo-b.md", "01a0f9ee-0000-7000-8000-00000000bbbb", "커밋 전에는 tsc 를 돌린다")
	out, err := Relearn(time.Date(2026, 10, 7, 0, 0, 0, 0, time.Local))
	if err != nil || out["total"] != 2 || out["active"] != 0 {
		t.Fatal(out, err)
	}
	all, _ := loadLessons()
	all = all[1:]
	if all[0].Rule != "커밋 전에 tsc 를 돌린다" || strings.Join(all[0].Sessions, ",") != "01a0f9ee-0000-7000-8000-00000000aaaa,01a0f9ee-0000-7000-8000-00000000bbbb" {
		t.Fatal(all)
	}
}
