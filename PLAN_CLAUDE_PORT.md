# Plan: Port `qwen-code-web` to `claude-code-web`

This document briefs an engineer (or Claude itself) to produce a parallel application that does for **Claude Code** what `qwen-code-web` does for **Qwen Code**: a single-binary Go web dashboard that runs multiple coding-agent sessions in parallel across projects, with a mobile-friendly UI.

The goal is **functional parity with a swapped agent backend**, not a rewrite. Most of the codebase ports unchanged. The only real work is the agent integration layer (process spawn, I/O bridge, event shape).

---

## 1. What `qwen-code-web` is

A self-contained Go binary that:

1. Serves a web dashboard on `:4000` listing registered projects from a workspace directory.
2. Per project, spawns a `qwen` TUI subprocess inside a PTY and bridges it to the browser over WebSocket.
3. Persists per-project session history to `~/.qwen-code-web/sessions/<name>_<hash>/events.jsonl` so a browser reconnect can replay the full conversation.
4. Provides project CRUD (add existing folder, `git init` new repo, `git clone`), a project file browser (read / raw / download / upload / delete), and a tool-call approval flow.
5. Optional bcrypt password auth + cookie tokens; strict WebSocket origin allowlist.

Source layout (all top-level Go files, single `main` package):

| File             | Purpose                                                                              |
| ---------------- | ------------------------------------------------------------------------------------ |
| `main.go`        | CLI flag parsing (`--workspace --host --port --origins --password`), settings load, startup, signal handling |
| `projects.go`    | `ProjectStore`: workspace-scoped registry in `~/.qwen-code-web/projects.json`         |
| `procmgr.go`     | `ProcManager`: one `ActiveProject` per running agent — spawn, monitor, stop          |
| `session.go`     | PTY spawn of the `qwen` binary, JSONL event/input file setup, binary resolution      |
| `tailer.go`      | Polling JSONL tailer (50 ms) — feeds `events.jsonl` lines into the WS broadcast hub  |
| `server.go`      | HTTP + WS server, all API routes, auth middleware, file browser endpoints            |
| `origin_test.go` | Origin allowlist tests                                                               |
| `public/`        | Embedded UI: `dashboard.html`, `index.html`, `app.css`, `app.js` (vanilla JS, no framework) |

---

## 2. The integration contract (the only part that changes)

`qwen-code-web` works because the underlying `qwen` binary is a fork of `gemini-cli` that has been patched to accept two non-standard flags:

```
qwen --json-file <events.jsonl> --input-file <input.jsonl> [extra args…]
```

— and then exchanges JSONL events with the parent process via those two files. The Go side just tails one and appends to the other.

### Event shape (qwen → host, appended to `events.jsonl`)

Each line is a JSON object. The shapes consumed by `public/app.js`:

```jsonc
{ "type": "system", "subtype": "session_start", "data": { "session_id": "…", "cwd": "/abs/path" } }
{ "type": "system", "subtype": "session_end" }

{ "type": "stream_event", "event": { "type": "message_start", … } }
{ "type": "stream_event", "event": { "type": "content_block_delta", "delta": { "type": "text_delta", "text": "…" } } }

{ "type": "assistant", "message": { "content": [ {"type":"text","text":"…"}, {"type":"tool_use","name":"…","input":{…}} ], "usage": { "input_tokens": N, "output_tokens": N } } }
{ "type": "user",      "message": { "content": [ {"type":"tool_result","tool_use_id":"…","content":"…"} ] } }

{ "type": "control_request",  "request_id": "…", "request":  { "tool_name": "…", "input": {…} } }
{ "type": "control_response", "response":  { "request_id": "…", "response": { "allowed": true|false } } }
```

This shape is essentially the Anthropic Messages SSE stream + tool-use blocks. **That is convenient: Claude Code emits the same shape natively** when run with streaming JSON output. Most of the front-end can be reused verbatim.

### Input shape (host → qwen, appended to `input.jsonl`)

```jsonc
{ "type": "submit", "text": "user prompt text" }
{ "type": "submit", "text": "/exit" }                          // used for graceful shutdown
{ "type": "confirmation_response", "request_id": "…", "allowed": true|false }
```

---

## 3. Mapping to Claude Code

Claude Code does not have `--json-file` / `--input-file`. It has two cleaner mechanisms:

### Option A — `claude` CLI with streaming JSON I/O (recommended)

The Claude Code CLI supports headless streaming over stdin/stdout:

```
claude -p \
  --input-format stream-json \
  --output-format stream-json \
  --permission-mode default
```

- stdin: one JSON object per line, the same `user`-turn shape as the SDK.
- stdout: one JSON object per line — `system/init`, `assistant`, `user` (tool results), and a final `result` event. The `assistant` / `user` shapes match the Messages API.
- `--permission-mode` and `--allowed-tools` / `--disallowed-tools` give tool-gating. For human-in-the-loop approval, use `--permission-prompt-tool <mcp-tool>` and route through a small local MCP server, or use `acceptEdits` / `plan` modes if the UI does not need per-tool approval at v1.

This is the closest analog to qwen's JSONL bridge: replace files with pipes.

### Option B — Claude Agent SDK (`@anthropic-ai/claude-agent-sdk` or `claude-agent-sdk` Python)

Cleaner if you are willing to introduce a Node or Python sidecar process. The SDK yields the same message stream programmatically and exposes a `canUseTool` callback for native per-tool approval — which maps 1:1 onto the existing `control_request` / `control_response` UI flow.

Trade-off: violates the "zero non-Go dependencies" property the README brags about. Prefer Option A unless you specifically need SDK-only features (custom tools, MemoryTool, hooks).

### Recommendation

**Use Option A (CLI + stream-json) for v1.** It preserves the "single Go binary, no Node required at runtime" story (Claude Code itself is the only runtime dependency, parallel to how `qwen` is today). Approvals can ship in v2 via a tiny embedded MCP permission-prompt server.

---

## 4. Concrete porting steps

Rename, replace, or rewrite as listed. The diff is small.

### 4.1 Identifier and path renames (mechanical)

| qwen-code-web                                  | claude-code-web                                  |
| ---------------------------------------------- | ------------------------------------------------ |
| `~/.qwen-code-web/`                            | `~/.claude-code-web/`                            |
| binary `qwen-code-web`                         | binary `claude-code-web`                         |
| flag `--password` setup writes `qwen-code-web` settings | same, under new dir                       |
| cookie `qwen_auth`                             | `claude_auth`                                    |
| `QwenArgs`, `GlobalQwenArgs` (in `projects.go`, `server.go`) | `ClaudeArgs`, `GlobalClaudeArgs`     |
| WS event wrapper `{ "type": "qwen_event", "data": … }` (server.go `marshalQwenEvent`) | `{ "type": "claude_event", "data": … }` |
| `app.js` label `'Qwen'`, header text          | `'Claude'`                                       |
| `dashboard.html` / `index.html` titles         | `claude-code-web · Dashboard`, etc.              |
| `[qwen/%s]` stderr prefix in `procmgr.go`     | `[claude/%s]`                                    |
| `[procmgr] Starting Qwen…` log lines           | `Starting Claude…`                               |
| `.gitignore` template line `.qwen-code-web/`   | `.claude-code-web/`                              |

These are all `s/qwen/claude/g`-class changes. Keep the project structure and Go package identical.

### 4.2 `session.go` — agent spawn (the real work)

Rewrite `resolveQwen`, `spawnQwen`, `spawnOptions`:

- `resolveClaude()` mirrors `resolveQwen()` exactly: try `zsh -l -c 'which claude'`, then `bash -l -c …`, then `exec.LookPath("claude")`, then NVM `~/.nvm/versions/node/*/bin/claude`, then `/opt/homebrew/bin/claude`, `/usr/local/bin/claude`, `~/.local/bin/claude`. The Node-shebang detection (`isNodeScript`, `resolveNode`) is still needed — `claude` ships as a Node script under npm and `npx @anthropic-ai/claude-code` installs.
- `spawnClaude(opts spawnOptions)` should build:
  ```
  claude -p --input-format stream-json --output-format stream-json --verbose [extraArgs…]
  ```
  Drop `--json-file` / `--input-file`. Keep PTY for `isatty` semantics if needed; if Claude misbehaves under PTY (it may, since it is a non-interactive headless mode), switch to plain pipes (`cmd.Stdin`, `cmd.Stdout`).
- Instead of files, wire `cmd.Stdin` and `cmd.Stdout` to pipes. Spawn two goroutines:
  - **stdout → events.jsonl**: line-buffered scanner that writes each line both to the in-memory hub *and* appends to `events.jsonl` for replay-on-reconnect. The existing tailer can be deleted, or kept as the read-side of the file (then the writer goroutine becomes the producer). Simpler: keep the file as the source of truth, write lines to it, and let the existing `Tailer` keep watching.
  - **input.jsonl → stdin**: a tailer-like reader, or just feed `appendInput` directly into stdin via a `bufio.Writer` and skip the input file. Recommended: keep the file (gives crash-safe input log), write each new line to both file and stdin.

Keeping `events.jsonl` + `input.jsonl` as on-disk logs preserves the "reload browser, see full history" property without any extra work — the persistence layer is unchanged.

### 4.3 Input message translation

`server.go` writes two input shapes to qwen. They must be translated for Claude's `stream-json` stdin:

| qwen input                                                                  | claude `stream-json` stdin                                                                           |
| --------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| `{"type":"submit","text":"…"}`                                              | `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]} }`                  |
| `{"type":"submit","text":"/exit"}`                                          | Close stdin (EOF) — Claude's stream-json mode exits when stdin closes                                |
| `{"type":"confirmation_response","request_id":"…","allowed":true}`          | v1: not needed if running with `--permission-mode acceptEdits`. v2: route via MCP permission tool.   |

Centralise this in a `func toClaudeInput(qwenStyle map[string]any) []byte` so the call sites in `handleProjectMessage` / `handleProjectApprove` / `ProcManager.Stop` keep their current shape.

### 4.4 Output event translation

Claude's `stream-json` output already emits `assistant` and `user` (tool-result) events whose `message.content` matches the front-end's expectations. Two adjustments:

- **Session start**: Claude emits `{"type":"system","subtype":"init","cwd":"…","session_id":"…", … }`. Rewrite the `subtype` to `session_start` (or update the front-end switch) and lift `cwd`/`session_id` into `data` to match the existing front-end at `app.js:654-670`. Cheapest: do the rewrite server-side in the goroutine that writes to `events.jsonl`.
- **Session end**: Claude emits a final `{"type":"result", … }`. Translate to `{"type":"system","subtype":"session_end"}` so the front-end's "session ended" notice still fires (`app.js:663-669`). Also use this to call `state.setStopped()` server-side, replacing today's `system/session_end` handling in `server.go:onProjectLiveEvent`.
- **Streaming text deltas**: Claude with `--output-format stream-json --verbose` emits raw SSE-style `stream_event` blocks that already match `app.js:672-686`. No change.

### 4.5 Approvals (defer to v2)

For v1, launch Claude with `--permission-mode acceptEdits` (or `default` with a curated `--allowed-tools` list). Skip rendering approval cards — Claude will not emit `control_request`. The UI gracefully handles their absence (the switch in `app.js:651-708` just no-ops).

For v2, implement an embedded MCP server in Go that exposes a single tool `approve_tool_use`, and launch Claude with `--permission-prompt-tool mcp__local__approve_tool_use --mcp-config <generated>`. When Claude invokes that tool, the Go side surfaces a `control_request`-shaped event on the WS hub (reusing the existing UI) and blocks the MCP response until the user clicks Allow/Deny.

### 4.6 README + docs

Update `README.md`:
- Replace "Qwen Code" with "Claude Code" everywhere.
- Replace prereq `Qwen Code installed and available as qwen` with `Claude Code installed and available as claude` (link: `npm install -g @anthropic-ai/claude-code`).
- Adjust paths (`~/.qwen-code-web` → `~/.claude-code-web`).
- Keep the security warning verbatim — Claude Code's tool-use surface is at least as dangerous as Qwen's.

Update `UI.md`: only the project name in §1 needs to change. The design system stays.

### 4.7 Tests

`origin_test.go` is agent-agnostic; rename strings only. Add one new test: `TestTranslateSubmitToClaudeStreamJSON` covering the input shape translation in §4.3.

---

## 5. What does **not** change

Do not touch:

- The whole front-end (`public/app.css`, `public/app.js`, `dashboard.html`, `index.html`) except the literal strings `Qwen`, `qwen-code-web` and the WS event wrapper key. The DOM, the event handling, the approval card flow, the file tree, the markdown rendering, the streaming spinner — all stays.
- `ProjectStore`, `Tailer`, `Hub`, auth middleware, token persistence, file-browser handlers, ZIP download, upload, delete, origin checking. Pure transport / persistence.
- The `~/.{name}-code-web/sessions/<base>_<hash>/` layout. Keeping it identical means an old `qwen-code-web` user could in principle replay history under the new binary (modulo the path rename).
- The settings file schema in `~/.{name}-code-web/settings.json` (`workspace`, `host`, `port`, `origins`, `passwordHash`). Rename `GlobalQwenArgs` → `GlobalClaudeArgs` in `projects.json` but keep everything else byte-compatible.

---

## 6. Suggested build order

1. **Fork & rename** — copy the repo, run the mechanical renames in §4.1, get it building under the new name. UI will be visibly "Claude" but back-end still spawns `qwen`. Smoke-test by leaving `qwen` installed; verify nothing else broke.
2. **Replace spawn** — rewrite `session.go` (§4.2) and the input/output translation (§4.3, §4.4). Test by starting one project; confirm session_start arrives, a `submit` round-trips to a streaming assistant response, and `events.jsonl` accumulates Claude's lines.
3. **Verify replay & reconnect** — disconnect WS mid-stream, reconnect, confirm `replay_start` → history → `replay_end` works unchanged.
4. **Lock down tools** — pick the v1 permission posture (`acceptEdits` or curated `--allowed-tools`). Document in README.
5. **(v2)** MCP-based approval bridge so the existing approval card UI lights up again.

---

## 7. Open questions for the implementer

These are decisions worth pausing on rather than guessing:

- **PTY or pipes for `claude -p`?** Qwen needs a TTY (`isatty=true`); Claude in headless `stream-json` mode probably does not, and may behave better with plain pipes. Try pipes first.
- **`--verbose` requirement.** Claude requires `--verbose` to get full streaming JSON in some modes. Confirm against the installed CLI version.
- **Per-session model selection.** Today `qwenArgs` is forwarded raw. Decide whether to expose a curated dropdown (`--model claude-sonnet-4-6` / `claude-opus-4-7`) in the UI, or leave it as a free-text args field.
- **Cross-binary coexistence.** If someone runs both `qwen-code-web` and `claude-code-web` on the same host, their session dirs must not collide. The proposed rename (`~/.claude-code-web/`) handles that. Port `4000` is the same default — document that they cannot run simultaneously without `--port`.
