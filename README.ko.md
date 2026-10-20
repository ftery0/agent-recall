# agent-recall

[English](README.md)

AI 코딩 도구(Claude Code·Codex)의 대화를 보관하고, 새 대화를 시작하거나 셸 명령이 실패할 때 필요한 부분을 다시 건네주는 도구입니다. 모델을 부르지 않습니다.

> **소스 공개·비상업.** [PolyForm Noncommercial License 1.0.0](LICENSE)에 따라 비상업 목적이면 누구나 쓰고, 보고, 고치고, 나눌 수 있습니다. 상업적 사용은 허용하지 않습니다.

## 하는 일

- **보관** — 대화 원본(`.jsonl`)을 `~/.local/share/harness/raw/` 에 복사합니다. 도구가 원본을 지워도 남습니다.
- **날짜별 기록** — 대화마다 Markdown 기록을 만듭니다. 요청과 시각, AI 답(주장으로 표시), 끝난 지점, 바뀐 파일, 검사, 커밋, AI가 부른 기억 명령이 들어갑니다.
- **시작 안내** — 대화를 시작하면 최근 요청·관련 기록·작업 노트·git 상태를 3,000자 안으로 보여 줍니다. 노트에 적힌 PR이 노트 저장 뒤 머지됐거나, 프로젝트 기억 메모에 적힌 파일이 메모 저장 뒤 커밋됐으면 ⚠ 로 알립니다.
- **사실 장부** — 도구 출력에 찍힌 커밋·push·PR·레포 사이 복사·선택창 답과 실패를 넘긴 명령을 덧붙이기만 하는 `recall/facts.jsonl` 에 모읍니다. 커밋은 git 기록으로 확인해서 출력이 조용한 커밋(`-q`·`-F`·스크립트)도 남습니다. 꺼낼 때 지금도 맞는지 git 에 다시 묻습니다.
- **실패한 순간의 메모** — 셸 명령이 실패한 직후, 예전 대화에서 같은 실패를 넘긴 적이 있으면 그 사실 한 줄("http://…:3000 은 실패했고 https://…:3000 로 됐다 (대화 N개, 마지막 MM-DD)")을 붙입니다. 없으면 아무것도 붙이지 않고 막지도 않습니다. 넘긴 명령은 같은 대화에서 파일을 고치지 않고 뒤이어 된 명령 가운데, 같은 프로그램과 같은 대상에 플래그·따옴표·프로그램 경로·주소의 host 만 바꾼 것이거나 같은 서버의 같은 자원을 다른 scheme·port 로 물은 것입니다. 조사·검사·되돌릴 수 없는 명령은 쓰지 않고, 비밀값은 가리며, 같은 줄은 한 대화에 한 번만 붙입니다.
- **행동 직전 메모** — 셸 명령이 커밋·push·머지·GitHub 변경을 하기 직전에, 닿는 레포의 원격 유무·복사해 간 레포·마지막 push 와 그 직전 사용자 요청을 붙입니다. 특별할 것 없는 레포와 관계없는 명령에는 붙이지 않습니다.
- **못 찾은 조회 모으기** — AI의 기억 명령이 빈 결과나 오류로 끝난 것을 셉니다(`harness recall misses`).

## 빌드

Go 1.26 이상이 필요합니다. macOS 에서 확인했습니다.

```sh
go build -o ~/.local/bin/harness .
```

시작 안내가 AI에게 `harness recall …` 을 쓰라고 알려 주므로, 실행 파일 이름은 `harness` 로 두고 `PATH` 에 넣습니다.

## 훅 연결

훅 설정 예시는 [README.md](README.md#connect-the-hooks)를 봅니다. Claude 는 `harness worklog claude start|stop|precompact`, `PreToolUse`(matcher `Bash`)의 `harness worklog claude action`, `PostToolUseFailure`(matcher `Bash`)의 `harness worklog claude failure` 를 등록합니다. Codex 는 `harness worklog codex start|stop` 과 `PreToolUse`·`PostToolUse`(matcher `Bash`)의 `harness edit-scope codex` 를 등록합니다. Codex 에는 실패 이벤트가 없고 훅에 종료 코드 없이 출력만 주므로, 이 훅 하나가 명령 앞에는 행동 메모를, 뒤에는 출력에 실패 문구(`curl: (7)`·`command not found`·`fatal:`·traceback)가 있을 때 실패 메모를 붙입니다. Codex 는 새 훅을 처음 볼 때 신뢰를 한 번 골라야 합니다.

## 명령

```sh
harness recall find '찾을 말'
harness recall day 어제 '찾을 말'
harness recall source <세션 ID> '확인할 말'
harness recall facts '찾을 말'
harness recall misses --days 7
harness recall build --since 2026-10-01
harness recall verify
```

## 설정

| 변수 | 기본값 | 뜻 |
| --- | --- | --- |
| `HARNESS_HOME` | `~/.local/share/harness` | 원본·기록·상태 |
| `HARNESS_VAULT_DIR` | `~/Documents/Obsidian` | 작업 노트를 읽을 노트 폴더 |
| `HARNESS_DAILY_DIR` | `<vault>/company/00_기록/데일리` | 데일리 노트 |
| `HARNESS_COMPANY_ORGS` | (없음) | 회사 레포로 볼 git 조직 이름, 쉼표로 구분 |

작업 노트는 회사 레포면 `<vault>/company/30_자료/작업 기록/`, 그 밖에는 `<vault>/personal/10_프로젝트/<프로젝트>/작업 기록/` 에서 읽습니다. 머리말에 `status: 진행`·`repos: <레포>` 가 있고 `## 다음` 목록이 있는 노트가 시작 안내에 나옵니다. 노트 폴더가 없어도 기록과 git 상태는 나옵니다.

## 개인정보

모든 자료는 내 컴퓨터에만 남고, 기록은 모델 없이 만듭니다. 원본과 기록에는 직접 입력한 비밀값이 들어 있을 수 있으니 `HARNESS_HOME` 을 커밋하지 마세요.

## 상태

개인 프로젝트를 다른 사람도 고치고 넓힐 수 있게 공개했습니다. 안내 문구가 한국어뿐이고, macOS 위주이며, 예전 판의 편집 훅(`edit-scope`·`structure-check`)이 남아 있습니다. 옛 SQLite 기억 DB는 지웠습니다.

## 라이선스

[PolyForm Noncommercial License 1.0.0](LICENSE)

Required Notice: Copyright (c) 2026 ftery0 (https://github.com/ftery0/agent-recall)
