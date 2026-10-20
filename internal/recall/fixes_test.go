package recall

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tries(steps ...string) []tried {
	var out []tried
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	for i, step := range steps {
		failed := strings.HasPrefix(step, "!")
		out = append(out, tried{text: strings.TrimPrefix(step, "!"), source: "tool:t" + string(rune('a'+i)), done: true, failed: failed, at: at.Add(time.Duration(i) * time.Second)})
	}
	return out
}

func TestFixPairsKeepWhatGotPastAFailure(t *testing.T) {
	got := fixPairs(tries(`!webqa shot http://front.example.com:3000/ko/home`, `ls`, `curl -sk https://front.example.com:3000/ko/home`))
	if len(got) != 1 || got[0].kind != "addr" || got[0].failed != "http://front.example.com:3000" || got[0].worked != "https://front.example.com:3000" {
		t.Fatal("다른 프로그램으로 된 주소 고침을 놓침", got)
	}
	got = fixPairs(tries(`!pnpm --filter @potio/mobile add expo-secure-store`, `pnpm --store-dir /x/store --filter @potio/mobile add expo-secure-store`))
	if len(got) != 1 || got[0].kind != "cmd" || got[0].source != "tool:ta" {
		t.Fatal("같은 프로그램의 고친 명령을 놓침", got)
	}
	for name, steps := range map[string][]string{
		"플래그를 바꿈":       {`!CI=true pnpm install --lockfile-only`, `CI=true pnpm install --no-frozen-lockfile`},
		"프로그램 경로를 씀":    {`!opencode --version`, `~/.opencode/bin/opencode --version`},
		"주소의 host 를 바꿈": {`!curl -I http://localhost:3015/admin/users`, `curl -I http://127.0.0.1:3015/admin/users`},
		"따옴표로 감쌈":       {`!gh api repos/o/r/contents/a.ts?ref=main`, `gh api 'repos/o/r/contents/a.ts?ref=main'`},
	} {
		if got := fixPairs(tries(steps...)); len(got) != 1 || got[0].kind != "cmd" {
			t.Error(name, "을 고침으로 보지 않음", got)
		}
	}
	for name, steps := range map[string][]string{
		"다른 경로로 넘어감":   {`!curl -fsS -I https://api.x.com/auth/github`, `curl -fsS -i https://api.x.com/health`},
		"다른 파일로 넘어감":   {`!gh api repos/o/r/contents/src/queries/Member?ref=main`, `gh api repos/o/r/contents/src/queries/queryKey.ts?ref=main`},
		"다른 하위 명령으로 감": {`!docker compose logs api`, `docker compose ps api`},
		"같은 명령 다시":     {`!curl http://localhost:3000/a`, `curl http://localhost:3000/a`},
		"주소가 나중에 됨":    {`!curl http://localhost:3000/a`, `curl https://localhost:3000/a`, `curl http://localhost:3000/a`},
		"다른 서버":        {`!curl http://localhost:8013/api`, `curl http://localhost:3000/`},
		"되돌릴 수 없는 고침":  {`!git push origin main`, `git push --force origin main`},
		"출력 모양만 바꿈":    {`!git log --oneline | head -80`, `git log --oneline | head -100`},
		"실패한 부분만 뺌":    {`!git status --short && ls -la nope`, `git status --short`},
		"같은 실패가 또 남":   {`!pnpm dev`, `pnpm dev:engine`, `!pnpm dev`},
		"검사의 실패는 코드 탓": {`!pnpm --filter user typecheck`, `pnpm --filter user lint`},
		"둘러보기만 함":      {`!cat a.txt`, `cat b.txt`},
	} {
		if got := fixPairs(tries(steps...)); len(got) != 0 {
			t.Error(name, "을 고침으로 봄", got)
		}
	}
	edited := tries(`!pnpm dev`, `pnpm dev:engine`)
	edited[1].edits = 1
	if got := fixPairs(edited); len(got) != 0 {
		t.Fatal("파일을 고친 뒤에 된 명령을 명령의 고침으로 봄", got)
	}
	far := tries(append(append([]string{`!pnpm dev`}, strings.Split(strings.Repeat("ls,", fixWindow), ",")[:fixWindow]...), `pnpm dev:engine`)...)
	if got := fixPairs(far); len(got) != 0 {
		t.Fatal("한참 뒤의 명령을 고침으로 봄", got)
	}
}

func TestFailureTextSaysAPairOnceWithoutSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("HARNESS_HOME", filepath.Join(home, "harness"))
	t.Setenv("TMPDIR", t.TempDir())
	repo := filepath.Join(home, "work", "app")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	s := session{id: "11111111-2222-3333-4444-555555555555", cwd: repo, tries: tries(
		`!webqa shot http://front.example.com:3000/ko/home`, `webqa shot https://front.example.com:3000/ko/home`,
		`!curl -H "token: abcdef123456" http://api.local/a?x`, `curl -H "token: abcdef123456" 'http://api.local/a?x'`,
	)}
	if err := appendFacts(map[string]bool{}, sessionFacts(s), time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(factsPath())
	if strings.Contains(string(raw), "abcdef123456") {
		t.Fatal("비밀값이 장부에 남음")
	}
	text := FailureText(`webqa shot http://front.example.com:3000/ko/storyboard`, repo, "s1")
	if !strings.Contains(text, "http://front.example.com:3000 은 실패했고 https://front.example.com:3000 로 됐다 (대화 1개") {
		t.Fatal("같은 주소 실패에 예전 고침을 붙이지 않음", text)
	}
	if again := FailureText(`webqa shot http://front.example.com:3000/ko/home`, repo, "s1"); again != "" {
		t.Fatal("같은 대화에 같은 줄을 또 붙임", again)
	}
	if other := FailureText(`webqa shot https://front.example.com:3000/ko/home`, repo, "s2"); other != "" {
		t.Fatal("된 주소의 실패에 고침을 붙임", other)
	}
	if text := FailureText(`curl -H "token: zzzzzz999999" http://api.local/a?x`, repo, "s3"); !strings.Contains(text, "«curl -H \"token: [가림]") {
		t.Fatal("비슷한 명령의 고침을 가린 채로 붙이지 않음", text)
	}
	if elsewhere := FailureText(`curl -H "token: zzzzzz999999" http://api.local/a?x`, filepath.Join(home, "other"), "s4"); elsewhere != "" {
		t.Fatal("다른 레포의 명령 고침을 붙임", elsewhere)
	}
}
