package worklog

import (
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	maxRequests, requestChars = 30, 120
	maxReports, reportChars   = 2, 500
	maxTodos, todoDays        = 10, 7
	maxNotes, maxTotal        = 2, 6000
)

// Opus 5.5 drifts into English in the notes it writes right after tool results and after
// compaction, even with the language setting and the CLAUDE.md rules (issues #96326, #99800).
// These lines put Korean back at the exact spots where it drifts.
const (
	batchReminder = "[언어] 진행 안내와 사용자에게 하는 설명은 한국어로 쓴다. 사용자가 요청한 영어 산출물·번역·코드·원문은 그대로 둔다."
	compactLine   = "=== 압축·이어 열기 뒤다. 위 요약이나 앞 대화가 영어여도 답과 진행 안내는 한국어로 쓴다 ==="
	// CompactInstruction is what the PreCompact hook prints. Claude Code 2.1.292 appends that
	// output to the summary prompt as "Additional Instructions" — read from its code, not
	// documented — so an update can silently drop it; `recall lang` shows whether summaries
	// still carry SummaryMarker.
	CompactInstruction = "요약은 한국어로 쓴다. <summary> 안 첫 줄은 `" + SummaryMarker + "` 로 쓰고, 설명과 절 제목도 한국어로 쓴다. 코드·파일 경로·명령·오류 메시지·인용한 원문은 원래 글자 그대로 둔다."
	SummaryMarker      = "요약 언어: 한국어"
)

var openTodo = regexp.MustCompile(`^- \[ \] \S`)

func projectDir(tool string, payload memory.Object) string {
	if dir := os.Getenv("CLAUDE_PROJECT_DIR"); tool == "claude" && dir != "" {
		return dir
	}
	return memory.Text(payload["cwd"])
}

// Start is the session-start card. A folder that is neither a repository nor named in any
// note's repos gets nothing, so casual sessions stay quiet — except the Korean line after a
// compaction or resume, which every folder gets.
func Start(tool string, payload memory.Object) string {
	var out []string
	if source := memory.Text(payload["source"]); tool == "claude" && (source == "compact" || source == "resume") {
		out = append(out, compactLine)
	}
	cwd := projectDir(tool, payload)
	if cwd == "" {
		return strings.Join(out, "\n")
	}
	r := findRepo(cwd)
	vault := VaultDir()
	notes := findNotes(vault, r)
	if !r.git && len(notes) == 0 {
		return strings.Join(out, "\n")
	}
	if r.company {
		out = append(out, "=== 작업 구분: 회사 레포 — 옵시디언은 company/, 회사 노션 사용 ===")
	} else {
		out = append(out, "=== 작업 구분: 개인 — 옵시디언은 personal/10_프로젝트/<프로젝트>/ 에만 쓰고, 회사 노션·회사 데일리는 건드리지 않는다 ===")
	}
	out = append(out, noteLines(vault, r, notes)...)
	if r.git {
		out = append(out, memoryLines(tool, payload, r.root)...)
		branch, _ := Git(r.root, gitWait, "rev-parse", "--abbrev-ref", "HEAD")
		head, _ := Git(r.root, gitWait, "rev-parse", "--short", "HEAD")
		out = append(out, "=== git — "+branch+" · "+head+" ===")
		if payload["recall_context"] != true {
			log, _ := Git(r.root, gitWait, "log", "--oneline", "-10")
			out = append(out, log)
		}
		if status, _ := Git(r.root, gitWait, "status", "-s"); status != "" {
			lines := strings.Split(status, "\n")
			limit := 30
			if payload["recall_context"] == true {
				limit = 10
			}
			out = append(out, fmt.Sprintf("=== git status -s (%d줄) ===", limit))
			out = append(out, lines[:min(len(lines), limit)]...)
		}
	}
	if r.company && payload["recall_context"] != true {
		out = append(out, todoLines(time.Now())...)
	}
	var requests, reports []string
	if payload["recall_context"] != true {
		requests, reports = digest(tool, payload, cwd)
	}
	render := func() string {
		lines := append([]string{}, out...)
		if len(requests) > 0 {
			lines = append(lines, fmt.Sprintf("=== 이전 요청 — 사람이 친 것 최근 %d개. 아직 안 끝난 요청이 없는지 노트의 다음 할 일과 대조한다 ===", len(requests)))
			lines = append(lines, requests...)
			lines = append(lines, fmt.Sprintf("=== 마지막 보고 %d개 ===", len(reports)))
			lines = append(lines, reports...)
		}
		return strings.Join(lines, "\n")
	}
	for len([]rune(render())) > maxTotal && len(requests) > 0 {
		requests = requests[1:]
	}
	return cut(render(), maxTotal)
}

func noteLines(vault string, r repo, notes []note) []string {
	var lines []string
	folders := map[string]bool{}
	var order []string
	for _, n := range notes {
		if n.status != "진행" && n.status != "보류" {
			continue
		}
		if len(lines) == maxNotes {
			break
		}
		if !folders[n.folder] {
			folders[n.folder] = true
			order = append(order, n.folder)
		}
		if n.status == "진행" {
			next := n.next
			if next == "" {
				next = "(없음)"
			}
			line := fmt.Sprintf("%s — 다음: %s (%s)", cut(n.title, 80), cut(next, 160), n.mod.Format("01-02 15:04"))
			if r.git {
				line += mergedSince(r.root, n)
			}
			lines = append(lines, line)
		} else {
			lines = append(lines, cut(n.title, 80)+" (보류)")
		}
	}
	if len(order) > 0 {
		return append([]string{"=== 진행 중 작업 — " + strings.Join(order, ", ") + " ==="}, lines...)
	}
	if len(notes) > 0 {
		return []string{"=== 진행 중 작업 없음 — 노트는 " + notes[0].folder + " ==="}
	}
	if r.company {
		return []string{"=== 이 레포의 작업 기록 노트 없음 — 새로 쓸 곳: company/30_자료/작업 기록/<제품>/ (지금 제품 폴더: " +
			strings.Join(subdirs(filepath.Join(vault, "company", "30_자료", "작업 기록")), ", ") + " — 맞는 게 없으면 사용자에게 한 번 묻는다) ==="}
	}
	return []string{"=== 이 레포의 작업 기록 노트 없음 — 새로 쓸 곳: personal/10_프로젝트/<프로젝트>/작업 기록/ (기존 프로젝트 폴더에서 찾고, 없으면 레포 이름으로 만든다) ==="}
}

func subdirs(dir string) []string {
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return []string{"없음"}
	}
	return names
}

func todoLines(now time.Time) []string {
	days := map[string]bool{}
	for back := 0; back < todoDays; back++ {
		days[now.AddDate(0, 0, -back).Format("2006-01-02")+".md"] = true
	}
	var todos []string
	filepath.WalkDir(dailyDir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !days[d.Name()] {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if openTodo.MatchString(line) {
				todos = append(todos, d.Name()[5:10]+"  "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if len(todos) == 0 {
		return nil
	}
	return append([]string{fmt.Sprintf("=== 데일리 미완료 (최근 %d일) — 이전 작업을 물으면 여기부터 본다 ===", todoDays)}, todos[:min(len(todos), maxTodos)]...)
}

// digest restores what compaction or a new session drops: right after compaction always,
// after a new session or /clear only when the previous session asked for more after its last note write.
func digest(tool string, payload memory.Object, cwd string) ([]string, []string) {
	current := memory.Text(payload["transcript_path"])
	source := memory.Text(payload["source"])
	path := ""
	switch source {
	case "compact":
		path = current
	case "startup", "clear":
		path = previousTranscript(tool, current, cwd)
	}
	if path == "" {
		return nil, nil
	}
	t := readTranscript(path)
	if len(t.requests) == 0 {
		return nil, nil
	}
	if source != "compact" && !t.saved.Before(t.requests[len(t.requests)-1].At) {
		return nil, nil
	}
	requests := t.requests[max(0, len(t.requests)-maxRequests):]
	var lines, reports []string
	for _, r := range requests {
		when := "--"
		if !r.At.IsZero() {
			when = r.At.Local().Format("01-02 15:04")
		}
		lines = append(lines, when+"  "+cut(r.Text, requestChars))
	}
	for _, r := range t.reports[max(0, len(t.reports)-maxReports):] {
		reports = append(reports, "- "+cut(r, reportChars))
	}
	return lines, reports
}

// Batch answers PostToolBatch, which fires once per batch of tool calls — failed ones included —
// right before the next model call, where the progress note gets written. Subagents don't talk
// to the user, so they get nothing.
func Batch(payload memory.Object) memory.Object {
	if memory.Text(payload["agent_id"]) != "" {
		return nil
	}
	return memory.Object{"hookSpecificOutput": memory.Object{"hookEventName": "PostToolBatch", "additionalContext": batchReminder}}
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
