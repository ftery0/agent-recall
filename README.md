# agent-recall

[한국어](README.ko.md)

Session memory for AI coding agents. It keeps your Claude Code and Codex conversations, turns them into dated records, and hands the right part back when a new conversation starts or a shell command fails — without calling a model.

> **Source-available, non-commercial.** Free to use, study, change and share for noncommercial purposes under the [PolyForm Noncommercial License 1.0.0](LICENSE). Commercial use is not permitted.

## What it does

- **Archive** — copies each conversation transcript (`.jsonl`) to `~/.local/share/harness/raw/`, so the source survives the client's own cleanup.
- **Dated records** — writes one Markdown record per conversation: requests with their time, the agent's answers (marked as claims), the last stopping point, changed files, checks, commits, and the memory lookups the agent ran.
- **Start card** — when a conversation starts, shows the latest requests, related records, recent work notes and git state (within 3,000 characters). It warns when a note's pull request was merged after the note was saved, or when files named in a project memory were committed after it.
- **Fact ledger** — keeps what tools showed (commits, pushes, pull requests, copies between repositories, answers to question prompts, and commands that got past a failure) in an append-only `recall/facts.jsonl`. Commits are confirmed from git's own history, so quiet ones (`-q`, `-F`, scripts) are kept. When a fact comes back, git is asked again whether it still holds.
- **Failure note** — right after a shell command fails, if an earlier conversation got past the same failure, it adds that one fact ("http://…:3000 failed and https://…:3000 worked (N conversations, last MM-DD)"). Otherwise it adds nothing and never blocks. A past fix is a later command in the same conversation, with no file edited in between, that runs the same program on the same targets with changed flags, quoting, program path or address host, or asks the same server for the same resource with another scheme or port. Investigation, check and irreversible commands are never used, secrets are masked, and a line is said once per conversation.
- **Action note** — right before a shell command commits, pushes, merges or changes GitHub, it adds what memory holds about the repositories involved: whether one has a remote, where its work was copied, and the last push with the request that preceded it. Ordinary repositories and unrelated commands get nothing.
- **Misses** — counts memory lookups that came back empty or failed (`harness recall misses`).

Messages and keyword matching are currently in Korean.

## Build

Requires Go 1.26+. Tested on macOS.

```sh
go build -o ~/.local/bin/harness .
```

The start card tells the agent to run `harness recall …`, so keep the binary name `harness` and put it on your `PATH`.

## Connect the hooks

Claude Code (`~/.claude/settings.json`):

```json
{
  "hooks": {
    "SessionStart": [{ "matcher": "startup|resume|clear|compact", "hooks": [{ "type": "command", "command": "harness worklog claude start", "timeout": 10 }] }],
    "PreToolUse": [{ "matcher": "Bash", "hooks": [{ "type": "command", "command": "harness worklog claude action", "timeout": 10 }] }],
    "PostToolUseFailure": [{ "matcher": "Bash", "hooks": [{ "type": "command", "command": "harness worklog claude failure", "timeout": 10 }] }],
    "Stop": [{ "hooks": [{ "type": "command", "command": "harness worklog claude stop", "timeout": 10 }] }],
    "PreCompact": [{ "hooks": [{ "type": "command", "command": "harness worklog claude precompact", "timeout": 10 }] }]
  }
}
```

Codex (`~/.codex/hooks.json`): `harness worklog codex start` on `SessionStart`, `harness worklog codex stop` on `Stop`, and `harness edit-scope codex` on both `PreToolUse` and `PostToolUse` with the matcher `Bash`. Codex has no failure event and gives hooks the command's output without its exit code, so that one hook adds the action note before a command and, after it, the failure note when the output carries an error a program prints when it fails (`curl: (7)`, `command not found`, `fatal:`, a traceback). Codex asks you to trust new hooks once.

Use an absolute path in `command` if the hook environment does not have `~/.local/bin` on its `PATH`.

## Commands

```sh
harness recall find 'words'                 # search records and work notes
harness recall day 2026-10-15 'words'       # what was asked on one day (YYYY-MM-DD, MM-DD, 오늘, 어제)
harness recall source <session-id> 'words'  # read the original transcript around the words
harness recall facts 'words'                # facts from the ledger, checked against git now
harness recall misses --days 7              # memory lookups that came back empty or failed
harness recall build --since 2026-10-01     # rebuild records from the archive
harness recall verify                       # check the archive against the client transcripts
```

## Settings

| Variable | Default | Meaning |
| --- | --- | --- |
| `HARNESS_HOME` | `~/.local/share/harness` | archive, records and state |
| `HARNESS_VAULT_DIR` | `~/Documents/Obsidian` | notes vault read for work notes |
| `HARNESS_DAILY_DIR` | `<vault>/company/00_기록/데일리` | daily notes |
| `HARNESS_COMPANY_ORGS` | (none) | comma-separated git organizations treated as company repositories |

Work notes are read from `<vault>/company/30_자료/작업 기록/` for company repositories and `<vault>/personal/10_프로젝트/<project>/작업 기록/` for the rest. A note with `status: 진행`, `repos: <repo>` in its front matter and a `## 다음` list shows up on the start card. Without a vault the card still shows records and git state.

Claude memories are looked up in `~/.claude/projects/*/memory` and `~/.claude-profiles/*/projects/*/memory`.

## Privacy

Everything stays on your machine. Records are built without a model. Transcripts and records can hold secrets you typed; do not commit `HARNESS_HOME`.

## Status

Personal project, shared so others can fix and extend it. Expect rough edges: Korean-only messages, macOS-first, and two edit hooks (`edit-scope`, `structure-check`) kept from earlier versions. The old SQLite memory database was removed.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[PolyForm Noncommercial License 1.0.0](LICENSE)

Required Notice: Copyright (c) 2026 ftery0 (https://github.com/ftery0/agent-recall)
