package worklog

import (
	"strings"
	"testing"
)

// under links a synthetic Claude row to the row before it, as Claude Code does.
func under(parent string, row map[string]any) map[string]any {
	row["parentUuid"] = parent
	return row
}

func agentRow(id, text string) map[string]any {
	return map[string]any{"type": "assistant", "uuid": id, "message": map[string]any{"content": text}}
}

func turnEnd(id string) map[string]any {
	return map[string]any{"type": "system", "subtype": "turn_duration", "uuid": id}
}

func requestTexts(t *testing.T, rows ...map[string]any) string {
	t.Helper()
	requests, _, err := ReadConversation(transcriptRows(t, rows...))
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range requests {
		texts = append(texts, r.Text)
	}
	return strings.Join(texts, "|")
}

func TestConversationDropsMessagesThePersonEditedOrWithdrew(t *testing.T) {
	caveat := under("turn-2", map[string]any{"type": "user", "uuid": "caveat", "isMeta": true, "message": map[string]any{"content": "<local-command-caveat>Caveat: run directly</local-command-caveat>"}})
	model := under("caveat", map[string]any{"type": "user", "uuid": "model", "message": map[string]any{"content": "<command-name>/model</command-name>\n<command-args></command-args>"}})
	got := requestTexts(t,
		typedRow("start", "작업을 시작하자"),
		under("start", agentRow("reply-1", "확인했습니다")),
		under("reply-1", turnEnd("turn-1")),
		under("turn-1", typedRow("approve", "승인")),
		under("approve", questionRow("ask", "ask-1", "어느 쪽인가요?")),
		under("ask", choiceRow("pick", "ask-1", "어느 쪽인가요?", "왼쪽")),
		under("turn-1", typedRow("edit", "잠깐, 다시 보자")),
		under("edit", agentRow("reply-2", "다시 봅니다")),
		under("reply-2", turnEnd("turn-2")),
		under("turn-2", typedRow("withdrawn", "보류")),
		caveat, model,
		under("model", typedRow("final", "계속하자")),
	)
	if got != "작업을 시작하자|잠깐, 다시 보자|계속하자" {
		t.Fatalf("an edited message, its UI answer or a withdrawn message stayed a request: %q", got)
	}
}

func TestConversationKeepsRequestsNoLaterMessageReplaced(t *testing.T) {
	t.Run("branch the tool left on compaction", func(t *testing.T) {
		call := under("start", map[string]any{"type": "assistant", "uuid": "call", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": "bash", "name": "Bash"}}}})
		result := under("call", map[string]any{"type": "user", "uuid": "result", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "bash", "content": "ok"}}}})
		got := requestTexts(t,
			typedRow("start", "작업을 시작하자"), call, result,
			under("result", typedRow("sent", "압축 뒤에 보낸 요청")),
			under("call", agentRow("resumed", "이어서 합니다")),
			under("resumed", typedRow("later", "다음 요청")),
		)
		if got != "작업을 시작하자|압축 뒤에 보낸 요청|다음 요청" {
			t.Fatalf("a request on a branch the tool started was dropped: %q", got)
		}
	})
	t.Run("last row hangs off the older message", func(t *testing.T) {
		got := requestTexts(t,
			typedRow("start", "시작"),
			under("start", turnEnd("turn")),
			under("turn", typedRow("first", "첫 말")),
			under("turn", typedRow("second", "둘째 말")),
			under("first", agentRow("late", "늦게 적힌 행")),
		)
		if got != "시작|첫 말|둘째 말" {
			t.Fatalf("an older message was taken as replacing a newer one: %q", got)
		}
	})
}

func TestConversationFallsBackToLaterSiblingWhenTheChainIsCut(t *testing.T) {
	for name, tc := range map[string]struct {
		rows []map[string]any
		want string
	}{
		"missing parent": {[]map[string]any{
			under("gone", typedRow("start", "앞에서 이어진 요청")),
			under("start", turnEnd("turn")),
			under("turn", typedRow("old", "보낸 뒤 고친 말")),
			under("turn", typedRow("new", "고친 말")),
			under("elsewhere", typedRow("other", "다른 곳에 달린 요청")),
		}, "앞에서 이어진 요청|고친 말|다른 곳에 달린 요청"},
		"compaction link loops back": {[]map[string]any{
			{"type": "system", "subtype": "compact_boundary", "uuid": "boundary", "logicalParentUuid": "start"},
			under("boundary", typedRow("start", "압축 뒤 첫 요청")),
			under("start", turnEnd("turn")),
			under("turn", typedRow("old", "보낸 뒤 고친 말")),
			under("turn", typedRow("new", "고친 말")),
		}, "압축 뒤 첫 요청|고친 말"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := requestTexts(t, tc.rows...); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
