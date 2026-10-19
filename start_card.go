package main

import (
	"github.com/ftery0/agent-recall/internal/memory"
	"github.com/ftery0/agent-recall/internal/recall"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"strings"
	"time"
)

const cardLimit = 3000

// startCard is the session-start card: where the work stopped, the notes and git state, then the
// user-confirmed lessons, cut to the card's budget with the leftover named.
func startCard(tool string, payload memory.Object) string {
	cwd := memory.Text(payload["cwd"])
	transcript := memory.Text(payload["transcript_path"])
	// A new conversation is named before its first line is written; that is not a failed lookup.
	if source := memory.Text(payload["source"]); source == "startup" || source == "clear" {
		if _, err := os.Stat(transcript); os.IsNotExist(err) {
			transcript = ""
		}
	}
	core, err := recall.ResumeText(cwd, transcript)
	copy := memory.Object{}
	for k, v := range payload {
		copy[k] = v
	}
	if core != "" {
		copy["recall_context"] = true
	}
	base := worklog.Start(tool, copy)
	var parts []string
	if strings.HasPrefix(base, "=== 압축·이어 열기 뒤다.") {
		first, rest, _ := strings.Cut(base, "\n")
		parts = append(parts, first)
		base = rest
	}
	if core != "" {
		parts = append(parts, core)
	}
	if err != nil {
		parts = append(parts, "기억 조회 미완료: "+cardExcerpt(err.Error(), 220))
	}
	var extra []string
	if core != "" || base != "" {
		lessons := recall.CardLines(cwd)
		parts = append(parts, lessons[len(lessons)-1])
		extra = append(extra, lessons[:len(lessons)-1]...)
		if warning := recall.SyncWarning(); warning != "" {
			parts = append(parts, cardExcerpt(warning, 250))
		}
		if notice := recall.FactsNotice(time.Now()); notice != "" {
			extra = append(extra, notice)
		}
	}
	extra = append(extra, strings.Split(base, "\n")...)
	const omitted = "시작 카드에 생략된 내용은 원본 기록과 recall find로 확인한다."
	skipped := false
	for _, line := range extra {
		if line == "" {
			continue
		}
		if len([]rune(strings.Join(parts, "\n")+"\n"+line+"\n"+omitted)) <= cardLimit {
			parts = append(parts, line)
			continue
		}
		skipped = true
		if len(parts) > 0 && strings.HasPrefix(parts[len(parts)-1], "=== 배운 것") {
			parts = parts[:len(parts)-1]
		}
	}
	if skipped {
		parts = append(parts, omitted)
	}
	return strings.Join(parts, "\n")
}

func cardExcerpt(s string, limit int) string {
	if runes := []rune(s); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return s
}
