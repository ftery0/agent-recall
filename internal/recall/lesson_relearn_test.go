package recall

import (
	"encoding/json"
	"github.com/ftery0/agent-recall/internal/worklog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const lessonID = "01a0f9ee-3306-7161-94a4-da7c47dfcad4"

func lessonFixture(t *testing.T) {
	t.Helper()
	home, _ := fixture(t)
	t.Setenv("HARNESS_VAULT_DIR", filepath.Join(home, "vault"))
}

func lessonRecordFixture(t *testing.T, name, id, rawID, rule string) string {
	t.Helper()
	rawPath := ""
	if rawID != "" {
		rawPath = filepath.Join(RawDir(), "codex", "2026-10-06", name+".jsonl")
		write(t, rawPath, jsonLines(t, map[string]any{"type": "session_meta", "timestamp": "2026-10-06T01:00:00Z", "payload": map[string]any{"id": rawID, "cwd": "/work/repo"}}))
	}
	s := session{tool: "codex", id: id, path: rawPath, cwd: "/work/repo", start: time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC), requests: []worklog.Request{{Text: "교훈 후보 확인"}}}
	path := filepath.Join(RecordDir(), "2026-10-06", name+".md")
	write(t, path, render(s, summary{Title: "후보", Lessons: []string{rule}}, true))
	return path
}

func TestRelearnPreviewPreservesFilesAndCanonicalizesOnlyProvenAliases(t *testing.T) {
	lessonFixture(t)
	rule := "기능을 지운 뒤 관련 주석도 함께 고친다"
	lessonRecordFixture(t, "one", lessonID, lessonID, rule)
	if err := saveLessons([]Lesson{{Rule: rule, Scope: "repo", Origin: "기록", Sessions: []string{"01a0f9ee", "47dfcad4", lessonID}, LastSeen: "2026-10-06", Status: "후보"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(lessonsPath())
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	preview, err := PreviewRelearn(day)
	if err != nil || preview["changed"] != 1 || preview["unresolved_connections"] != 0 {
		t.Fatal(preview, err)
	}
	details := preview["changes"].([]map[string]any)
	if len(details) != 1 || details[0]["index"] != 0 || details[0]["unknown_before"] != 3 || details[0]["unknown_after"] != 0 || details[0]["sources_after"] != 1 {
		t.Fatal("preview omitted per-lesson evidence changes", details)
	}
	after, _ := os.ReadFile(lessonsPath())
	if string(before) != string(after) {
		t.Fatal("preview wrote lessons")
	}
	if _, err = os.Stat(filepath.Join(Dir(), ".build.lock")); !os.IsNotExist(err) {
		t.Fatal("preview created a lock", err)
	}
	if _, err = os.Stat(filepath.Join(Dir(), "배운것.md")); !os.IsNotExist(err) {
		t.Fatal("preview rendered a view", err)
	}
	out, err := RelearnChecked(day, preview["source_token"].(string))
	if err != nil || out["applied"] != true {
		t.Fatal(out, err)
	}
	all, _ := loadLessons()
	if len(all) != 1 || strings.Join(all[0].Sessions, ",") != lessonID || knownSessions(all[0]) != 1 || all[0].Status != "후보" || all[0].Confirmed {
		t.Fatal(all)
	}
	if len(all[0].Sources) != 1 || len(all[0].Sources[0].Aliases) != 2 || all[0].Sources[0].Basis != "원문 세션" {
		t.Fatal(all[0].Sources)
	}
	info, _ := os.Stat(lessonsPath())
	out, err = Relearn(day.AddDate(0, 0, 1))
	afterInfo, _ := os.Stat(lessonsPath())
	if err != nil || out["changed"] != 0 || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("relearn was not idempotent", out, err)
	}
}

func TestAutomaticLessonCollectionDoesNotApplyManualSourceMigration(t *testing.T) {
	lessonFixture(t)
	s := session{id: lessonID, cwd: "/work/repo", path: "/raw/source", start: time.Now()}
	if err := learn(s, summary{Lessons: []string{"자동 수집은 출처 보정을 적용하지 않는다"}}); err != nil {
		t.Fatal(err)
	}
	all, _ := loadLessons()
	if len(all) != 1 || len(all[0].Sources) != 0 || all[0].Status != "후보" || all[0].Confirmed {
		t.Fatal("install bypassed manual migration", all)
	}
}

func TestRelearnSaveFailureReportsUncertaintyAndPreservesExistingJSON(t *testing.T) {
	lessonFixture(t)
	rule := "저장 실패의 결과를 다시 확인한다"
	lessonRecordFixture(t, "known", lessonID, "", rule)
	saveLessons([]Lesson{{Rule: rule, Scope: "repo", Origin: "기록", Sessions: []string{"47dfcad4"}, Status: "후보"}})
	write(t, filepath.Join(Dir(), ".build.lock"), "")
	before := string(mustReadLesson(t, lessonsPath()))
	if err := os.Chmod(Dir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(Dir(), 0o700) })
	out, err := Relearn(time.Now())
	if err == nil || !strings.Contains(err.Error(), "저장 결과 미확인") || out["lessons_saved"] != "미확인" || out["applied"] != nil || string(mustReadLesson(t, lessonsPath())) != before {
		t.Fatal("save failure became success or discarded current data", out, err)
	}
}

func TestRelearnPreservesAmbiguousPrefixAndConfirmedState(t *testing.T) {
	lessonFixture(t)
	rule := "커밋 전에 관련 검사를 확인한다"
	second := "01a0f9ee-0000-7000-8000-00000000bbbb"
	lessonRecordFixture(t, "a", lessonID, "", rule)
	lessonRecordFixture(t, "b", second, "", rule)
	saveLessons([]Lesson{{Rule: rule, Scope: "repo", Origin: "기록", Sessions: []string{"01a0f9ee", "missing-legacy"}, Status: "활성", Confirmed: true}})
	out, err := Relearn(time.Now())
	all, _ := loadLessons()
	if err != nil || out["active"] != 1 || len(all) != 1 {
		t.Fatal(out, all, err)
	}
	if strings.Join(all[0].Sessions, ",") != "01a0f9ee,missing-legacy,"+lessonID+","+second || knownSessions(all[0]) != 2 || unknownSessions(all[0]) != 2 || !all[0].Confirmed || all[0].Status != "활성" {
		t.Fatal("ambiguous evidence or user confirmation lost", all)
	}
}

func TestRelearnUsesMetadataRatherThanLastUUIDInFilename(t *testing.T) {
	lessonFixture(t)
	other := "01a0ccd0-fecf-7d83-a7b0-d290c3a1fea2"
	rule := "파일 이름에서 세션 원문을 추정하지 않는다"
	lessonRecordFixture(t, lessonID+"_"+other, lessonID, lessonID, rule)
	saveLessons([]Lesson{{Rule: rule, Scope: "repo", Origin: "기록", Sessions: []string{"47dfcad4", lessonID}, Status: "후보"}})
	_, err := Relearn(time.Now())
	all, _ := loadLessons()
	if err != nil || len(all) != 1 || strings.Join(all[0].Sessions, ",") != lessonID || all[0].Sources[0].Session != lessonID {
		t.Fatal(all, err)
	}
}

func TestRelearnKeepsConflictingMetadataUncertain(t *testing.T) {
	lessonFixture(t)
	rule := "충돌한 원문의 연결을 보존한다"
	other := "01a0ccd0-fecf-7d83-a7b0-d290c3a1fea2"
	lessonRecordFixture(t, "conflict", lessonID, other, rule)
	saveLessons([]Lesson{{Rule: rule, Scope: "repo", Origin: "기록", Sessions: []string{lessonID}, Status: "후보", LastSeen: "2026-10-07"}})
	out, err := Relearn(time.Now())
	all, _ := loadLessons()
	if err != nil || out["unresolved_connections"] != 1 || strings.Join(all[0].Sessions, ",") != lessonID || knownSessions(all[0]) != 0 || sourceDate(all[0], false) != "미확인" {
		t.Fatal(out, all, err)
	}
}

func TestRelearnRejectsChangedPreviewAndPreservesUnknownJSONFields(t *testing.T) {
	lessonFixture(t)
	rule := "앞으로 추가한 교훈 필드도 보존한다"
	path := lessonRecordFixture(t, "known", lessonID, "", rule)
	raw := `[{"rule":"` + rule + `","scope":"repo","origin":"기록","sessions":["` + lessonID + `"],"last_seen":"2026-10-06","status":"후보","future":{"value":7},"sources":[{"session":"` + lessonID + `","record":"` + path + `","basis":"기록 세션","future_source":[1,2]}]}]`
	write(t, lessonsPath(), raw)
	preview, err := PreviewRelearn(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Replace(string(mustReadLesson(t, path)), "title:", "changed: true\ntitle:", 1))
	if _, err = RelearnChecked(time.Now(), preview["source_token"].(string)); err == nil || string(mustReadLesson(t, lessonsPath())) != raw {
		t.Fatal("changed preview applied", err)
	}
	if _, err = Relearn(time.Now()); err != nil {
		t.Fatal(err)
	}
	saved := string(mustReadLesson(t, lessonsPath()))
	if !strings.Contains(saved, `"future"`) || !strings.Contains(saved, `"future_source"`) || !strings.Contains(saved, `"value": 7`) {
		t.Fatal("unknown JSON fields lost", saved)
	}
}

func mustReadLesson(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRelearnMemoryDateIsImportHistoryAndDoesNotAdvance(t *testing.T) {
	lessonFixture(t)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude-profiles", "work", "projects", "p", "memory", "one.md")
	rule := "사용자 지시를 원문으로 다시 확인한다"
	write(t, path, "---\ntype: feedback\ndescription: "+rule+"\n---\nbody\n")
	saveLessons([]Lesson{{Rule: rule, Scope: "전체", Origin: "옛 메모리", Sessions: []string{"memory:p/one.md"}, LastSeen: "2026-10-01", Status: "후보"}})
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	if _, err := Relearn(day); err != nil {
		t.Fatal(err)
	}
	all, _ := loadLessons()
	if all[0].LastSeen != "2026-10-01" || sourceDate(all[0], false) != "미확인" || sourceDate(all[0], true) != "2026-10-01" || knownSessions(all[0]) != 0 || unknownSessions(all[0]) != 0 {
		t.Fatal(all)
	}
	before := string(mustReadLesson(t, lessonsPath()))
	if out, err := Relearn(day.AddDate(0, 0, 10)); err != nil || out["changed"] != 0 || string(mustReadLesson(t, lessonsPath())) != before {
		t.Fatal("reimport changed the first imported date", out, err)
	}
	view := string(mustReadLesson(t, filepath.Join(Dir(), "배운것.md")))
	if !strings.Contains(view, "| 미확인 | 2026-10-01 | 미확인 |") {
		t.Fatal("import date shown as spoken date", view)
	}
}

func TestRelearnSnapshotRejectsEvidenceChangeAndBusyApply(t *testing.T) {
	lessonFixture(t)
	path := lessonRecordFixture(t, "known", lessonID, lessonID, "원문이 바뀌면 출처를 재확인한다")
	plan, err := planLessons(time.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(mustReadLesson(t, path))+"\nchanged\n")
	if err = plan.input.verify(); err == nil {
		t.Fatal("changed source was not detected")
	}
	lock, err := exclusive(filepath.Join(Dir(), ".build.lock"))
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if out, err := Relearn(time.Now()); err != nil || out["skipped"] == nil {
		t.Fatal("busy writer was not skipped", out, err)
	}
	if _, err := PreviewRelearn(time.Now()); err != nil {
		t.Fatal("read-only preview unnecessarily required the write lock", err)
	}
}

func TestRelearnReadErrorsAndViewFailuresDoNotClaimApplied(t *testing.T) {
	lessonFixture(t)
	write(t, RecordDir(), "not a folder")
	if _, err := PreviewRelearn(time.Now()); err == nil {
		t.Fatal("invalid record folder reported as empty")
	}
	if err := os.Remove(RecordDir()); err != nil {
		t.Fatal(err)
	}
	lessonRecordFixture(t, "known", lessonID, "", "보기 갱신 실패를 성공으로 말하지 않는다")
	if err := os.MkdirAll(filepath.Join(Dir(), "배운것.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := Relearn(time.Now())
	if err == nil || out["applied"] != nil || out["lessons_saved"] != true {
		t.Fatal("partial publish reported complete", out, err)
	}
	var all []Lesson
	if err = json.Unmarshal(mustReadLesson(t, lessonsPath()), &all); err != nil || len(all) != 1 {
		t.Fatal("saved authoritative JSON was corrupted", err)
	}
}

func TestMemoryImportPreservesAllLessonsWithExactIdentity(t *testing.T) {
	lessonFixture(t)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude-profiles", "work", "projects", "p", "memory", "one.md")
	id := "memory:p/one.md"
	write(t, path, "---\ntype: feedback\ndescription: 새 설명으로 기존 본문을 덮어쓰지 않는다\n---\n")
	before := []Lesson{
		{Rule: "기존 전역 기억의 본문을 유지한다", Scope: "전체", Origin: "옛 메모리", Sessions: []string{id}, LastSeen: "2026-10-01", Status: "활성", Confirmed: true},
		{Rule: "기존 프로젝트 기억의 본문을 유지한다", Scope: "repo", Origin: "기록", Sessions: []string{id}, LastSeen: "2026-09-30", Status: "후보"},
		{Rule: "별칭만 남은 기존 기억을 유지한다", Scope: "other", Origin: "옛 메모리", Sessions: []string{"legacy"}, LastSeen: "2026-10-02", Status: "후보", Sources: []LessonSource{{Aliases: []string{id}, ImportedAt: "2026-09-28", Basis: "미확인"}}},
		{Rule: "경로만 남은 기존 기억을 유지한다", Scope: "미확인", Origin: "옛 메모리", LastSeen: "2026-10-03", Status: "후보", Sources: []LessonSource{{Raw: path, ImportedAt: "2026-09-27", Basis: "미확인"}}},
	}
	if err := saveLessons(before); err != nil {
		t.Fatal(err)
	}
	before, err := loadLessons()
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	out, err := ImportMemory(day)
	all, loadErr := loadLessons()
	if err != nil || loadErr != nil || len(all) != len(before) || out["changed"] != 4 {
		t.Fatal("existing identities were merged or duplicated", out, err, loadErr, all)
	}
	imports := []string{"2026-10-01", "2026-10-08", "2026-09-28", "2026-09-27"}
	for i, got := range all {
		got.Sources = before[i].Sources
		if !reflect.DeepEqual(got, before[i]) {
			t.Fatal("existing lesson fields changed", i, got, before[i])
		}
		found := false
		for _, source := range all[i].Sources {
			if source.Raw == path && reflect.DeepEqual(source.Aliases, []string{id}) {
				found = source.ImportedAt == imports[i] && source.Session == "" && source.Basis == "메모리 수입"
			}
		}
		if !found {
			t.Fatal("exact source or its original imported date lost", i, all[i].Sources)
		}
	}
	saved := string(mustReadLesson(t, lessonsPath()))
	if out, err := ImportMemory(day.AddDate(0, 0, 10)); err != nil || out["changed"] != 0 || string(mustReadLesson(t, lessonsPath())) != saved {
		t.Fatal("reimport rewrote existing identities or import history", out, err)
	}
}

func TestNewMemoryKeepsScopeUncertainDespiteOriginMetadata(t *testing.T) {
	lessonFixture(t)
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, ".claude-profiles", "work", "projects", "p", "memory", "one.md")
	rule := "사용자 요청 항목을 빠짐없이 답한다"
	write(t, path, "---\ntype: feedback\ndescription: "+rule+"\noriginSessionId: "+lessonID+"\n---\nAI summary: scope=전체\n")
	prior := Lesson{Rule: rule, Scope: "미확인", Origin: "옛 메모리", Sessions: []string{"memory:p/one.md-other"}, LastSeen: "2026-10-01", Status: "활성", Confirmed: true,
		Sources: []LessonSource{{Raw: path + ".other", Aliases: []string{"memory:p/one.md-other"}, ImportedAt: "2026-10-01", Basis: "메모리 수입"}}}
	if err := saveLessons([]Lesson{prior}); err != nil {
		t.Fatal(err)
	}
	stored, err := loadLessons()
	if err != nil {
		t.Fatal(err)
	}
	prior = stored[0]
	day := time.Date(2026, 10, 8, 0, 0, 0, 0, time.Local)
	plan, err := planLessons(day, false)
	if err != nil || len(plan.all) != 2 || !reflect.DeepEqual(plan.all[0], prior) {
		t.Fatal("similar content or partial identity entered the existing confirmed lesson", plan.all, err)
	}
	details := plan.result["changes"].([]map[string]any)
	if len(details) != 1 || details[0]["scope"] != "미확인" || plan.result["unresolved_connections"] != 0 {
		t.Fatal("preview omitted uncertain scope or changed ID connection semantics", details, plan.result)
	}
	got := plan.all[1]
	if got.Scope != "미확인" || got.Status != "후보" || got.Confirmed || got.Origin != "옛 메모리" || got.LastSeen != "2026-10-08" || !reflect.DeepEqual(got.Sessions, []string{"memory:p/one.md"}) {
		t.Fatal("memory summary established scope or user confirmation", got)
	}
	if len(got.Sources) != 1 || got.Sources[0].Session != "" || got.Sources[0].ImportedAt != "2026-10-08" || knownSessions(got) != 0 || sourceDate(got, false) != "미확인" {
		t.Fatal("origin metadata became verified session or spoken date", got)
	}
	if _, err := ImportMemory(day); err != nil {
		t.Fatal(err)
	}
	saved := string(mustReadLesson(t, lessonsPath()))
	if out, err := ImportMemory(day.AddDate(0, 0, 10)); err != nil || out["changed"] != 0 || string(mustReadLesson(t, lessonsPath())) != saved {
		t.Fatal("new import history advanced on reimport", out, err)
	}
}
