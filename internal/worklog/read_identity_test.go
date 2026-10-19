package worklog

import (
	"strings"
	"testing"
)

func TestConversationConflictingHumanIdentityFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  []map[string]any
		count int
		line  string
	}{
		{"claude typed cancellation", []map[string]any{typedRow("approval", "채택한다"), typedRow("approval", "아직 채택하지 마")}, 1, "line 2"},
		{"codex cancellation", []map[string]any{codexUser("approval", "채택한다"), codexUser("approval", "아직 채택하지 마")}, 1, "line 2"},
		{"async choice changed", []map[string]any{typedRow("human", "결정을 물어봐"), questionRow("assistant", "ask", "채택할까요?"), choiceRow("answer", "ask", "채택할까요?", "채택"), choiceRow("answer", "ask", "채택할까요?", "채택하지 않음")}, 2, "line 4"},
		{"typed and choice composite identity", []map[string]any{typedRow("human", "결정을 물어봐"), questionRow("assistant", "ask", "채택할까요?"), typedRow("answer:ask:0", "별도 취소 요청"), choiceRow("answer", "ask", "채택할까요?", "채택")}, 2, "line 4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, _, err := ReadConversation(transcriptRows(t, tc.rows...))
			if err == nil || !strings.Contains(err.Error(), "conflicting human request ID") || !strings.Contains(err.Error(), tc.line) || len(requests) != tc.count {
				t.Fatalf("conflict did not fail closed: requests=%+v error=%v", requests, err)
			}
			if strings.Contains(err.Error(), "채택") {
				t.Fatal("conflict error disclosed the human body", err)
			}
		})
	}
}

func TestConversationSameIdentitySameBodyOnlyIsDeduplicated(t *testing.T) {
	for _, rows := range [][]map[string]any{
		{typedRow("first", "응"), typedRow("first", "응"), typedRow("second", "응")},
		{codexUser("first", "응"), codexUser("first", "응"), codexUser("second", "응")},
	} {
		requests, _, err := ReadConversation(transcriptRows(t, rows...))
		if err != nil || len(requests) != 2 || requests[0].ID == requests[1].ID {
			t.Fatalf("same-body duplicate or separate identity mishandled: requests=%+v error=%v", requests, err)
		}
	}
}
