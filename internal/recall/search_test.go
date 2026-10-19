package recall

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFoldComposesHangulJamo(t *testing.T) {
	if got := fold("한글 ABC"); got != "한글 abc" {
		t.Fatalf("자모를 음절로 붙이지 못함: %q", got)
	}
}

func TestKeysSplitHangulAndIdentifiers(t *testing.T) {
	var got []string
	keys(compose("세션에서 useBoardVersions TSK-1344 왜"), func(k string) { got = append(got, k) })
	for _, want := range []string{"세션", "션에", "에서", "useboardversions", "board", "versions", "tsk-1344", "tsk", "1344", "왜"} {
		if !slices.Contains(got, want) {
			t.Fatalf("missing %q in %v", want, got)
		}
	}
}

func TestFindShowsTierRequestAndFilters(t *testing.T) {
	recordFixture(t)
	record := func(name, repo, request, answer string) {
		write(t, filepath.Join(RecordDir(), "2026-10-07", name+".md"), "---\nsession: "+name+"-0000-4000-8000-000000000000\nrepo: "+repo+
			"\ndate: 2026-10-07\n---\n\n# "+request+"\n\n## 내용\n\n### 요청\n- 10-07 10:00 [req-"+name+" 줄12 claude] "+request+"\n  → AI(주장): "+answer+"\n")
	}
	record("aaaaaaaa", "web", "세션에서 헤더 숨김 빼줘", "useBoardVersions 를 고쳤습니다")
	record("bbbbbbbb", "api", "헤더 색만 바꿔줘", "바꿨습니다")
	out := FindText("헤더를 숨김", 5, FindFilter{})
	// The title is the first request, so the line shown is its answer.
	if !strings.Contains(out, "· 세션에서 헤더 숨김 빼줘 · [단어형] → AI(주장): useBoardVersions") || !strings.Contains(out, "요청 req-aaaaaaaa 줄12") {
		t.Fatal("맞은 단계나 요청 위치가 없음", out)
	}
	if lines := strings.Split(out, "\n"); len(lines) != 2 || !strings.Contains(lines[1], "· 헤더 색만 바꿔줘 · [부분]") {
		t.Fatal("모든 말을 가진 기록이 먼저 오지 않음", out)
	}
	if out := FindText("board versions", 5, FindFilter{}); !strings.Contains(out, "→ AI(주장): useBoardVersions") {
		t.Fatal("식별자 조각으로 찾지 못하거나 답 줄을 보이지 않음", out)
	}
	if out := FindText("헤더", 5, FindFilter{Repo: "api"}); strings.Contains(out, "aaaaaaaa") || !strings.Contains(out, "bbbbbbbb") {
		t.Fatal("레포로 거르지 못함", out)
	}
	if out := FindText("헤더", 5, FindFilter{Since: time.Now().AddDate(0, 0, 1)}); !strings.HasPrefix(out, "찾은 기록 없음") {
		t.Fatal("날짜로 거르지 못함", out)
	}
}

func TestTheShownLineNeverRepeatsTheTitle(t *testing.T) {
	forms := []string{"촬영"}
	if got, _ := bestLine(searchUnit{show: "촬영 설정 맞춰줘\n→ AI(주장): 맞췄습니다"}, forms, "촬영 설정 맞춰줘"); got != "→ AI(주장): 맞췄습니다" {
		t.Fatal("제목인 첫 요청을 다시 보임", got)
	}
	words := strings.TrimSpace(strings.Repeat("촬영 각도 ", 20))
	if got, _ := bestLine(searchUnit{show: words}, forms, cut(words, 60)); got != "…"+strings.TrimSpace(string([]rune(words)[60:])) {
		t.Fatal("잘린 제목 뒤의 말 대신 요청 전체를 다시 보임", got)
	}
	if got, n := bestLine(searchUnit{show: "촬영 설정\n- 다른 줄\n- 촬영 각도", heading: true}, forms, "노트"); got != "- 촬영 각도" || n != 1 {
		t.Fatal("절 제목이 같은 수의 말을 가진 아래 줄을 이김", got)
	}
	recordFixture(t)
	write(t, filepath.Join(os.Getenv("HARNESS_VAULT_DIR"), "personal", "10_프로젝트", "P", "작업 기록", "촬영 메모.md"), "# 촬영 메모\n\n- 각도 정리\n")
	if out := FindText("촬영 메모", 5, FindFilter{}); !strings.Contains(out, "· 촬영 메모 · [") || !strings.Contains(out, "] - 각도 정리") || strings.Contains(out, "] 촬영 메모") {
		t.Fatal("노트 제목을 대표 줄로 다시 보임", out)
	}
	// A title from the file name stands alone before the first heading; the line is found under it.
	write(t, filepath.Join(os.Getenv("HARNESS_VAULT_DIR"), "personal", "10_프로젝트", "P", "작업 기록", "TSK-9 공통 메모.md"), "---\nticket: TSK-9\n---\n## 목표\n화면 정리\n\n## 결정\n- 다른 결정\n- 하위 없이 부모 TSK-9 만\n")
	if out := FindText("TSK-9", 5, FindFilter{}); !strings.Contains(out, "· TSK-9 공통 메모 · [정확] - 하위 없이 부모 TSK-9 만") {
		t.Fatal("제목으로만 맞은 노트에서 말이 든 줄을 보이지 못함", out)
	}
}
