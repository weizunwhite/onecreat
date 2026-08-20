# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

OneCreat is a DeepSeek-native AI coding agent (a Codex/Codex-style harness) written in Go. Active development is on the `main-v2` branch; the `v1` branch is the legacy TypeScript build (maintenance only). This is the user's own fork/deployment ("onecreat"), tuned DeepSeek-first.

> **本地开发工作流见 [`docs/开发工作流.md`](docs/开发工作流.md)**(构建/打包/装机、账号网关本地实测、改完自查三连、红线教训)。
> 本文件夹 `/Users/localwork/06_System/onecreat` 是 onecreat 的**唯一开发仓库**(2026-06-18 从 reasonix 物理分离,origin=weizunwhite/onecreat,无 upstream);旧文件夹 `reasonix_source/DeepSeek-Reasonix` 是分离前副本,**别在那开发**。
> ⚠️ **不要把 reasonix 上游的流式渲染优化往这搬**——已实测更差,详见 `docs/开发工作流.md` 红线段。

## Two Go modules

This repo has **two separate Go modules** — `go build ./...` at the root does **not** include the desktop:

- **root** (`module reasonix`) — the kernel, CLI, and command binaries. Pure Go, `CGO_ENABLED=0`, single static binary.
- **`desktop/`** (`module reasonix/desktop`) — the Wails GUI (CGO/WebKit). Nested on purpose so the kernel build never pulls in WebKit. Build/test it from inside `desktop/`.

## Commands

```sh
# Kernel (root module)
make build              # -> bin/reasonix, bin/reasonix-hardware-mcp, bin/reasonix-plugin-example
make test               # go test ./...
make vet                # go vet ./...   (also runs as a pre-push hook via `make hooks`)
make fmt                # gofmt -w .
go test ./internal/control/ -run TestName -v       # single package / single test
go test -race ./internal/agent/ ./internal/control/ # concurrency-sensitive packages
make cross              # cross-compile to dist/ (darwin|linux|windows × amd64|arm64)
make e2e-codegraph      # gated CodeGraph MCP end-to-end test (needs `gh`)

# Desktop (nested module — cd in first)
cd desktop && go build ./... && go vet ./... && go test ./...
cd desktop/frontend && pnpm tsc --noEmit && pnpm build   # frontend type-check + bundle
cd desktop && wails dev      # hot-reload Go + Vite frontend
cd desktop && wails build    # -> build/bin/

# Hardware MCP verification (Arduino/ESP-IDF/PlatformIO toolchains)
make hardware-verify
make hardware-device-verify ARGS="..."
```

Run the agent: `reasonix setup` (config wizard → `./onecreat.toml`), then `reasonix chat` / `reasonix run "<task>"`. Secrets come from the environment via `api_key_env` (e.g. `DEEPSEEK_API_KEY`), or `.env` — never the TOML.

## Architecture

### The kernel pipeline (read these to understand the whole)

`internal/boot` (`boot.Build`) assembles everything from config into a **`control.Controller`** — the single, frontend-agnostic orchestration object. Every frontend drives the same Controller:

- `internal/cli` — the bubbletea chat TUI (`reasonix chat`) and headless `run`. Dispatch is `internal/cli/cli.go` (`run`/`chat`/`serve`/`setup`/`acp`/`mcp`/`codegraph`/`doctor`).
- `internal/serve` — HTTP + SSE server (`reasonix serve`).
- `internal/acp` — Agent Client Protocol (editor integrations). Its `internal/cli/acp.go` factory owns **no** assembly: every session is one `boot.Build` call, with the two genuinely per-session inputs passed as options (`Workspace` from the client's cwd, `ExtraPlugins` from `session/new`) plus `HostProvidesCodeIntel` so the agent doesn't start a second CodeGraph daemon and LSP manager inside an editor that already runs its own. `internal/cli/acp_assembly_test.go` fails if the transport starts importing runtime building blocks again.
- `desktop/` — Wails app; `desktop/app.go` is the bound surface the React frontend calls.

The Controller wraps **`internal/agent.Agent`** (the actual run loop: stream model → execute tool calls → repeat) over an **`internal/agent.Session`** (the message log). Tools come from `internal/tool` (registry); providers from `internal/provider` (registry).

**Events flow outward through a sink.** The Agent/Controller emit `internal/event.Event`s to an `event.Sink`; each frontend implements the sink to render the stream (TUI redraw, SSE frame, Wails `EventsEmit`). Frontends call *in* via Controller methods (`Submit`, `Approve`, `Rewind`, …) and observe *out* via the sink. There is no direct frontend→model path.

### Registry extensibility (no `switch model`)

`Provider` and `Tool` are interfaces resolved by name. Compile-time built-ins self-register via `init()` and are pulled in by blank imports in `main`:

- **Add a provider**: one file in `internal/provider/<kind>` with an `init()` registration. `internal/provider/openai` covers all OpenAI-compatible endpoints (DeepSeek, MiMo, etc.); `internal/provider/anthropic` is the Anthropic-native path.
- **Add a built-in tool**: one file in `internal/tool/builtin` with `init() { tool.RegisterBuiltin(...) }`.
- **Runtime plugins** (MCP): external executables declared in config, spoken to over newline-delimited JSON-RPC 2.0 (`internal/plugin`, stdio + Streamable HTTP). Each remote tool is adapted to the `Tool` interface (`mcp__server__tool`).

### Critical conventions (these have bitten before — respect them)

- **Session is single-writer + Snapshot.** `internal/agent/session.go`: `Session.Messages` is guarded by `mu`; only the run-loop goroutine writes (direct reads on that goroutine are lock-free). **Any cross-goroutine read must use `Session.Snapshot()`**; mutations from off the run loop (compaction, rewind) go through the locked methods (`Add`/`Replace`/`Truncate`). Compaction (`internal/agent/compact.go`) rewrites the log in place and fires `Agent.onCompact` so the Controller can invalidate stale checkpoint boundaries.

- **DeepSeek prefix cache shapes the prompt design.** The prompt grows append-only (high cache hits) until a turn nears the window, then **compaction** is the deliberate cache-reset point. Per-turn runtime state — plan-mode marker, coaching persona, memory framing — is injected at `Compose` time and is **never part of the cached system prefix**. Don't move runtime state into the system prompt.

- **Checkpoints / rewind.** `internal/control` keeps `cpBound` (turn → message index) for conversation rewind/fork; `internal/checkpoint` snapshots file pre-edits for code rewind. Any operation that restructures the message log must invalidate `cpBound` (see `InvalidateCheckpoints`).

- **Desktop multi-tab model.** `desktop/tabmanager.go` owns one independent `control.Controller` + event sink + session file + **workspace root** **per tab**, so background tabs run truly in parallel *and* can sit in different projects. `tabManager` is the **single source of truth**: there is no mirror of the active tab on `App` — "active" is just an id. Read a tab with `a.tabs.View(id)` / `Ctrl(id)` (a locked snapshot), write with `a.tabs.Update(id, fn)`; `""` resolves to the active tab for the frontend entry points that predate multi-tab. Anything that must reach a *specific* (possibly background) tab takes a `tabID` (e.g. `Approve`, `SetPlanMode`). After a slow rebuild, always write back by the **originating** id, never "the active tab". `a.mu` guards only App-level state (MCP views, the selected workspace) and never nests with the tab lock. `boot.Build` is seconds-long — never hold `a.mu` across it; build outside the lock, write back inside. Every `boot.Build` call must pass that tab's `Workspace` — omitting it silently drops the tab back to the process cwd.

- **Desktop bridge is hand-mirrored.** `desktop/frontend/src/lib/bridge.ts` (`AppBindings`) mirrors `desktop/app.go`'s exported method set by hand — change a Go signature and you must update the TS interface, the in-browser mock (`makeMockApp`), and call sites. Wails passes args positionally. `internal/eventwire` now owns the JSON event contract — the `event.Kind` → wire-string map, the wire structs, and the encoder. `desktop/wire.go` and `internal/serve/wire.go` are thin alias/delegation shims over it, so a new `event.Kind` is registered once (`eventwire.KindNames`); `TestKindNamesCoversEveryDeclaredKind` fails until it is. In Web mode the browser-facing HTTP surface is an explicit allowlist (`desktop/rpc_surface.go`, `rpcPublicMethods`): a new exported `*App` method does **not** become an endpoint until it is listed there *and* in `AppBindings`; `TestRPCSurfaceMatchesFrontendBindings` keeps the two sets identical.

### Runtime scopes (ownership, not directories)

`internal/runtime` names the four lifetimes explicitly: **Process → Workspace → Session → Turn**. A Workspace is *shared* by every session on the same root (refcounted via `OpenWorkspace`/`Release`); Sessions and Turns are never shared. Closing a scope closes its children first, then its own resources in reverse registration order; `Defer` is a method on the scope you hold, so a resource can only be attached to a lifetime you actually have. Cancellation flows **downward only** — a cancelled Turn never touches its Session.

> **Not wired yet.** `boot.Build` still chains MCP / CodeGraph / LSP into one session-scoped `cleanup`, so closing one tab still stops a CodeGraph daemon a sibling tab shares. Landing the real wiring is Plan 04 (single composition root) and Plan 05 (resource lifetimes). Don't treat the scope package as evidence that the sharing bug is fixed.

### Config & memory

- **Workspace is an explicit value, not the process cwd.** `internal/workspace.Context` (immutable, always absolute; zero value = process-cwd semantics) is threaded through `config.LoadIn`, `boot.Options.Workspace`, `builtin.ConfineWorkspace` and `control.Options.WorkspaceRoot`. Runtime workspace switching must never call `os.Chdir` — only *startup* may (`desktop.resolveStartupWorkspace`, the web `--workspace` flag). Anything workspace-relative (project config, `.mcp.json`, `.env`, memory, skills, file tools, bash, CodeGraph, LSP, hooks, checkpoint confinement) resolves against the Context.
- Resolution order: **flag > `<workspace>/onecreat.toml` > `~/.config/onecreat/config.toml` > built-in defaults** (`internal/config`).
- `[[plugins]]` and a project-root `.mcp.json` are merged; `onecreat.toml` wins on name collision. `.mcp.json` entries are tracked by `PluginEntry.Source` and are **not** written back into `onecreat.toml` on save.
- `internal/config/render.go` renders annotated TOML and **must round-trip** through `Load` — there's a `TestRenderTOMLRoundTrips` guard. If you add a config field, render it and extend that test.
- Project memory lives in `AGENTS.md` / `REASONIX.md` (generated by `/init` inside a session). The hardware MCP server is a separate binary (`cmd/reasonix-hardware-mcp`) providing Arduino/ESP-IDF/PlatformIO tools; see `docs/HARDWARE_MCP.md`.

## Notes

- Reference specs live in `docs/SPEC.md`, `docs/CHECKPOINTS.md`, `docs/HARDWARE_MCP.md`.
- Recent in-repo comments are often written in Chinese (this is the user's fork); match the surrounding file's comment language rather than imposing one.
