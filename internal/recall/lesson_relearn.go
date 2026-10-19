package recall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

type lessonRecord struct {
	source LessonSource
	scope  string
	rules  []string
	id     string
}

type lessonPlan struct {
	all    []Lesson
	input  *lessonInputs
	result map[string]any
	token  string
}

// PreviewRelearn reads the same evidence as apply without creating even a lock file.
func PreviewRelearn(day time.Time) (map[string]any, error) {
	plan, err := planLessons(day, true)
	if err != nil {
		return nil, err
	}
	if err = plan.input.verify(); err != nil {
		return nil, err
	}
	plan.result["preview"] = true
	return plan.result, nil
}

func Relearn(day time.Time) (map[string]any, error) {
	return RelearnChecked(day, "")
}

// RelearnChecked rejects a changed preview and checks its inputs again before publishing.
func RelearnChecked(day time.Time, expectedToken string) (map[string]any, error) {
	return applyLessons(day, true, expectedToken)
}

// ImportMemory only reads the original memory files; imported lessons remain candidates.
func ImportMemory(day time.Time) (map[string]any, error) {
	return applyLessons(day, false, "")
}

func applyLessons(day time.Time, records bool, expected string) (map[string]any, error) {
	lock, err := exclusive(filepath.Join(Dir(), ".build.lock"))
	if lock == nil {
		return map[string]any{"skipped": "a build is running"}, err
	}
	defer lock.Close()
	plan, err := planLessons(day, records)
	if err != nil {
		return nil, err
	}
	if expected != "" && plan.token != expected {
		return nil, fmt.Errorf("교훈 미리보기의 출처 또는 계산 결과 변경: 다시 미리보기한다")
	}
	if err = plan.input.verify(); err != nil {
		return nil, err
	}
	if plan.result["changed"].(int) > 0 || plan.input.files[lessonsPath()].data == nil {
		if err = saveLessons(plan.all); err != nil {
			plan.result["lessons_saved"] = "미확인"
			return plan.result, fmt.Errorf("교훈 집계 저장 결과 미확인: 현재 JSON을 다시 읽고 미리보기한다: %w", err)
		}
	}
	plan.result["lessons_saved"] = true
	if err = renderLessons(plan.all); err != nil {
		return plan.result, fmt.Errorf("교훈 집계는 저장했으나 보기 갱신 미완료: %w", err)
	}
	plan.result["applied"] = true
	return plan.result, nil
}

func planLessons(day time.Time, records bool) (lessonPlan, error) {
	plan := lessonPlan{input: newLessonInputs()}
	raw, err := plan.input.read(lessonsPath(), false)
	if err != nil {
		return plan, err
	}
	if raw != nil {
		if err = json.Unmarshal(raw, &plan.all); err != nil {
			return plan, fmt.Errorf("교훈 JSON 해석 실패")
		}
	}
	before := make([][]byte, len(plan.all))
	for i, l := range plan.all {
		before[i] = memory.Packed(l)
	}
	var catalogue []lessonRecord
	aliasesMerged := 0
	if records {
		catalogue, err = readLessonRecords(plan.input)
		if err != nil {
			return plan, err
		}
		for i := range plan.all {
			oldCount := len(plan.all[i].Sessions)
			normalizeLesson(&plan.all[i], catalogue)
			aliasesMerged += max(0, oldCount-len(plan.all[i].Sessions))
			if plan.all[i].Status == "활성" && !plan.all[i].Confirmed {
				plan.all[i].Status = "후보"
			}
		}
		for _, r := range catalogue {
			for _, rule := range r.rules {
				plan.all = mergeSource(plan.all, Lesson{Rule: rule, Scope: r.scope, Origin: "기록"}, r.id, r.source.SessionStart, r.source)
			}
		}
	}
	if err = readLessonMemories(&plan, day); err != nil {
		return plan, err
	}
	changed, active, unresolved, aliases := 0, 0, 0, 0
	reasons := map[string]int{}
	var details []map[string]any
	for i, l := range plan.all {
		if i >= len(before) || string(before[i]) != string(memory.Packed(l)) {
			changed++
			var prior Lesson
			var fields []string
			if i >= len(before) {
				reasons["new_candidates"]++
				fields = append(fields, "new_candidate")
			} else {
				_ = json.Unmarshal(before[i], &prior)
				var previous, next map[string]json.RawMessage
				_ = json.Unmarshal(before[i], &previous)
				_ = json.Unmarshal(memory.Packed(l), &next)
				for _, field := range []string{"sources", "sessions", "last_seen", "status", "confirmed"} {
					if string(previous[field]) != string(next[field]) {
						reasons[field]++
						fields = append(fields, field)
					}
				}
			}
			hash := sha256.Sum256([]byte(l.Scope + "\x00" + l.Rule))
			details = append(details, map[string]any{"index": i, "lesson_id": hex.EncodeToString(hash[:8]), "scope": l.Scope, "fields": fields, "sessions_before": prior.Sessions, "sessions_after": l.Sessions, "sources_before": len(prior.Sources), "sources_after": len(l.Sources), "known_sessions_before": knownSessions(prior), "known_sessions_after": knownSessions(l), "unknown_before": unknownSessions(prior), "unknown_after": unknownSessions(l)})
		}
		if l.Status == "활성" && l.Confirmed {
			active++
		}
		unresolved += unknownSessions(l)
		for _, s := range l.Sources {
			aliases += len(s.Aliases)
		}
	}
	digest := sha256.Sum256(append([]byte(plan.input.token()), memory.Packed(plan.all)...))
	plan.token = hex.EncodeToString(digest[:])
	plan.result = map[string]any{"total": len(plan.all), "active": active, "changed": changed, "unresolved_connections": unresolved, "preserved_aliases": aliases, "aliases_merged": aliasesMerged, "record_sources": len(catalogue), "source_token": plan.token, "spoken_dates": "미확인", "change_reasons": reasons, "changes": details}
	return plan, nil
}

func readLessonRecords(input *lessonInputs) ([]lessonRecord, error) {
	paths, err := input.paths(filepath.Join(RecordDir(), "*", "*.md"))
	if err != nil {
		return nil, err
	}
	var records []lessonRecord
	for _, path := range paths {
		// Read through the inputs, not loadRecords: the preview's token covers exactly these bytes.
		raw, err := input.read(path, false)
		if err != nil || raw == nil {
			return nil, fmt.Errorf("교훈 기록 읽기 미완료: %s", path)
		}
		rec := record{path: path, raw: raw, head: frontMatter(raw)}
		id := rec.field("session")
		if id == "" {
			id = "record:" + path
		}
		source := LessonSource{Record: path, Raw: rec.field("raw"), Basis: "미확인"}
		if fullSessionID.MatchString(id) {
			source.Session, source.Basis = id, "기록 세션"
			source.SessionStart = validLessonDay(rec.field("date"))
		}
		if source.Raw != "" {
			meta, err := input.read(source.Raw, true)
			if err != nil {
				return nil, err
			}
			rawID, start := lessonMetadata(meta)
			if rawID != "" {
				if id == rawID || sessionAlias(id, rawID) {
					id, source.Session, source.Basis = rawID, rawID, "원문 세션"
					if start != "" {
						source.SessionStart = start
					}
				} else {
					source.Session, source.SessionStart, source.Basis = "", "", "미확인"
				}
			}
		}
		m, _ := rec.summary()
		records = append(records, lessonRecord{source: source, scope: rec.field("repo"), rules: m.Lessons, id: id})
	}
	return records, nil
}

func normalizeLesson(l *Lesson, catalogue []lessonRecord) {
	for i := range l.Sources {
		if l.Sources[i].Session != "" {
			l.Sources[i].Basis = "미확인"
		}
	}
	var sessions []string
	for _, old := range l.Sessions {
		matches := map[string]lessonRecord{}
		for _, r := range catalogue {
			if r.source.Session != "" && (old == r.source.Session || sessionAlias(old, r.source.Session)) {
				matches[r.source.Session] = r
			}
		}
		id := old
		if len(matches) == 1 {
			for full, r := range matches {
				if l.Scope != "전체" && l.Scope != r.scope {
					continue
				}
				id = full
				if old != full {
					r.source.Aliases = []string{old}
				}
				addSource(l, r.source)
			}
		}
		if !slices.Contains(sessions, id) {
			sessions = append(sessions, id)
		}
	}
	l.Sessions = sessions
}

func sessionAlias(alias, full string) bool {
	return len(alias) == 8 && fullSessionID.MatchString(full) && (strings.HasPrefix(full, alias) || strings.HasSuffix(full, alias))
}

func validLessonDay(day string) string {
	if _, err := time.Parse("2006-01-02", day); err == nil {
		return day
	}
	return ""
}

func readLessonMemories(plan *lessonPlan, day time.Time) error {
	home, _ := os.UserHomeDir()
	var paths []string
	for _, pattern := range []string{filepath.Join(home, ".claude-profiles", "*", "projects", "*", "memory", "*.md"), filepath.Join(home, ".claude", "projects", "*", "memory", "*.md")} {
		found, err := plan.input.paths(pattern)
		if err != nil {
			return err
		}
		paths = append(paths, found...)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if filepath.Base(path) == "MEMORY.md" {
			continue
		}
		raw, err := plan.input.read(path, false)
		if err != nil || raw == nil {
			return fmt.Errorf("옛 메모리 읽기 미완료: %s", path)
		}
		kind, desc := "", ""
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "type:"); ok {
				kind = strings.TrimSpace(v)
			}
			if v, ok := strings.CutPrefix(line, "description:"); ok {
				desc = strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
		if (kind != "feedback" && kind != "user") || desc == "" {
			continue
		}
		project := filepath.Base(filepath.Dir(filepath.Dir(path)))
		id := "memory:" + project + "/" + filepath.Base(path)
		source := LessonSource{Raw: path, ImportedAt: day.Format("2006-01-02"), Basis: "메모리 수입", Aliases: []string{id}}
		matched := false
		for i := range plan.all {
			l := &plan.all[i]
			found := slices.Contains(l.Sessions, id)
			imported := source.ImportedAt
			if l.Origin == "옛 메모리" && validLessonDay(l.LastSeen) != "" {
				imported = l.LastSeen
			}
			for _, s := range l.Sources {
				if s.Raw != path && !slices.Contains(s.Aliases, id) {
					continue
				}
				found = true
				if s.ImportedAt != "" {
					imported = s.ImportedAt
				}
			}
			if found {
				// Exact memory identity enriches evidence without rewriting an existing lesson.
				add := source
				add.ImportedAt = imported
				addSource(l, add)
				matched = true
			}
		}
		if matched {
			continue
		}
		// Similar descriptions do not establish shared scope or confirmation across sources.
		plan.all = append(plan.all, mergeSource(nil, Lesson{Rule: desc, Scope: "미확인", Origin: "옛 메모리"}, id, source.ImportedAt, source)...)
	}
	return nil
}
