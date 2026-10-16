package recall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHintFindsTheDayTicketAndUnfinishedWorkARequestPointsTo(t *testing.T) {
	_, night, morning := twoDayFixture(t)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	vault := os.Getenv("HARNESS_VAULT_DIR")
	write(t, filepath.Join(vault, "personal", "10_프로젝트", "P", "작업 기록", "영상 탭.md"), "---\nstatus: 진행\nrepos: repo\n---\n## 현재 상태\n- TSK-1344 촬영 설정 맞추기, 백엔드 커밋 b7306a2b\n")
	if text := HintText("apps/user/DetailModal.tsx 이 파일 무슨 역할인지 알려줘", "/work/repo", "", now); text != "" {
		t.Fatal("과거를 말하지 않은 질문에 기억을 붙임", text)
	}
	text := HintText("어제 한 거 보고 오늘 할 일 정리해줘", "/work/repo", "", now)
	for _, want := range []string{"=== 하네스가 질문을 보고 먼저 찾은 기억", "[2026-10-08 대화 2개 — 전부: harness recall day 2026-10-08]",
		"- repo · 밤에 시작한 일 · 그날 마지막 09:10 아침에 이어서 한 일 (세션 " + short(night) + ")",
		"- repo · 다른 대화 첫 요청 · 그날 마지막 10:00 다른 대화 두번째 요청 (세션 " + short(morning) + ")"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q\n%s", want, text)
		}
	}
	logs := "[Image #16] <pasted_content id=\"887d\">\n2026-10-08 [info] TSK-1344 PR #12 done\n</pasted_content id=\"887d\">\n이미지가 안 만들어져"
	if text := HintText(logs, "/work/repo", "", now) + HintText("2026-10-08 14:18:59 [info] Credit held", "/work/repo", "", now); text != "" {
		t.Fatal("붙여 넣은 글이나 로그 시각을 질문으로 읽음", text)
	}
	if text := HintText("확인한 편집 후 다른 변경: .plans/2026-10-07-영상-탭-시안/v2.html, 기획/2026-10-08 작업안.md", "/work/repo", "", now) + HintText("저번주부터 만든 이미지 지워줘", "/work/repo", "", now); text != "" {
		t.Fatal("파일 경로 속 날짜나 '저번주'를 지난 일로 읽음", text)
	}
	if text := HintText("2026-10-07 에 뭐 했지?", "/work/repo", "", now); !strings.Contains(text, "[2026-10-07 대화 1개") {
		t.Fatal("날짜를 읽지 못함", text)
	}
	if text := HintText("10월 7일에 내가 뭐 했었지?", "/work/repo", "", now); !strings.Contains(text, "[2026-10-07 대화 1개") || !strings.Contains(text, "그날 마지막 23:50 밤에 시작한 일") {
		t.Fatal("날짜를 읽지 못함", text)
	}
	if text := HintText("tsk-1344 어디까지 했었지?", "/work/repo", "", now); !strings.Contains(text, "[TSK-1344 — 더: harness recall find TSK-1344]\n- 영상 탭 · - TSK-1344 촬영 설정 맞추기") {
		t.Fatal("티켓을 찾지 못함", text)
	}
	text = HintText("우리 전에 하던 거 이어서 하자", "/work/repo", night, now)
	if !strings.Contains(text, "[이 레포 최근 대화 — 원문: harness recall source <세션> <말>]\n- 10-08 10:00 다른 대화 첫 요청 — 마지막 요청 다른 대화 두번째 요청 · 끝난 지점(AI 주장) 두번째 요청도 끝냈습니다. (세션 "+short(morning)+")") || strings.Contains(text, short(night)) {
		t.Fatal("지금 대화를 빼고 최근 대화를 보이지 못함", text)
	}
	if text := HintText(strings.Repeat("어제 TSK-1344 PR #12 ", 50), "/work/repo", "", now); len([]rune(text)) > hintLimit+1 {
		t.Fatal("붙이는 글이 한도를 넘음", len([]rune(text)))
	}
}

func TestHintSaysWhetherAPullRequestWasMerged(t *testing.T) {
	recordFixture(t)
	repo := filepath.Join(t.TempDir(), "web")
	merged := time.Date(2026, 10, 14, 8, 53, 45, 0, time.UTC)
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "Merge pull request #377 from me/feature"}} {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_COMMITTER_DATE="+merged.Format(time.RFC3339), "GIT_AUTHOR_DATE="+merged.Format(time.RFC3339))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.Local)
	if text := HintText("PR #377 어떻게 됐어?", repo, "", now); !strings.Contains(text, "[PR #377] 내 컴퓨터 git 기록: "+merged.Local().Format("01-02 15:04")+" 머지됨 (") {
		t.Fatal("머지를 알리지 못함", text)
	}
	if text := HintText("PR 380 은?", repo, "", now); !strings.Contains(text, "[PR #380] 내 컴퓨터 git 기록에 머지 없음 — 아직 안 받아 왔을 수 있으니 gh pr view 380 로 확인") {
		t.Fatal("머지가 없을 때 안내가 없음", text)
	}
	if text := HintText("PR 2개로 나눠줘", repo, "", now); text != "" {
		t.Fatal("개수를 PR 번호로 읽음", text)
	}
}
