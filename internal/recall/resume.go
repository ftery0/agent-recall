package recall

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type resumeRecord struct {
	path, raw, session, cwd, state string
	m                              summary
	when                           time.Time
	requests                       []worklog.Request
	files                          []string // what the conversation changed, from its record
	ended                          time.Time
	last                           string // the agent's answer to the last request, read from the transcript
	rawError                       bool
}

// Read location metadata without loading the rest of the conversation.
func transcriptMetadata(path string) (cwd, id string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	return transcriptMetadataReader(f)
}

func transcriptMetadataReader(reader io.Reader) (cwd, id string, err error) {
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), 64<<20)
	for i := 0; i < 32 && scan.Scan(); i++ {
		var row map[string]any
		if json.Unmarshal(scan.Bytes(), &row) != nil {
			continue
		}
		if row["isSidechain"] == true {
			continue
		}
		if value, _ := row["cwd"].(string); value != "" {
			cwd = filepath.Clean(value)
		}
		if value, _ := row["sessionId"].(string); value != "" {
			id = value
		}
		if row["type"] == "session_meta" {
			payload, _ := row["payload"].(map[string]any)
			if source, ok := payload["source"].(map[string]any); ok && source["subagent"] != nil {
				return "", "", nil
			}
			if value, _ := payload["cwd"].(string); value != "" {
				cwd = filepath.Clean(value)
			}
			id, _ = payload["id"].(string)
		}
		if cwd != "" && id != "" {
			break
		}
	}
	return cwd, id, scan.Err()
}

func unfinished(m summary, state string) bool {
	if state != "ready" || len(m.Todo) == 0 {
		return true // an empty remaining-work list is not an explicit completion statement
	}
	for _, todo := range m.Todo {
		if _, closed := resolved(m, "todo", todo); !closed {
			return true
		}
	}
	return false
}

func recordTime(raw []byte, fallback time.Time) time.Time {
	// A conversation can run for days; only the end line knows the day it stopped.
	if at, err := time.ParseInLocation(endStamp, header(raw, "end"), time.Local); err == nil {
		return at
	}
	span := strings.Split(header(raw, "time"), "-")
	if len(span) > 0 {
		if at, err := time.ParseInLocation("2006-01-02 15:04", header(raw, "date")+" "+span[len(span)-1], time.Local); err == nil {
			return at
		}
	}
	return fallback
}

// requestedRecord selects only an unambiguous title match from the person's latest substantive
// request. A bare confirmation supplies no task identity, even when one record is newer.
func requestedRecord(records []resumeRecord, requests []worklog.Request) (resumeRecord, bool) {
	for i := len(requests) - 1; i >= 0; i-- {
		r := requests[i]
		text := r.Text
		if r.QuestionID != "" {
			_, answer, found := strings.Cut(text, "\n선택: ")
			if !found {
				return resumeRecord{}, false
			}
			text = answer // question wording is not the person's task choice
		} else if len([]rune(text)) <= 4 {
			continue
		}
		words := terms(text)
		var hits []resumeRecord
		for _, record := range records {
			name := strings.ToLower(record.m.Title)
			if name != "" && strings.Contains(strings.ToLower(text), name) {
				hits = append(hits, record)
				continue
			}
			matched := map[string]bool{}
			for _, word := range words {
				if !strings.Contains(name, word) {
					continue
				}
				unique := true
				for _, other := range records {
					if other.raw != record.raw && strings.Contains(strings.ToLower(other.m.Title), word) {
						unique = false
						break
					}
				}
				if unique {
					matched[word] = true
				}
			}
			if len(matched) >= 2 {
				hits = append(hits, record)
			}
		}
		if len(hits) == 1 {
			return hits[0], true
		}
		return resumeRecord{}, false
	}
	return resumeRecord{}, false
}

func requestExcerpt(r worklog.Request, limit int) string {
	r.Text = mask(r.Text)
	if r.QuestionID != "" {
		question, answer, found := strings.Cut(r.Text, "\n선택: ")
		if found {
			answer = cut(answer, limit/3)
			return cut(strings.TrimPrefix(question, "질문: "), max(1, limit-len([]rune(answer))-3)) + " → " + answer
		}
	}
	return cut(strings.ReplaceAll(r.Text, "\n", " "), limit)
}

func requestView(requests []worklog.Request, count, budget int) string {
	var b strings.Builder
	recent := append([]worklog.Request(nil), requests[max(0, len(requests)-count):]...)
	// Keep questions from one UI answer in their original order within the newest event.
	sort.SliceStable(recent, func(i, j int) bool { return recent[i].Line > recent[j].Line })
	shown := ""
	for _, r := range recent {
		// A resent long request says nothing new; short answers like "1번" can mean different things.
		if r.Text == shown && len([]rune(r.Text)) > 16 {
			continue
		}
		shown = r.Text
		prefix := fmt.Sprintf("  [%s 줄%d] ", r.ID, r.Line)
		room := budget - len([]rune(b.String()+prefix+"\n"))
		if room < 16 {
			break
		}
		// An excerpt cut short ends with one more rune, "…".
		b.WriteString(prefix + requestExcerpt(r, min(110, room-1)) + "\n")
		if r.QuestionID == "" && len([]rune(r.Text)) <= 16 && r.Context != "" {
			line := "    보조 문맥(AI 주장): " + cut(strings.ReplaceAll(mask(r.Context), "\n", " "), 60) + "\n"
			if len([]rune(b.String()+line)) <= budget {
				b.WriteString(line)
			}
		}
	}
	return b.String()
}

func appendViewLine(b *strings.Builder, line string, budget int) {
	if len([]rune(b.String()+line+"\n")) <= budget {
		b.WriteString(line + "\n")
	}
}

func recordView(record resumeRecord, budget int) string {
	var b strings.Builder
	heading := "- 목표/제목: " + cut(mask(record.m.Title), 60)
	if len(record.requests) > 1 {
		// A long conversation keeps its first request as its title after the work moved on.
		heading += " (처음 요청 — 지금 일은 아래 최신 원문)"
	}
	appendViewLine(&b, heading, budget)
	ending, check := "", cut(sessionCheck(record.session, record.cwd, record.files, record.ended, time.Now()), 200)
	if record.last != "" {
		ending = "  끝난 지점(AI 주장): " + gist(record.last, 120)
	}
	if len(record.requests) > 0 {
		appendViewLine(&b, "  이전 사용자 원문(최신부터, 아래 요약보다 우선):", budget)
		// The requests leave room for where the conversation stopped and what git says of it now.
		room := budget - len([]rune(b.String()+ending+check)) - 2
		b.WriteString(requestView(record.requests, 4, min(max(0, room), budget*3/5)))
	}
	if ending != "" {
		appendViewLine(&b, ending, budget)
	}
	if check != "" {
		appendViewLine(&b, check, budget)
	}
	if record.rawError {
		appendViewLine(&b, "  원문 조회 미확정: 이전 요약의 해석을 원문으로 확인한다.", budget)
	}
	if record.state == "ready" && len(record.m.Todo) == 0 {
		appendViewLine(&b, "  현황 미확정: 남은 일 없음만으로 전체 완료를 확정하지 않는다.", budget)
	}
	var active, closed, remaining []string
	for _, d := range record.m.Decisions {
		if r, ok := resolved(record.m, "decision", d.What); ok {
			closed = append(closed, d.What+" → "+r.State+" ["+r.Source+"]")
			continue
		}
		label := "요약·근거 미확인"
		if d.Status == "사용자" {
			label = "사용자 원문 연결·요약 해석 " + d.Source
		}
		active = append(active, requestExcerpt(worklog.Request{Text: d.What, QuestionID: "choice"}, 80)+" — "+cut(mask(d.Why), 40)+" ["+label+"]")
	}
	for _, todo := range record.m.Todo {
		if r, ok := resolved(record.m, "todo", todo); ok {
			closed = append(closed, todo+" → "+r.State+" ["+r.Source+"]")
		} else {
			remaining = append(remaining, todo)
		}
	}
	for _, value := range remaining[:min(2, len(remaining))] {
		appendViewLine(&b, "  남은 일: "+cut(mask(value), 70), budget)
	}
	for _, value := range closed[max(0, len(closed)-2):] {
		appendViewLine(&b, "  명시된 닫힘: "+cut(mask(value), 110), budget)
	}
	for _, value := range active[max(0, len(active)-2):] {
		appendViewLine(&b, "  결정/이유: "+value, budget)
	}
	return b.String()
}

// ResumeText is a bounded view over existing records, not another memory database. User source
// events precede model interpretation; recorded approvals never create new execution authority.
func ResumeText(cwd, transcriptPath string) (string, error) {
	if cwd == "" {
		return "", nil
	}
	cwd = filepath.Clean(cwd)
	root := worklog.RepoRoot(cwd)
	roots := map[string]string{cwd: root}
	matchesFolder := func(other string) bool {
		if other == "" {
			return false
		}
		other = filepath.Clean(other)
		if other == cwd {
			return true
		}
		if root == "" {
			return false
		}
		otherRoot, ok := roots[other]
		if !ok {
			otherRoot = worklog.RepoRoot(other)
			roots[other] = otherRoot
		}
		return otherRoot == root
	}
	stored, readErrors := loadRecords()
	var records []resumeRecord
	knownRaw := map[string]bool{}
	for _, rec := range stored {
		rawPath := rec.field("raw")
		knownRaw[rawPath] = true
		folder := rec.cwd()
		if folder == "" {
			folder, _, _ = transcriptMetadata(rawPath)
		}
		if !matchesFolder(folder) {
			continue
		}
		m, _ := rec.summary()
		if m.Title == "" {
			m.Title = rec.title()
		}
		state := rec.field("summary_state")
		when := rec.ended()
		if rawInfo, err := os.Stat(rawPath); err == nil {
			size, sizeErr := strconv.ParseInt(rec.field("raw_bytes"), 10, 64)
			if (sizeErr == nil && rawInfo.Size() != size) || rawInfo.ModTime().After(rec.mod) {
				state = "pending"
				when = lastActive(rawPath)
			}
		}
		if unfinished(m, state) {
			records = append(records, resumeRecord{path: rec.path, raw: rawPath, session: rec.field("session"), files: rec.items("바뀐 파일"), ended: rec.ended(), cwd: folder, state: state, m: m, when: when})
		}
	}
	// Stop archives immediately; the summary collector may not have run yet. Inspect only
	// location/time metadata here and read the selected conversations below, without writes.
	var currentFolder, currentSession string
	if transcriptPath != "" {
		var err error
		currentFolder, currentSession, err = transcriptMetadata(transcriptPath)
		if err != nil {
			readErrors = append(readErrors, err)
		}
	}
	for _, tool := range []string{"claude", "codex"} {
		days, err := os.ReadDir(filepath.Join(RawDir(), tool))
		if err != nil && !os.IsNotExist(err) {
			readErrors = append(readErrors, err)
			continue
		}
		var rawPaths []string
		for _, day := range days {
			if !day.IsDir() {
				continue
			}
			folder := filepath.Join(RawDir(), tool, day.Name())
			files, err := os.ReadDir(folder)
			if err != nil {
				readErrors = append(readErrors, err)
				continue
			}
			for _, file := range files {
				if !file.IsDir() && strings.HasSuffix(file.Name(), ".jsonl") {
					rawPaths = append(rawPaths, filepath.Join(folder, file.Name()))
				}
			}
		}
		for _, path := range rawPaths {
			if knownRaw[path] || path == transcriptPath {
				continue
			}
			folder, id, err := transcriptMetadata(path)
			if err != nil {
				readErrors = append(readErrors, fmt.Errorf("%s: %w", path, err))
				continue
			}
			if !matchesFolder(folder) || (currentSession != "" && id == currentSession) {
				continue
			}
			if id == "" {
				id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
			}
			records = append(records, resumeRecord{raw: path, session: id, cwd: folder, state: "pending", m: summary{Title: "최근 대화(기록 전)"}, when: lastActive(path)})
		}
	}
	// The conversation open now leads, so its input is not read as the next record's work.
	current := func(r resumeRecord) bool {
		return (transcriptPath != "" && r.raw == transcriptPath) || (currentSession != "" && r.session == currentSession)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if a, b := current(records[i]), current(records[j]); a != b {
			return a
		}
		return records[i].when.After(records[j].when)
	})
	var requests []worklog.Request
	if transcriptPath != "" {
		if matchesFolder(currentFolder) || currentFolder == "" {
			var err error
			requests, _, err = worklog.ReadConversation(transcriptPath)
			if err != nil {
				readErrors = append(readErrors, err)
			}
		}
	}
	if len(records) == 0 && len(requests) == 0 {
		return "", errors.Join(readErrors...)
	}
	selected := false
	if record, ok := requestedRecord(records, requests); ok {
		records, selected = []resumeRecord{record}, true
	}
	totalCandidates := len(records)
	var visible []resumeRecord
	shownSessions := map[string]bool{}
	for _, record := range records {
		if len(visible) == 2 {
			break
		}
		// Codex can keep one conversation in two files; the newest stands for it.
		if record.session != "" && shownSessions[record.session] {
			continue
		}
		if current(record) {
			visible, shownSessions[record.session] = append(visible, record), true
			continue
		}
		prior, reports, err := worklog.ReadConversation(record.raw)
		if err != nil {
			record.rawError = true
			readErrors = append(readErrors, err)
		}
		if record.path == "" && len(prior) == 0 {
			totalCandidates-- // synthetic-only conversations are not work candidates
			continue
		}
		record.requests, record.last = prior, lastAnswer(prior, reports)
		if record.path == "" {
			record.m.Title = requestTitle(prior) // not recorded yet: name it by what the person typed
		}
		visible, shownSessions[record.session] = append(visible, record), true
	}
	records = visible
	if len(records) == 0 && len(requests) == 0 {
		return "", errors.Join(readErrors...)
	}
	var footer strings.Builder
	if len(requests) > 0 {
		fmt.Fprintf(&footer, "현재 입력 원본: %s\n", transcriptPath)
	}
	// Source commands retain full session IDs even when long filesystem paths would
	// crowd out the actual request. Source lookup returns the full original path.
	for _, record := range records {
		fmt.Fprintf(&footer, "원문: harness recall source %s --limit 10\n", record.session)
	}
	for _, record := range records {
		var detail string
		if record.path != "" {
			detail = fmt.Sprintf("상세: %s\n", record.path)
		} else {
			detail = fmt.Sprintf("보관 원본: %s\n", record.raw)
		}
		if len([]rune(footer.String()+detail)) <= 800 {
			footer.WriteString(detail)
		}
	}
	var b strings.Builder
	b.WriteString("=== 작업 이어가기 — 현재 사용자 원문이 과거 요약보다 우선 ===\n과거 승인은 새 실행 권한이 아니다. AI 보고는 완료 증거가 아니다.\n")
	if totalCandidates > 1 {
		fmt.Fprintf(&b, "작업 후보 %d개 중 최근 %d개. 첫 사용자 요청으로 작업을 좁힌다.\n", totalCandidates, len(records))
	} else if len(records) == 1 {
		label := "같은 폴더의 작업 후보"
		if selected {
			label = "사용자 요청의 제목과 일치한 작업"
		}
		b.WriteString(label + ":\n")
	}
	remaining := 2000 - len([]rune(b.String()+footer.String()))
	if len(requests) > 0 {
		b.WriteString("현재 사용자 입력(최신부터):\n")
		budget := remaining
		if len(records) > 0 {
			budget = min(450, remaining/3)
		}
		b.WriteString(requestView(requests, 6, max(0, budget)))
	}
	for i, record := range records {
		if i > 0 && current(records[i-1]) && !current(record) {
			b.WriteString("같은 폴더의 다른 대화(지금 대화 아님):\n")
		}
		budget := (2000 - len([]rune(b.String()+footer.String()))) / (len(records) - i)
		b.WriteString(recordView(record, max(0, budget)))
	}
	return mask(b.String() + footer.String()), errors.Join(readErrors...)
}
