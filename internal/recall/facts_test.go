package recall

import (
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommitsMadeReadsGitAndRtkOutput(t *testing.T) {
	if got := commitsMade(`git commit -m "fix: a"`, "[feature/x 1a2b3c4d] fix: a\n 1 file changed"); len(got) != 1 || got[0] != (madeCommit{"1a2b3c4d", "fix: a"}) {
		t.Fatal("git 출력 해시를 읽지 못함", got)
	}
	logged := commitsMade(`git commit -m "fix: b" && git log --oneline -2`, "ok 1 file changed, 2 insertions(+)\nok\n5e6f7a8b fix: b\n0000000 older")
	if len(logged) != 1 || logged[0] != (madeCommit{"5e6f7a8b", "fix: b"}) {
		t.Fatal("rtk 출력 뒤 로그 줄에서 해시를 읽지 못함", logged)
	}
	if bare := commitsMade(`git commit -m "fix: c"`, "ok 3 files changed"); len(bare) != 1 || bare[0] != (madeCommit{"", "fix: c"}) {
		t.Fatal("해시 없는 rtk 출력에서 제목을 잃음", bare)
	}
	if none := commitsMade(`git commit -m "fix: d"`, "nothing to commit, working tree clean"); len(none) != 0 {
		t.Fatal("커밋 안 된 명령을 커밋으로 읽음", none)
	}
}

func TestCommandDirFollowsCdAndGitC(t *testing.T) {
	home, _ := os.UserHomeDir()
	for shell, want := range map[string]string{
		`git commit -m x`:                        "/work/repo",
		`cd web && git add . && git commit -m x`: "/work/repo/web",
		`cd "/a b" && cd c; git commit -m x`:     "/a b/c",
		`git -C ~/other commit -m x`:             filepath.Join(home, "other"),
		`git commit -m x && cd later`:            "/work/repo",
	} {
		if got := commandDir("/work/repo", shell); got != want {
			t.Errorf("%s: got %q, want %q", shell, got, want)
		}
	}
}

func factAt(minute int) time.Time { return time.Date(2026, 10, 7, 1, minute, 0, 0, time.UTC) }

func TestFoldFactsWeighsGradesDecisionsAndTime(t *testing.T) {
	ev := factEvidence{Session: "s"}
	rule := claim("rule/repo/1", "검사 전에 묻는다", "ai", "repo", ev, factAt(0))
	rejected := claim("rule/repo/2", "아무 말", "ai", "repo", ev, factAt(0))
	older := claim("choice/s/q", "1번", "user", "repo", ev, factAt(1))
	newer := claim("choice/s/q", "2번", "user", "repo", factEvidence{Session: "s", Request: "b"}, factAt(2))
	tieA := claim("choice/s/t", "가", "user", "repo", ev, factAt(3))
	tieB := claim("choice/s/t", "나", "user", "repo", factEvidence{Session: "s", Request: "c"}, factAt(3))
	confirm := factLine{Kind: "decision", ID: "d1", Fact: rule.Fact, Verdict: "확인", ObservedAt: factAt(5).Format(time.RFC3339)}
	reject := factLine{Kind: "decision", ID: "d2", Fact: rejected.Fact, Verdict: "거절", ObservedAt: factAt(5).Format(time.RFC3339)}
	status := map[string]string{}
	for _, f := range foldFacts([]factLine{rule, rejected, older, newer, tieA, tieB}) {
		status[f.Value] = f.Status
	}
	if status["검사 전에 묻는다"] != factProposed || status["1번"] != factReplaced || status["2번"] != factActive || status["가"] != factConflict || status["나"] != factConflict {
		t.Fatal("등급·시각에 따른 상태가 틀림", status)
	}
	for _, f := range foldFacts([]factLine{rule, rejected, confirm, reject}) {
		status[f.Value] = f.Status
	}
	if status["검사 전에 묻는다"] != factActive || status["아무 말"] != factRejected {
		t.Fatal("사용자 확인·거절이 반영되지 않음", status)
	}
}

func TestSessionFactsTakeVerdictsOnlyFromTypedWords(t *testing.T) {
	rule := claim("rule/repo/1", "검사 전에 묻는다", "ai", "repo", factEvidence{}, factAt(0))
	short := shortFact(rule.Fact)
	s := session{id: "s1", cwd: t.TempDir(), requests: []worklog.Request{
		{ID: "a", Text: "확인 " + short, Source: "claude:typed", At: factAt(1)},
		{ID: "b", Text: "거절 " + short, Source: "claude:queued", At: factAt(2)},
		{ID: "c", Text: "확인 f0000000", Source: "claude:typed", At: factAt(3)},
		{ID: "d", Text: "이거 확인 " + short + " 해줘", Source: "claude:typed", At: factAt(4)},
		{ID: "e", Text: "질문: 어디서 할까요?\n선택: 여기서", Source: "claude:choice", QuestionID: "ask", At: factAt(5)},
	}}
	var decisions, choices []factLine
	for _, line := range sessionFacts(s, []factLine{rule}) {
		switch {
		case line.Kind == "decision":
			decisions = append(decisions, line)
		case strings.HasPrefix(line.Slot, "choice/"):
			choices = append(choices, line)
		}
	}
	if len(decisions) != 1 || decisions[0].Verdict != "확인" || decisions[0].Evidence.Request != "a" {
		t.Fatal("직접 친 확인만 결정이 되어야 함", decisions)
	}
	if len(choices) != 1 || choices[0].Value != "여기서" || choices[0].Grade != "user" || choices[0].Evidence.Quote != "어디서 할까요?" {
		t.Fatal("선택창 답을 사용자 사실로 남기지 못함", choices)
	}
}

func TestAppendFactsWritesOnceAndRefusesTimesWithoutZone(t *testing.T) {
	fixture(t)
	line := claim("choice/s/q", "1번", "user", "repo", factEvidence{Session: "s"}, factAt(0))
	have := map[string]bool{}
	for range 2 {
		if err := appendFacts(have, []factLine{line}, factAt(9)); err != nil {
			t.Fatal(err)
		}
	}
	have = map[string]bool{}
	if err := appendFacts(have, []factLine{line}, factAt(9)); err != nil {
		t.Fatal(err)
	}
	if lines, err := readFacts(); err != nil || len(lines) != 2 || lines[0].RecordedAt == "" {
		t.Fatal("같은 줄을 아는 동안 두 번 쓰거나 기록 시각이 없음", lines, err)
	}
	line.ID, line.ObservedAt = "other", "2026-10-07 10:00"
	if err := appendFacts(map[string]bool{}, []factLine{line}, factAt(9)); err == nil || !strings.Contains(err.Error(), "시간대") {
		t.Fatal("시간대 없는 시각을 받아들임", err)
	}
}

func TestBuildRecordsCommitsPullRequestsAndConfirmedRules(t *testing.T) {
	_, _, path := recordFixture(t)
	if err := saveLessons([]Lesson{{Rule: "커밋 전에 검사 결과를 보인다", Scope: "repo", Origin: "기록", Status: "후보", LastSeen: "2026-10-06"}}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	use := func(id, command string, at time.Time) map[string]any {
		return map[string]any{"type": "assistant", "timestamp": at.Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}}
	}
	result := func(id, output string, at time.Time) map[string]any {
		return map[string]any{"type": "user", "timestamp": at.Format(time.RFC3339), "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": output}}}}
	}
	at := now.Add(-3 * time.Hour)
	appendRecordRows(t, path,
		use("c9", `git commit -m "fix: 헤더 정리"`, at), result("c9", "[main 9a8b7c6d] fix: 헤더 정리", at),
		use("p9", "gh pr create --base dev --title 헤더", at), result("p9", "https://github.com/me/web/pull/12", at),
	)
	if _, err := Build(now.AddDate(0, 0, -14), false, now); err != nil {
		t.Fatal(err)
	}
	lines, err := readFacts()
	if err != nil {
		t.Fatal(err)
	}
	var rule factState
	kinds := map[string]bool{}
	for _, f := range foldFacts(lines) {
		kinds[strings.SplitN(f.Slot, "/", 2)[0]] = true
		if strings.HasPrefix(f.Slot, "rule/") {
			rule = f
		}
		if strings.HasPrefix(f.Slot, "commit/") && (f.Ref != "9a8b7c6d" || f.Value != "fix: 헤더 정리" || f.Status != factActive) {
			t.Fatal("커밋 사실이 틀림", f)
		}
	}
	if !kinds["commit"] || !kinds["pr"] || rule.Status != factProposed {
		t.Fatal("커밋·PR 이 없거나 교훈이 AI 후보로 오지 않음", kinds, rule)
	}
	if strings.Contains(strings.Join(CardLines("/work/repo"), "\n"), "커밋 전에 검사") {
		t.Fatal("확인 전 후보가 카드에 나옴")
	}
	appendRecordRows(t, path, recordHuman("confirm", "확인 "+shortFact(rule.Fact), now.Add(-time.Hour)))
	if _, err := Build(now.AddDate(0, 0, -14), false, now); err != nil {
		t.Fatal(err)
	}
	if rules := confirmedRules("repo"); len(rules) != 1 || rules[0] != "커밋 전에 검사 결과를 보인다" {
		t.Fatal("직접 친 확인이 규칙을 확정하지 못함", rules)
	}
}

func TestFactsNoticeShowsConflictsAndOnlyTheDaysProposals(t *testing.T) {
	fixture(t)
	now := factAt(0).Add(72 * time.Hour)
	if got := FactsNotice(now); got != "" {
		t.Fatal("빈 장부에 알림", got)
	}
	old := claim("rule/repo/1", "오래된 후보", "ai", "repo", factEvidence{}, now.Add(-30*time.Hour))
	if err := appendFacts(map[string]bool{}, []factLine{old}, now); err != nil {
		t.Fatal(err)
	}
	if got := FactsNotice(now); got != "" {
		t.Fatal("하루 지난 후보로 카드마다 알림", got)
	}
	fresh := claim("rule/repo/2", "새 후보", "ai", "repo", factEvidence{}, now.Add(-time.Hour))
	tieA := claim("choice/s/t", "가", "user", "repo", factEvidence{Session: "s"}, factAt(3))
	tieB := claim("choice/s/t", "나", "user", "repo", factEvidence{Session: "s", Request: "c"}, factAt(3))
	if err := appendFacts(map[string]bool{}, []factLine{fresh, tieA, tieB}, now); err != nil {
		t.Fatal(err)
	}
	if got := FactsNotice(now); got != "사실 장부 확인 필요: 서로 다른 답 2개 · 새 AI 후보 1개 — harness recall facts --review" {
		t.Fatal(got)
	}
}
