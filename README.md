# trail

See what your coding agents did, folder by folder.

![trail showing a Claude Code session: the trace strip under the header, the transcript as a trail of steps on the left, and the diffs it made on the right](docs/screenshot.png)

trail finds the sessions that Claude Code, Codex and OpenCode leave on your machine, files each session under the project it ran in, and shows the transcript next to the changes it made. Click an edit in the transcript to jump to its diff; click a diff to jump back to the step that made it. A Commits tab lists what landed in git while the session ran, with the commits that touch the agent's files first.

Each session opens with a trace: one tick per step, prompts tall, edits amber, failures red. Drag across it to move through the transcript. Press ⌘K (Ctrl+K) to search: it matches session names, and everything said and run inside every transcript. Pick a match to open the session at that step, with every other matching step marked on the trace; `n` and `Shift+N` step through them. `/` filters the sidebar by name, and `j`/`k` step between sessions.

It’s a single static Go binary with an embedded browser UI (fonts included, nothing loads from the network). It only reads agent storage, never writes to it, and makes no network calls unless you turn on [related sessions](#related-sessions-opt-in), which talk to an embedding model on your machine.

## Install

With [Homebrew](https://brew.sh) on macOS:

```
brew install veluribharath/tap/trail
trail -open
```

Or download the archive for your machine from [Releases](https://github.com/veluribharath/trail/releases/latest), unpack it, and run `./trail`. Builds are published for macOS (Apple Silicon and Intel) and Linux (x86_64 and ARM64).

```
tar -xzf trail_*_darwin_arm64.tar.gz
xattr -d com.apple.quarantine ./trail   # macOS only: the binary isn't signed
./trail -open
```

With Go 1.24 or newer:

```
go install github.com/veluribharath/trail@latest
```

From source:

```
git clone https://github.com/veluribharath/trail && cd trail
make build
```

## Run it

```
./trail            # http://127.0.0.1:7878
./trail -open      # and open the browser
./trail -list      # print sessions by folder and exit
./trail -search "flaky login"   # print matching sessions and steps, and exit
```

On a remote dev box, `./trail -host 0.0.0.0` and open the port from your laptop. Transcripts often contain secrets, so only do this on a network you trust (or tunnel the port over SSH instead).

| Flag | Default |
| --- | --- |
| `-host` | `127.0.0.1` |
| `-port` | `7878` |
| `-claude-dir` | `$CLAUDE_CONFIG_DIR` or `~/.claude` |
| `-codex-dir` | `$CODEX_HOME` or `~/.codex` |
| `-opencode-dir` | `$XDG_DATA_HOME/opencode` or `~/.local/share/opencode` |
| `-semantic` | off; see [Related sessions](#related-sessions-opt-in) |
| `-embed-url` | `http://127.0.0.1:11434` (Ollama) |
| `-embed-model` | `nomic-embed-text` |

## What it reads

| Agent | Location | Edits shown as diffs |
| --- | --- | --- |
| Claude Code | `~/.claude/projects/<folder>/<id>.jsonl` | `Edit`, `MultiEdit`, `Write` |
| Codex | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` (current and legacy formats) | `apply_patch`, direct or through the shell tool |
| OpenCode | `~/.local/share/opencode/opencode.db` (OpenCode 1.2 and later) | `edit`, `write`, `apply_patch`; subagent sessions are shown inside their parent |

OpenCode keeps every session in one SQLite database. trail reads it with a small built-in reader instead of a SQLite library: it opens the database and its `-wal` file read-only, takes no locks, and creates no files next to them, so it can't get in the way of a running OpenCode. A session that is being written shows up as of the last committed step.

A session the agent wrote to in the last 10 minutes opens live: new steps appear as the agent works, marked Live in the header. Following a session can't interrupt it. Both agents only append whole lines to the session file, so trail checks the file's size about once a second and, when it grows, reads the complete lines and closes the file again. It never holds the file open or touches the agents' lock files.

Sessions are filed under their git repository. A session that ran in a linked worktree is filed under the main repository and labelled with the worktree. Folders that aren't repositories are filed as themselves.

## Search

Search covers prompts, replies, tool calls (commands, patterns, paths, edit contents) and the output of steps that failed, which is where error messages are. Output of steps that succeeded (mostly file contents and logs) isn't searched. Every word has to appear in the same step; the last word also matches as a prefix while you type. Identifiers are split, so `session` finds `handleSession` and `name` finds `user_name`.

| Write | To find |
| --- | --- |
| `"exact phrase"` | the words in that order |
| `in:prompt`, `in:reply`, `in:tool`, `in:output`, `in:thinking`, `in:all` | only those parts of a step (thinking is left out unless asked for) |
| `tool:bash` | only calls to that tool |
| `file:cart.go` | only steps that edited a matching file |
| `agent:codex` | only one agent's sessions |

Add `mode:words` to a query to turn off related sessions (below) for it.

The index lives in memory and is built in the background when trail starts, re-reading only sessions that changed. Nothing is written to disk. On a generated 450 MB set of transcripts it took about 5 seconds to build, used about 160 MB, and answered in about 100 ms.

### Related sessions (opt-in)

With `-semantic`, search also finds turns that mean the same thing without sharing words, so "where did we speed up page loads" finds a session about caching. Matches found this way are labelled Related and ranked together with word matches.

It needs an embedding model running on your machine. With [Ollama](https://ollama.com):

```
ollama pull nomic-embed-text
./trail -semantic
```

Any server with an OpenAI-compatible embeddings API works too (llama.cpp, LM Studio, vLLM): `./trail -semantic -embed-url http://127.0.0.1:8080/v1 -embed-model <name>`.

What changes when you turn it on:

- trail sends each turn's text (prompt, replies, one-line tool summaries) to the embedding server, `http://127.0.0.1:11434` unless you say otherwise. If you point `-embed-url` at another machine, transcript text goes there; trail warns you at startup.
- trail writes the vectors to its own cache (`~/Library/Caches/trail/vectors` on macOS, `~/.cache/trail/vectors` on Linux), readable only by you, so a restart only embeds new or changed sessions. It never writes to agent storage. `./trail -forget` deletes the cache.
- The first run embeds every session, newest first, and can take a while on a large history; search works from the start and picks up related sessions as they are ready.

Queries that use `in:`, `tool:` or `file:` stay word searches, since the vectors don't know about fields, tools or files.

## How diffs and commits are matched

The Edits tab comes straight from the transcript: each change is the before/after text the agent sent to its edit tool, so it is exact for that step. A Claude Code `Write` replaces the whole file, and the transcript doesn't say what was there before, so every line shows as added.

The Commits tab runs `git log --all` in the session's folder for a window from 10 minutes before the session started to 2 hours after it ended. A commit "touches" the session when it changes a file the transcript edited. This is a heuristic: a commit in the window by someone else that touches the same file will also match.

## Layout

```
main.go      flags, startup, -list
model.go     Session, Transcript, Event, FileChange
claude.go    Claude Code adapter
codex.go     Codex adapter and apply_patch parser
opencode.go  OpenCode adapter
sqlite.go    read-only SQLite reader (tables, indexes, WAL)
index.go     discovery, mtime cache, repo-root grouping
search.go    full-text index and queries
semantic.go  opt-in related-session search over a local embedding model
git.go       commits in the session window
server.go    JSON API and embedded UI
live.go      live updates for a session that is still being written
web/         index.html, style.css, app.js (no build step)
```

Adding an agent means implementing `Provider` (four methods) and registering it in `main.go`. An agent that keeps sessions in a database rather than one file each also implements `Stamp`, so the index can tell which sessions changed.

## Not yet

- Gemini CLI, Kiro, pi
- Renaming sessions from trail (names are read from the agents, never written)

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and the few ground rules (trail stays read-only, and test fixtures must never be real transcripts). To report a security problem privately, see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). The embedded fonts, Instrument Sans and IBM Plex Mono, are under the SIL Open Font License; their licenses are in `web/fonts/`.
