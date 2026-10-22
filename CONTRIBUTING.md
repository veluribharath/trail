# Contributing to trail

Thanks for helping. trail is small on purpose: one Go binary, no runtime dependencies, a UI with no build step.

## Set up

You need Go 1.24 or newer.

```
git clone https://github.com/veluribharath/trail
cd trail
make test      # go vet + go test
make fuzz      # each fuzz test for a minute (FUZZTIME=10m for longer)
make run       # build and open the UI on your own sessions
```

The UI lives in `web/` as plain `index.html`, `style.css` and `app.js`. Edit them and rebuild; they are embedded into the binary at build time.

## Ground rules

- **Read-only.** trail never writes to an agent's storage. Keep it that way. What trail writes of its own (the vector cache, your notes) goes under its own directories, readable only by you.
- **Never commit real transcripts.** Session logs contain prompts, file contents, command output and often secrets. Fixtures in `testdata/` must be written by hand or generated, never copied from your own `~/.claude`, `~/.codex`, `~/.gemini`, `~/.kiro` or OpenCode and Kiro databases.
- **Nothing off the machine.** The page loads no external scripts, fonts or images, and trail makes no network calls besides running `git` locally. The one exception is opt-in: `-semantic` sends turn text to the embedding server the person points it at (localhost by default) and caches vectors in trail's own cache directory. Keep anything like it behind a flag.
- **No new dependencies** without a good reason. The standard library has covered everything so far.

## Adding an agent

Implement the `Provider` interface in `model.go` (`Name`, `Root`, `Discover`, `Parse`) in a new file, register it in `main.go`, and add fixtures plus a test like `TestCodexParse`. If the agent keeps session names outside its session files, also implement `SessionNames`. If it keeps sessions in a database rather than one file each, return one path per session from `Discover` and implement `Stamp` (see `opencode.go`); the OpenCode fixture is built by `testdata/opencode/gen.py`.

Agents people have asked for are listed under "Not yet" in the README. When a format changes upstream, a short note in the PR about which version wrote the new format helps a lot.

## Pull requests

- Run `make test` before opening one. CI runs the same checks on Linux and macOS, plus a short `make fuzz`.
- `make test` includes a UI smoke test that drives the page in headless Chrome. It skips when it can't find Chrome or Chromium; point `TRAIL_CHROME` at one to run it. If you touch the SQLite reader or a parser, run `make fuzz` for a while too.
- For UI changes, include a screenshot in both light and dark mode.
- Keep commits focused, with messages that say why the change was made.
