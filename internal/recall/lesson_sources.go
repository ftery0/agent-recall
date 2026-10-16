package recall

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

// Sources identify records, not the speaker of a lesson inferred by a model.
// SessionStart and ImportedAt never establish when the lesson was spoken.
type LessonSource struct {
	Session      string   `json:"session,omitempty"`
	Record       string   `json:"record,omitempty"`
	Raw          string   `json:"raw,omitempty"`
	SessionStart string   `json:"session_start,omitempty"`
	ImportedAt   string   `json:"imported_at,omitempty"`
	Basis        string   `json:"basis,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	extra        map[string]json.RawMessage
}

var fullSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (l *Lesson) UnmarshalJSON(raw []byte) error {
	type plain Lesson
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*l = Lesson(p)
	return unknownFields(raw, &l.extra, "rule", "scope", "origin", "sessions", "last_seen", "status", "confirmed", "sources")
}

func (l Lesson) MarshalJSON() ([]byte, error) {
	type plain Lesson
	return withUnknown(plain(l), l.extra)
}

func (s *LessonSource) UnmarshalJSON(raw []byte) error {
	type plain LessonSource
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*s = LessonSource(p)
	return unknownFields(raw, &s.extra, "session", "record", "raw", "session_start", "imported_at", "basis", "aliases")
}

func (s LessonSource) MarshalJSON() ([]byte, error) {
	type plain LessonSource
	return withUnknown(plain(s), s.extra)
}

func unknownFields(raw []byte, out *map[string]json.RawMessage, known ...string) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}
	for _, key := range known {
		delete(*out, key)
	}
	return nil
}

func withUnknown(known any, extra map[string]json.RawMessage) ([]byte, error) {
	raw, err := json.Marshal(known)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, value := range extra {
		if _, exists := fields[key]; !exists {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func mergeSource(all []Lesson, add Lesson, session, day string, source LessonSource) []Lesson {
	if len([]rune(strings.TrimSpace(mask(add.Rule)))) < 6 {
		return all
	}
	index := len(all)
	for i := range all {
		if all[i].Scope == add.Scope && overlap(all[i].Rule, strings.TrimSpace(mask(add.Rule))) >= sameLesson {
			index = i
			break
		}
	}
	all = merge(all, add, session, day)
	if index < len(all) {
		addSource(&all[index], source)
	}
	return all
}

func addSource(l *Lesson, add LessonSource) {
	for i, s := range l.Sources {
		if s.Session != add.Session || s.Record != add.Record || s.Raw != add.Raw {
			continue
		}
		for _, alias := range add.Aliases {
			if !slices.Contains(l.Sources[i].Aliases, alias) {
				l.Sources[i].Aliases = append(l.Sources[i].Aliases, alias)
			}
		}
		if add.Basis != "" {
			l.Sources[i].Basis = add.Basis
		}
		l.Sources[i].SessionStart = max(s.SessionStart, add.SessionStart)
		if s.ImportedAt == "" {
			l.Sources[i].ImportedAt = add.ImportedAt
		}
		return
	}
	l.Sources = append(l.Sources, add)
}

func knownSessions(l Lesson) int {
	known := map[string]bool{}
	for _, s := range l.Sources {
		if fullSessionID.MatchString(s.Session) && s.Basis != "" && s.Basis != "미확인" {
			known[s.Session] = true
		}
	}
	return len(known)
}

func unknownSessions(l Lesson) int {
	known := map[string]bool{}
	for _, s := range l.Sources {
		if s.Basis == "" || s.Basis == "미확인" {
			continue
		}
		known[s.Session] = true
		for _, alias := range s.Aliases {
			known[alias] = true
		}
	}
	unknown := map[string]bool{}
	for _, id := range l.Sessions {
		if !known[id] {
			unknown[id] = true
		}
	}
	return len(unknown)
}

func latestSourceDate(l Lesson, imported bool) string {
	day := ""
	for _, s := range l.Sources {
		if imported {
			day = max(day, s.ImportedAt)
		} else if s.Basis != "" && s.Basis != "미확인" {
			day = max(day, s.SessionStart)
		}
	}
	return day
}

func sourceDate(l Lesson, imported bool) string {
	if day := latestSourceDate(l, imported); day != "" {
		return day
	}
	return "미확인"
}
