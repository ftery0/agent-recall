package recall

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ftery0/agent-recall/internal/memory"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type lessonInput struct {
	data     []byte
	metadata bool
}

// Only the records, imported notes, current lessons and used metadata are tracked.
// Raw transcripts are not fully hashed or sent anywhere.
type lessonInputs struct {
	files    map[string]lessonInput
	patterns map[string][]string
}

func newLessonInputs() *lessonInputs {
	return &lessonInputs{files: map[string]lessonInput{}, patterns: map[string][]string{}}
}

func (in *lessonInputs) paths(pattern string) ([]string, error) {
	if paths, ok := in.patterns[pattern]; ok {
		return paths, nil
	}
	var paths []string
	var err error
	if pattern == filepath.Join(RecordDir(), "*", "*.md") {
		err = filepath.WalkDir(RecordDir(), func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if path == RecordDir() && os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			rel, _ := filepath.Rel(RecordDir(), path)
			depth := len(strings.Split(rel, string(filepath.Separator)))
			if d.IsDir() && depth >= 2 {
				return filepath.SkipDir
			}
			if !d.IsDir() && depth == 2 && filepath.Ext(path) == ".md" {
				paths = append(paths, path)
			}
			return nil
		})
		if err == nil {
			if info, statErr := os.Stat(RecordDir()); statErr == nil && !info.IsDir() {
				err = fmt.Errorf("기록 경로가 폴더가 아님")
			}
		}
	} else {
		paths, err = filepath.Glob(pattern)
	}
	if err != nil {
		return nil, fmt.Errorf("교훈 출처 목록 읽기 미완료: %s", pattern)
	}
	sort.Strings(paths)
	in.patterns[pattern] = paths
	return paths, nil
}

func (in *lessonInputs) read(path string, metadata bool) ([]byte, error) {
	if input, ok := in.files[path]; ok {
		return input.data, nil
	}
	var raw []byte
	var err error
	if metadata {
		raw, err = readLessonMetadata(path)
	} else {
		raw, err = os.ReadFile(path)
	}
	if os.IsNotExist(err) {
		raw, err = nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("교훈 출처 읽기 미완료: %s", path)
	}
	in.files[path] = lessonInput{data: raw, metadata: metadata}
	return raw, nil
}

func readLessonMetadata(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	var raw bytes.Buffer
	for lines := 0; lines < 32 && scan.Scan(); lines++ {
		raw.Write(scan.Bytes())
		raw.WriteByte('\n')
		if id, day := lessonMetadata(raw.Bytes()); id != "" && day != "" {
			break
		}
	}
	if err = scan.Err(); err != nil {
		return nil, err
	}
	after, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("원문 메타데이터를 읽는 중 변경")
	}
	return raw.Bytes(), nil
}

func lessonMetadata(raw []byte) (id, day string) {
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var row map[string]any
		if json.Unmarshal(line, &row) != nil {
			continue
		}
		if row["isSidechain"] == true {
			return "", ""
		}
		if at := stamp(row["timestamp"]); day == "" && !at.IsZero() {
			day = at.Local().Format("2006-01-02")
		}
		if value, _ := row["sessionId"].(string); fullSessionID.MatchString(value) {
			id = value
		}
		if row["type"] == "session_meta" {
			payload, _ := row["payload"].(map[string]any)
			if source, ok := payload["source"].(map[string]any); ok && source["subagent"] != nil {
				return "", ""
			}
			if value, _ := payload["id"].(string); fullSessionID.MatchString(value) {
				id = value
			}
		}
	}
	return id, day
}

func (in *lessonInputs) token() string {
	var files []string
	for path, input := range in.files {
		digest := sha256.Sum256(input.data)
		files = append(files, fmt.Sprintf("%s:%t:%t:%x", path, input.metadata, input.data == nil, digest))
	}
	sort.Strings(files)
	digest := sha256.Sum256(memory.Packed(map[string]any{"files": files, "patterns": in.patterns}))
	return hex.EncodeToString(digest[:])
}

func (in *lessonInputs) verify() error {
	current := newLessonInputs()
	for pattern := range in.patterns {
		if _, err := current.paths(pattern); err != nil {
			return err
		}
	}
	for path, input := range in.files {
		if _, err := current.read(path, input.metadata); err != nil {
			return err
		}
	}
	if in.token() != current.token() {
		return fmt.Errorf("교훈 집계 출처가 변경됨: 다시 미리보기한다")
	}
	return nil
}
