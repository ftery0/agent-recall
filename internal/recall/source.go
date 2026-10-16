package recall

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var sourcePrefix = regexp.MustCompile(`^[A-Za-z0-9_-]{6,}$`)

func Source(prefix, query string) (string, error) { return SourcePage(prefix, query, "", 0, 20) }

// SourcePage uses original IDs/lines and pages long requests without dropping their middle.
func SourcePage(prefix, query, requestID string, offset, limit int) (string, error) {
	if !sourcePrefix.MatchString(prefix) || offset < 0 || limit < 1 || limit > 100 {
		return "", fmt.Errorf("세션 ID·offset·limit 확인 필요")
	}
	matches, err := filepath.Glob(filepath.Join(RawDir(), "*", "*", "*"+prefix+"*.jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("원본 후보 %d개: 세션 ID를 더 길게 지정한다 (%s)", len(matches), prefix)
	}
	requests, _, err := worklog.ReadConversation(matches[0])
	if err != nil {
		return "", err
	}
	type entry struct {
		line           int
		id, role, text string
	}
	var entries []entry
	for _, r := range requests {
		text := r.Text
		if r.Context != "" {
			text += "\n직전 AI 설명(보조 주장): " + r.Context
		}
		entries = append(entries, entry{r.Line, r.ID, "사용자", text})
	}
	f, err := os.Open(matches[0])
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var row struct {
			UUID        string `json:"uuid"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
			Payload struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"payload"`
		}
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			return "", fmt.Errorf("원본 %d줄 해석 실패", line)
		}
		if row.IsSidechain {
			continue
		}
		role, text := row.Message.Role, flatText(row.Message.Content)
		if row.Payload.Role != "" {
			role, text = row.Payload.Role, flatText(row.Payload.Content)
		}
		if role == "assistant" && text != "" {
			entries = append(entries, entry{line, row.UUID, "AI 보고(주장)", text})
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].line < entries[j].line })
	words := terms(query)
	var chunks []string
	for _, e := range entries {
		if requestID != "" && e.id != requestID {
			continue
		}
		text := mask(e.text)
		all := true
		for _, w := range words {
			all = all && strings.Contains(strings.ToLower(text), w)
		}
		if !all {
			continue
		}
		runes := []rune(text)
		for at := 0; at < len(runes); at += 400 {
			chunks = append(chunks, fmt.Sprintf("[줄%d %s 문자%d] %s: %s", e.line, e.id, at, e.role, string(runes[at:min(at+400, len(runes))])))
		}
	}
	if offset > len(chunks) {
		return "", fmt.Errorf("offset %d가 전체 %d개 구간을 넘음", offset, len(chunks))
	}
	end := min(offset+limit, len(chunks))
	lines := []string{matches[0], fmt.Sprintf("구간 %d:%d / %d", offset, end, len(chunks))}
	lines = append(lines, chunks[offset:end]...)
	if end < len(chunks) {
		lines = append(lines, fmt.Sprintf("이어서: --offset %d --limit %d", end, limit))
	}
	return strings.Join(lines, "\n"), nil
}
