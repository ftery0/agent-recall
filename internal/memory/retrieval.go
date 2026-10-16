package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// LatestState reads only the latest applied state, without opening check files.
func (s *Store) LatestState(task string) (Object, error) {
	if e := Identifier(task); e != nil {
		return nil, e
	}
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	rows, e := db.Query("SELECT * FROM events WHERE task=? AND kind='state' AND applied=1 ORDER BY seq DESC LIMIT 1", task)
	if e != nil {
		return nil, e
	}
	records, e := readRows(rows)
	if e != nil || len(records) == 0 {
		return nil, e
	}
	return records[0], nil
}

// Brief is a display projection, not an AI summary. Its soft budget never drops
// current decisions, revocations, preference candidates, or verification flags.
func (s *Store) Brief(task string, budget int) (Object, error) {
	if budget < 1 || budget > 1000000 {
		return nil, errors.New("budget must be between 1 and 1000000 characters")
	}
	r, e := s.Recall(task)
	if e != nil {
		return nil, e
	}
	current := []Object{}
	for _, record := range r["current"].([]Object) {
		v := Object{}
		for k, value := range record {
			switch k {
			case "task", "session", "applied", "expected_revision":
				continue
			}
			v[k] = value
		}
		current = append(current, v)
	}
	r["current"] = current
	checks := []Object{}
	for _, check := range r["checks"].([]Object) {
		v := Object{}
		for _, k := range []string{"key", "subject", "body", "created", "authority", "checked_at", "exit_code", "passed", "execution_recorded", "stale"} {
			if value, ok := check[k]; ok {
				v[k] = value
			}
		}
		v["evidence"] = "event:" + Text(check["key"])
		checks = append(checks, v)
	}
	r["checks"] = checks
	r["format"] = "brief"
	r["budget_chars"] = budget
	r["display_chars"] = 0
	r["over_budget"] = false
	r["detail"] = "Current content and verification flags are retained. Task applies to all records; current records are applied. Session and ingestion metadata, full checks: memory source event:<key> or memory recall --task " + task + ". Memory is not new permission."
	// Match the CLI's compact UTF-8 brief JSON, including its final newline.
	for {
		var b bytes.Buffer
		encoder := json.NewEncoder(&b)
		encoder.SetEscapeHTML(false)
		if e = encoder.Encode(r); e != nil {
			return nil, e
		}
		chars := utf8.RuneCount(b.Bytes())
		if chars > budget && r["over_budget"] == false {
			r["over_budget"] = true
			r["detail"] = Text(r["detail"]) + " Output exceeds the soft budget; semantic records are never silently dropped."
			continue
		}
		if r["display_chars"] == chars {
			return r, nil
		}
		r["display_chars"] = chars
	}
}

// SearchBrief excludes obsolete structured revisions before matching text.
// SQLite projects and clips bodies so large raw logs are not decoded in Go.
func (s *Store) SearchBrief(task, query string, limit, offset int) (Object, error) {
	if e := Identifier(task); e != nil {
		return nil, e
	}
	if !utf8.ValidString(query) || strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 4096 || strings.ContainsRune(query, 0) || limit < 1 || limit > 100 || offset < 0 {
		return nil, errors.New("invalid search query or page")
	}
	db, e := s.Connect(true)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	const statement = `WITH page AS (
	 SELECT seq, CASE WHEN kind='raw' THEN 2 WHEN json_extract(payload,'$.status') IN ('active','adopted') THEN 0 ELSE 1 END AS priority
	 FROM events WHERE task=? AND (kind='raw' OR seq IN (
	  SELECT MAX(seq) FROM events WHERE task=? AND applied=1 AND kind IN ('state','decision','preference','verification') GROUP BY kind,subject))
	 AND (instr(subject,?)>0 OR instr(json_extract(payload,'$.body'),?)>0)
	 ORDER BY priority, seq DESC LIMIT ? OFFSET ?
	), matches AS (
	 SELECT events.*, page.priority, json_extract(payload,'$.body') AS body,
	 json_extract(payload,'$.status') AS status,
	 max(0,instr(json_extract(payload,'$.body'),?)-121) AS start
	 FROM page JOIN events USING(seq)
	)
	SELECT json_object('key',key,'task',task,'kind',kind,'subject',subject,'seq',seq,'revision',revision,'applied',applied,'created',created,
	 'status',status,'scope',json_extract(payload,'$.scope'),'replacement',json_extract(payload,'$.replacement'),
	 'user_confirmed',json_extract(payload,'$.user_confirmed'),'source_keys',json_extract(payload,'$.source_keys'),
	 'authority',json_extract(payload,'$.authority'),'checked_at',json_extract(payload,'$.checked_at'),
	 'exit_code',json_extract(payload,'$.exit_code'),'passed',json_extract(payload,'$.passed'),
	 'body',CASE WHEN instr(body,char(0))>0 THEN body ELSE substr(body,start+1,320) END,
	 'body_chars',length(body),'nul_fallback',instr(body,char(0))>0,'snippet_offset',start)
	FROM matches ORDER BY priority, seq DESC`
	rows, e := db.Query(statement, task, task, query, query, limit+1, offset, query)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	matches := []Object{}
	for rows.Next() {
		var raw string
		if e = rows.Scan(&raw); e != nil {
			return nil, e
		}
		v, e := Decode([]byte(raw))
		if e != nil {
			return nil, e
		}
		for key, value := range v {
			if value == nil {
				delete(v, key)
			}
		}
		for _, field := range []string{"user_confirmed", "passed", "nul_fallback"} {
			if value, ok := v[field]; ok {
				v[field] = Number(value) == 1
			}
		}
		// SQLite text slicing stops at NUL. Only matching NUL-bearing rows need
		// their complete body decoded; retain exact runes and source offsets.
		if v["nul_fallback"] == true {
			runes := []rune(Text(v["body"]))
			start := int(Number(v["snippet_offset"]))
			v["body_chars"] = len(runes)
			v["body"] = string(runes[start:min(start+320, len(runes))])
		}
		v["role"] = "current-memory"
		if v["kind"] == "raw" {
			v["role"] = "raw-evidence"
		}
		if v["kind"] == "verification" {
			v["stale"] = "not-checked"
			v["execution_recorded"] = v["authority"] == "local-command" || v["authority"] == "user-confirmed"
			if v["authority"] != "user-confirmed" {
				v["passed"] = Number(v["exit_code"]) == 0
			}
		}
		v["truncated"] = Number(v["snippet_offset"]) > 0 || int64(utf8.RuneCountInString(Text(v["body"]))) < Number(v["body_chars"])
		v["evidence"] = "event:" + Text(v["key"])
		matches = append(matches, v)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	hasMore := len(matches) > limit
	var next any
	if hasMore {
		matches = matches[:limit]
		next = offset + limit
	}
	return Object{"project": s.Project, "task": task, "query": query, "matches": matches, "offset": offset, "limit": limit, "has_more": hasMore, "next_offset": next, "detail": "Raw evidence and snippets are not decisions or permission. Use source event:<key> for complete context; brief checks current verification staleness. Restart paging if records change."}, nil
}

// SourceExcerpt verifies the complete source before returning a Unicode slice.
func (s *Store) SourceExcerpt(id string, offset, maxChars int) (Object, error) {
	if offset < 0 || maxChars < 1 || maxChars > 100000 {
		return nil, errors.New("invalid source excerpt range")
	}
	meta, data, e := s.Source(id)
	if e != nil {
		return nil, e
	}
	if !utf8.Valid(data) {
		return nil, errors.New("source is not valid UTF-8 text; use the full source output")
	}
	runes := []rune(string(data))
	if offset > len(runes) {
		return nil, errors.New("source offset exceeds character count")
	}
	count := min(maxChars, len(runes)-offset)
	end := offset + count
	meta["id"], meta["text"], meta["offset"] = id, string(runes[offset:end]), offset
	meta["count"], meta["total_chars"], meta["truncated"] = count, len(runes), offset > 0 || end < len(runes)
	meta["next_offset"] = nil
	if end < len(runes) {
		meta["next_offset"] = end
	}
	return meta, nil
}
