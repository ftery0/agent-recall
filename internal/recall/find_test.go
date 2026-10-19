package recall

import (
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Only synthetic records enter the search corpus; the question file stays outside it.
func evalFixture(t *testing.T) string {
	t.Helper()
	_, vault, _ := recordFixture(t)
	records := []struct{ name, id, body string }{
		{"exact", "12345678-0000-4000-8000-000000000001", "정확매치 중간조각"},
		{"body", "aaaaaaaa-0000-4000-8000-000000000003", "본문인용 ffffffff-1111-4111-8111-222222222222"},
		{"collision", "deadbeef-0000-4000-8000-000000000001", "충돌검색"},
		{"outside-results", "deadbeef-1111-4111-8111-000000000002", "전혀다른내용"},
		{"known-full", "abcdef12-0000-4000-8000-000000000003", "별칭의원본"},
		{"legacy", "abcdef12", "옛별칭"},
		{"codex", "01abcdef-0000-7000-8000-0000c0decafe", "끝별칭"},
		{"unknown-legacy", "cafebabe", "미확인별칭"},
		{"path", "bbbbbbbb-0000-4000-8000-000000000004", "경로본문 personal/10_프로젝트/회귀/작업 기록/없는.md"},
	}
	for _, r := range records {
		write(t, filepath.Join(RecordDir(), "2026-10-08", r.name+".md"),
			"---\nsession: "+r.id+"\n---\n# 회귀 자료\n"+r.body+"\n")
	}
	write(t, filepath.Join(RecordDir(), "2026-10-08", "missing-header.md"),
		"session: ffffffff-1111-4111-8111-222222222222\n# 회귀 자료\n헤더흉내\n")
	write(t, filepath.Join(vault, "personal", "10_프로젝트", "회귀", "작업 기록", "메모.md"), "# 메모\n노트매치\n")
	return filepath.Join("..", "..", "testdata", "recall_regression.json")
}

func TestEvalRegressionRejectsCitationsAndAmbiguousIdentities(t *testing.T) {
	questions := evalFixture(t)
	before, err := os.ReadFile(questions)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Eval(questions)
	if err != nil || out["questions"] != 13 || out["keyword_hits"] != 6 || out["question_hits"] != 6 {
		t.Fatal(out, err)
	}
	want := map[string][2]bool{
		"exact-session": {true, true}, "body-citation": {false, false},
		"partial-session": {false, false}, "missing-frontmatter": {false, false},
		"ambiguous-short-session": {false, false}, "resolved-legacy-session": {true, true},
		"codex-suffix": {true, true}, "unknown-legacy-session": {false, false},
		"session-alternatives": {true, true}, "note-alternative": {true, true},
		"path-citation": {false, false}, "natural-question-miss": {true, false}, "keyword-miss": {false, true},
	}
	results := out["results"].([]evalResult)
	if len(results) != len(want) {
		t.Fatal("Question results were omitted", results)
	}
	for _, result := range results {
		if got := [2]bool{result.KeywordHit, result.QuestionHit}; got != want[result.ID] {
			t.Errorf("%s: got %v, want %v", result.ID, got, want[result.ID])
		}
		for _, h := range append(result.KeywordResults, result.QuestionResults...) {
			if h.Path == questions {
				t.Error("Question answers entered the search corpus")
			}
		}
		if result.ID == "ambiguous-short-session" && (len(result.KeywordResults) != 1 || result.KeywordResults[0].Matched) {
			t.Error("Collision outside the search results was ignored", result)
		}
		for _, mode := range []struct {
			hit     bool
			results []evalHit
		}{{result.KeywordHit, result.KeywordResults}, {result.QuestionHit, result.QuestionResults}} {
			matched := false
			for _, hit := range mode.results {
				matched = matched || hit.Matched
			}
			if matched != mode.hit {
				t.Errorf("%s: Hit flag has no corresponding result", result.ID)
			}
		}
	}
	keywordMissed := []string{"body-citation", "partial-session", "missing-frontmatter", "ambiguous-short-session", "unknown-legacy-session", "path-citation", "keyword-miss"}
	questionMissed := []string{"body-citation", "partial-session", "missing-frontmatter", "ambiguous-short-session", "unknown-legacy-session", "path-citation", "natural-question-miss"}
	if !reflect.DeepEqual(out["keyword_missed"], keywordMissed) || !reflect.DeepEqual(out["question_missed"], questionMissed) {
		t.Fatal("Missing IDs do not match each query mode", out)
	}
	after, _ := os.ReadFile(questions)
	if string(before) != string(after) {
		t.Error("Eval modified its input")
	}
}

func TestEvalFullSessionSurvivesAliasCollisionAndDuplicateRecords(t *testing.T) {
	evalFixture(t)
	id := "deadbeef-0000-4000-8000-000000000001"
	write(t, filepath.Join(RecordDir(), "2026-10-08", "duplicate.md"),
		"---\nsession: "+id+"\n---\n# 복사본\n충돌검색\n")
	questions := filepath.Join(t.TempDir(), "q.json")
	write(t, questions, `[{"id":"full","question":"충돌검색","expect_sessions":["`+id+`"],"keywords":["충돌검색"]}]`)
	out, err := Eval(questions)
	if err != nil || out["keyword_hits"] != 1 || out["question_hits"] != 1 {
		t.Fatal(out, err)
	}
	results := out["results"].([]evalResult)
	if len(results[0].KeywordResults) != 2 {
		t.Fatal("Duplicate full identities should remain search results", results)
	}
}

func TestEvalDuplicateFullIdentityDoesNotMakeShortAliasAmbiguous(t *testing.T) {
	evalFixture(t)
	id := "01abcdef-0000-7000-8000-0000c0decafe"
	write(t, filepath.Join(RecordDir(), "2026-10-08", "codex-copy.md"),
		"---\nsession: "+id+"\n---\n# 복사본\n끝별칭\n")
	questions := filepath.Join(t.TempDir(), "q.json")
	write(t, questions, `[{"id":"short","question":"끝별칭","expect_sessions":["c0decafe"],"keywords":["끝별칭"]}]`)
	out, err := Eval(questions)
	if err != nil || out["keyword_hits"] != 1 || out["question_hits"] != 1 {
		t.Fatal(out, err)
	}
	write(t, filepath.Join(RecordDir(), "2026-10-08", "suffix-collision.md"),
		"---\nsession: 02abcdef-0000-7000-8000-0000c0decafe\n---\n# 다른 일\n검색밖\n")
	out, err = Eval(questions)
	if err != nil || out["keyword_hits"] != 0 || out["question_hits"] != 0 {
		t.Fatal("Suffix collision outside the top results passed", out, err)
	}
}

func TestEvalVaultHeadersCannotEstablishRecordIdentity(t *testing.T) {
	evalFixture(t)
	full := "13572468-0000-4000-8000-000000000008"
	note := "personal/10_프로젝트/회귀/작업 기록/세션인용.md"
	write(t, filepath.Join(worklog.VaultDir(), note), "---\nsession: "+full+"\n---\n# 메모\n노트세션\n")
	write(t, filepath.Join(RecordDir(), "2026-10-08", "note-backed-legacy.md"),
		"---\nsession: 13572468\n---\n# 옛 기록\n원본없는별칭\n")
	write(t, filepath.Join(worklog.VaultDir(), "personal", "10_프로젝트", "회귀", "작업 기록", "충돌인용.md"),
		"---\nsession: 12345678-9999-4999-8999-999999999999\n---\n# 메모\n다른노트\n")
	questions := filepath.Join(t.TempDir(), "q.json")
	write(t, questions, `[
		{"id":"note-full","question":"노트세션","expect_sessions":["`+full+`"],"keywords":["노트세션"]},
		{"id":"note-short","question":"노트세션","expect_sessions":["13572468"],"keywords":["노트세션"]},
		{"id":"legacy","question":"원본없는별칭","expect_sessions":["13572468"],"keywords":["원본없는별칭"]},
		{"id":"note-or","question":"노트세션","expect_sessions":["`+full+`"],"expect_note":"`+note+`","keywords":["노트세션"]},
		{"id":"unrelated-note","question":"정확매치","expect_sessions":["12345678"],"keywords":["정확매치"]}
	]`)
	out, err := Eval(questions)
	if err != nil || out["keyword_hits"] != 2 || out["question_hits"] != 2 {
		t.Fatal("Vault headers changed record identity", out, err)
	}
	for _, result := range out["results"].([]evalResult) {
		want := result.ID == "note-or" || result.ID == "unrelated-note"
		if result.KeywordHit != want || result.QuestionHit != want {
			t.Error(result)
		}
	}
}

func TestFindSkipsTheLookupsAgentsRan(t *testing.T) {
	lookupSessions(t)
	if out, err := Build(time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local), false, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)); err != nil || out["written"] != 2 {
		t.Fatal(out, err)
	}
	if hits := Find("보여줘", 5); len(hits) != 2 {
		t.Fatal("요청을 찾지 못함", hits)
	}
	if hits := Find("TSK-1344", 5); len(hits) != 0 {
		t.Fatal("AI가 부른 기억 명령이 찾기 결과에 나옴", hits[0].Line)
	}
}

func TestEvalSearchesOnlyRecordsStartedBeforeTheQuestion(t *testing.T) {
	recordFixture(t)
	morning, evening := "11111111-0000-4000-8000-000000000001", "22222222-0000-4000-8000-000000000002"
	for id, at := range map[string]string{morning: "09:00", evening: "18:00"} {
		write(t, filepath.Join(RecordDir(), "2026-10-08", id[:8]+".md"), "---\nsession: "+id+"\ndate: 2026-10-08\ntime: "+at+"-"+at+"\n---\n# 시점 자료\n시점검색\n")
	}
	// Asked at noon: the morning record existed, the evening one did not yet.
	asked := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local).UTC().Format("2006-01-02T15:04")
	questions := filepath.Join(t.TempDir(), "q.json")
	write(t, questions, `[{"id":"before","question":"시점검색 뭐였지","keywords":["시점검색"],"expect_sessions":["`+morning+`"],"asked_at":"`+asked+`"},`+
		`{"id":"after","question":"시점검색 뭐였지","keywords":["시점검색"],"expect_sessions":["`+evening+`"],"asked_at":"`+asked+`"},`+
		`{"id":"no-time","question":"시점검색 뭐였지","keywords":["시점검색"],"expect_sessions":["`+evening+`"]}]`)
	out, err := Eval(questions)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"before": true, "after": false, "no-time": true}
	for _, r := range out["results"].([]evalResult) {
		if r.KeywordHit != want[r.ID] || r.QuestionHit != want[r.ID] {
			t.Errorf("%s: keyword %v question %v, want %v", r.ID, r.KeywordHit, r.QuestionHit, want[r.ID])
		}
	}
}
