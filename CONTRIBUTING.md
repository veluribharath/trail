# Contributing to trail

Thanks for helping. trail is small on purpose: one Go binary, no runtime dependencies, a UI with no build step.

## Set up

You need Go 1.24 or newer.

```
git clone https://github.com/veluribharath/trail
cd trail
make test      # go vet + go test
make run       # build and open the UI on your own sessions
```

The UI lives in `web/` as plain `index.html`, `style.css` and `app.js`. Edit them and rebuild; they are embedded into the binary at build time.

## Ground rules

- **Read-only.** trail never writes to an agent's storage. Keep it that way.
- **Never commit real transcripts.** Session logs contain prompts, file contents, command output and often secrets. Fixtures in `testdata/` must be written by hand or generated, never copied from your own `~/.claude` or `~/.codex`.
- **Nothing off the machine.** The page loads no external scripts, fonts or images, and trail makes no network calls besides running `git` locally.
- **No new dependencies** without a good reason. The standard library has covered everything so far.

## Adding an agent

Implement the `Provider` interface in `model.go` (`Name`, `Root`, `Discover`, `Parse`) in a new file, register it in `main.go`, and add fixtures plus a test like `TestCodexParse`. If the agent keeps session names outside its session files, also implement `SessionNames`.

Agents people have asked for are listed under "Not yet" in the README. When a format changes upstream, a short note in the PR about which version wrote the new format helps a lot.

## Pull requests

- Run `make test` before opening one. CI runs the same checks on Linux and macOS.
- For UI changes, include a screenshot in both light and dark mode.
- Keep commits focused, with messages that say why the change was made.
