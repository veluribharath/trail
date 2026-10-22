# trail

See what your coding agents did, folder by folder.

![trail showing a Claude Code session: the trace strip under the header, the transcript as a trail of steps on the left, and the diffs it made on the right](docs/screenshot.png)

trail finds the sessions that Claude Code, Codex, OpenCode, Gemini CLI and Kiro CLI leave on your machine, files each session under the project it ran in, and shows the transcript next to the changes it made. Click an edit in the transcript to jump to its diff; click a diff to jump back to the step that made it. A Commits tab lists what landed in git while the session ran, with the commits that touch the agent's files first. Any session can be [exported](#export-a-session) as one HTML file to share, and [Stats](#stats) show how your agents work across every project.

Each session opens with a trace: one tick per step, prompts tall, edits amber, failures red. Drag across it to move through the transcript. Press ⌘K (Ctrl+K) to search: it matches session names, and everything said and run inside every transcript. Pick a match to open the session at that step, with every other matching step marked on the trace; `n` and `Shift+N` step through them. `/` filters the sidebar by name, and `j`/`k` step between sessions.

It’s a single static Go binary with an embedded browser UI (fonts included, nothing loads from the network). It only reads agent storage, never writes to it, writes an export only where you save it, and makes no network calls unless you turn on [related sessions](#related-sessions-opt-in), which talk to an embedding model on your machine.

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
./trail -prices    # print the token prices used for costs, and exit
./trail -export c1 -o session.html   # write one session as an offline page, and exit
./trail -stats 30   # print the last 30 days in numbers (or -stats all), and exit
./trail -mcp       # an MCP server on stdio, so your agent can search past sessions
```

On a remote dev box, `./trail -host 0.0.0.0` and open the port from your laptop. Transcripts often contain secrets, so only do this on a network you trust (or tunnel the port over SSH instead).

| Flag | Default |
| --- | --- |
| `-host` | `127.0.0.1` |
| `-port` | `7878` |
| `-claude-dir` | `$CLAUDE_CONFIG_DIR` or `~/.claude` |
| `-codex-dir` | `$CODEX_HOME` or `~/.codex` |
| `-opencode-dir` | `$XDG_DATA_HOME/opencode` or `~/.local/share/opencode` |
| `-gemini-dir` | `$GEMINI_CLI_HOME/.gemini` or `~/.gemini` |
| `-kiro-dir` | `~/.kiro` |
| `-kiro-data-dir` | `~/.local/share/kiro-cli` (`$XDG_DATA_HOME/kiro-cli`); `~/Library/Application Support/kiro-cli` on macOS |
| `-semantic` | off; see [Related sessions](#related-sessions-opt-in) |
| `-embed-url` | `http://127.0.0.1:11434` (Ollama) |
| `-embed-model` | `nomic-embed-text` |
| `-prices` | print the price table and exit |
| `-export`, `-o`, `-redact` | off; see [Export a session](#export-a-session) |
| `-stats` | off; a number of days, or `all`; see [Stats](#stats) |
| `-mcp` | off; see [Let your agent search past sessions](#let-your-agent-search-past-sessions-mcp) |

## What it reads

| Agent | Location | Edits shown as diffs |
| --- | --- | --- |
| Claude Code | `~/.claude/projects/<folder>/<id>.jsonl` | `Edit`, `MultiEdit`, `Write` |
| Codex | `~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl` (current and legacy formats) | `apply_patch`, direct or through the shell tool |
| OpenCode | `~/.local/share/opencode/opencode.db` (OpenCode 1.2 and later) | `edit`, `write`, `apply_patch`; subagent sessions are shown inside their parent |
| Gemini CLI | `~/.gemini/tmp/<project>/chats/session-*.jsonl` (and older `.json` sessions) | `replace`, `write_file` (a real before/after, since Gemini CLI records the old file); subagent runs aren't shown yet |
| Kiro CLI | `~/.kiro/sessions/cli/<id>.jsonl` (CLI 2.x), `~/.kiro/sessions/<workspace>/sess_<id>/` (V3 and the Kiro IDE), and headless and 1.x conversations in `kiro-cli/data.sqlite3` | `write`/`fs_write` (create, str_replace, insert, append), `str_replace`, `fs_append`, `delete_file`; subagent sessions are shown inside their parent |

OpenCode keeps every session in one SQLite database, and so does Kiro CLI for headless runs (`kiro-cli chat --no-interactive`) and for 1.x. trail reads them with a small built-in reader instead of a SQLite library: it opens the database and its `-wal` file read-only, takes no locks, and creates no files next to them, so it can't get in the way of a running agent. A session that is being written shows up as of the last committed step.

A session the agent wrote to in the last 10 minutes opens live: new steps appear as the agent works, marked Live in the header. Following a session can't interrupt it. Claude Code, Codex and Gemini CLI append whole lines to the session file, so trail checks the file's size about once a second and, when it grows, reads the complete lines and closes the file again. It never holds the file open or touches the agents' lock files. OpenCode and Kiro CLI sessions follow the same way, by checking the database or session files about once a second; they update as each step is written.

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

## Tokens and cost

Every agent records how many tokens each model response used, and trail adds them up: per session in the sidebar, per folder, in the session header (input, cache hits, cache writes, output and thinking), and per step in the transcript. Hover any figure for the full breakdown.

The dollar figures are **API-equivalent**: what the same tokens cost at the provider's API list prices. On a Claude or ChatGPT subscription you pay a flat fee instead, and on Bedrock or Vertex prices differ. Click *Show tokens* in a session header to show token counts everywhere instead; trail remembers the choice.

Each response is priced on its own, at its own model's rates, because cache pricing varies by model and some models charge more for long prompts (Claude Haiku 5.5 past 100K prompt tokens, GPT-5.5 past 272K, Gemini Pro past 200K). Claude's 5-minute and 1-hour cache writes, fast mode, US-only inference and web searches are priced as well. Usage in Claude Code subagent transcripts counts toward their session.

Prices ship inside trail (`prices.json`, dated) and trail never fetches them. `./trail -prices` prints the table. To add a model or change a rate, put a `prices.json` in trail's config directory (`~/Library/Application Support/trail/` on macOS, `~/.config/trail/` on Linux) in the same shape; its entries replace the bundled ones for the models they list:

```json
{"prices": [
  {"models": ["my-model"], "input": 1.0, "cacheRead": 0.1, "cacheWrite": 1.25, "output": 4.0}
]}
```

Rates are USD per million tokens. A model with no price shows its tokens with the cost as "—", and a total that includes one reads "≥". OpenCode records its own cost for each step, which trail uses for models it has no price for.

## Export a session

Export saves one session as a single HTML file: the transcript, the trace, every diff, and the commits that touch files the session edited, with their patches. It opens from disk in any browser, with no trail running, and looks like trail without the sidebar. Press `/` to find text in it, and `n` and `Shift+N` to step through the matches.

In the browser, open a session and choose Export, then Download HTML; your browser asks where to save it. From the command line:

```
./trail -export c1 -o session.html     # a session id, provider:id, or the start of an id
./trail -export c1 > session.html
```

`trail -list` prints session ids. trail writes the file you name and nothing else; it's created readable only by you.

Transcripts can contain secrets: API keys, tokens, environment variables, file contents and command output. Add `-redact` to replace the ones trail recognises with `[redacted]` in the transcript, commit messages and patches (cloud provider keys, GitHub, GitLab, Slack, Stripe and model-provider tokens, JWTs, private keys, passwords in URLs, and values assigned to names like `password`, `secret` or `api_key`). It's pattern matching, so read the file before you share it either way. The file itself can't make any network request: its content security policy blocks everything but its own inlined script, styles and fonts, so opening it never loads or sends anything. Commits made in the session's window that don't touch its files are left out.

## Let your agent search past sessions (MCP)

`trail -mcp` is a [Model Context Protocol](https://modelcontextprotocol.io) server on standard input and output. Register it once and your agent can look through every past session on this machine: how a bug was fixed before, why a file changed, what was tried and failed.

```
claude mcp add trail -- trail -mcp          # Claude Code
codex mcp add trail -- trail -mcp           # Codex
gemini mcp add trail trail -mcp             # Gemini CLI
```

Other clients take the same command in their MCP config: `{"command": "trail", "args": ["-mcp"]}`. Add the same `-claude-dir`-style flags you run trail with if your agent folders aren't in the default places, and `-semantic` to blend in meaning search.

It offers three read-only tools:

| Tool | What it does |
| --- | --- |
| `search_sessions` | Full-text search with the same syntax as the UI (`"phrases"`, `in:`, `tool:`, `file:`, `agent:`), optionally limited to a project (`"."` is the folder the agent runs in). Returns sessions with their best matching steps. |
| `list_sessions` | Recent sessions, optionally for one project or agent. |
| `get_session` | One session's transcript, a page of steps at a time, with long text shortened unless asked. |

Nothing runs in the background. The agent starts `trail -mcp` with each session, and trail reads nothing until the first tool call. Then, if a trail window is open on this machine over the same agent folders, trail asks it, so nothing is rebuilt; otherwise it builds its own index, which takes a few seconds on a large history. A window writes its address to `ui.json` in trail's cache directory while it runs, and only a window on 127.0.0.1 is used.

Whatever the agent reads goes to its model provider, so every result is redacted the same way as `-export -redact`. Redaction is pattern matching: it catches the usual shapes of keys and tokens, not every secret a transcript can hold.

## Stats

The chart button at the top of the sidebar (or the link on the home page) opens Stats for the last 7, 30 or 90 days, a year, or all time, across every project or just one. Agents you turn off in the sidebar are left out.

- **Sessions per day, week or month**, stacked by project: your five busiest projects in the range by name, the rest as Other.
- **API-equivalent cost per day, week or month**, stacked the same way, when sessions recorded token counts. Show tokens switches it, the tiles and the tables to token counts (the same switch as in a session).
- **Failed tool calls** over the same buckets: the share of tool calls that returned an error.
- **Most edited files**: edits recorded in transcripts, and how many sessions made them. A session in a linked worktree counts under the main repository's paths.
- **Failure rate by tool**, for the twelve most used tools. Tool names are each agent's own (Claude Code's `Bash`, Codex's `shell`), so they are listed per agent. Rates from fewer than 20 calls are drawn faded.
- **Projects** and **Agents** tables with sessions, edits, failure rates and cost. Click a project to narrow everything to it.

Every chart has a Table button for the numbers behind it. A session counts on the day it started, in your local time. Tool calls and edited files come from reading whole transcripts, which trail does in the background for search, so for the first seconds after it starts the page says how many it has read and fills in as it goes. `trail -stats 30` prints the same numbers in the terminal.

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
gemini.go    Gemini CLI adapter
kiro.go      Kiro CLI adapter (JSONL sessions and data.sqlite3)
sqlite.go    read-only SQLite reader (tables, indexes, WAL)
index.go     discovery, mtime cache, repo-root grouping
search.go    full-text index and queries
stats.go     sessions, files and failures over time
semantic.go  opt-in related-session search over a local embedding model
git.go       commits in the session window
server.go    JSON API and embedded UI
export.go    one session as a self-contained HTML file
redact.go    secret redaction for exports and MCP results
mcp.go       trail -mcp: the MCP server and its tools
live.go      live updates for a session that is still being written
usage.go     token counts per response and session
prices.go    pricing, with the bundled prices.json and overrides
web/         index.html, style.css, app.js (no build step)
```

Adding an agent means implementing `Provider` (four methods) and registering it in `main.go`. An agent that keeps sessions in a database rather than one file each also implements `Stamp`, so the index can tell which sessions changed.

## Not yet

- pi
- Renaming sessions from trail (names are read from the agents, never written)

## Contributing

Issues and pull requests are welcome. See [CONTRIBUTING.md](CONTRIBUTING.md) for setup and the few ground rules (trail stays read-only, and test fixtures must never be real transcripts). To report a security problem privately, see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). The embedded fonts, Instrument Sans and IBM Plex Mono, are under the SIL Open Font License; their licenses are in `web/fonts/`.
