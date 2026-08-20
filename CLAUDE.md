# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

OneCreat is a DeepSeek-native AI coding agent (a Claude-Code/Codex-style harness) written in Go. Active development is on the `main-v2` branch; the `v1` branch is the legacy TypeScript build (maintenance only). This is the user's own fork/deployment ("onecreat"), tuned DeepSeek-first.

> **本地开发工作流见 [`docs/开发工作流.md`](docs/开发工作流.md)**(构建/打包/装机、账号网关本地实测、改完自查三连、红线教训)。
> 本文件夹 `/Users/localwork/06_System/onecreat` 是 onecreat 的**唯一开发仓库**(2026-06-18 从 reasonix 物理分离,origin=weizunwhite/onecreat,无 upstream);旧文件夹 `reasonix_source/DeepSeek-Reasonix` 是分离前副本,**别在那开发**。
> ⚠️ **不要把 reasonix 上游的流式渲染优化往这搬**——已实测更差,详见 `docs/开发工作流.md` 红线段。
> **账号系统 / 登录 / 权限 / 点数 / 档位 / AI 网关**——onecreat 是教学平台(teacher)的 B 端客户端,这些改动前先读 [`docs/账号系统与教学平台互通.md`](docs/账号系统与教学平台互通.md)(讲清两半代码在哪、改一件事动哪两边;对端仓库 `/Users/zunwei/system/teacher`)。

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

# Web 模式(单二进制起本地 HTTP 服务 + 浏览器当 UI,纯 Go 免打包;见 docs/Web模式.md)
make build-web               # -> bin/onecreat-web  (等价于 pnpm build + go build -tags web)
make release-web VERSION=vX.Y.Z   # 主分发形态:全平台 Web 发行包 -> dist/ (scripts/web-build.sh;单平台传 os/arch)
cd desktop && go test -tags web ./...   # web 标签下的测试也要绿

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

- **Desktop multi-tab model.** `desktop/tabmanager.go` owns one independent `control.Controller` + event sink + session file + **workspace root** **per tab**, so background tabs run truly in parallel *and* can sit in different projects. `tabManager` is the **single source of truth**: there is no mirror of the active tab on `App` — "active" is just an id. Read a tab with `tabs.View(id)` / `Ctrl(id)` (a locked snapshot), write with `tabs.Update(id, fn)`; `""` resolves to the active tab for the frontend entry points that predate multi-tab. Anything that must reach a *specific* (possibly background) tab takes a `tabID` (e.g. `Approve`, `SetPlanMode`). Building and rebuilding a tab's controller lives in `desktop/tab_runtime.go` (`tabRuntimeService`), which also owns the selected project folder: after a slow rebuild, always write back by the **originating** id, never "the active tab"; `boot.Build` is seconds-long — never hold a lock across it; and every `boot.Build` call must pass that tab's `Workspace` (omitting it silently drops the tab back to the process cwd).

- **`desktop.App` is a transport facade, not an owner.** Its fields are only `ctx`, `shell`, `tabs`, `factory` and the domain services (`hw`, `mcp`, `files`, `memory`, `sessions`, `rt`, `serial`, wired in `wireServices`); every mutable domain state and every lock lives in a service. Methods on `App` are delegation or DTO mapping — `desktop/app_facade_test.go` fails if a `sync.Mutex` reappears on the struct, if a field outside the whitelist shows up, if `app.go` grows past 900 lines, or if a method body there exceeds 26 lines. Services take their dependencies as injected funcs (`root`, `ctrl`, `shell`, `ctx`), never a back-pointer to `*App`. Tests build one with `newBareApp`, not a bare `&App{}`.

- **The Go↔TS method contract is generated.** `desktop/frontend/src/lib/bindings.generated.ts` (`AppBindings`) is produced by `desktop/cmd/gen-bindings` (`cd desktop && go generate ./...`) from `rpcPublicMethods` plus the listed methods' Go signatures and doc comments — see `desktop/internal/tsgen`. Never hand-edit it; `TestFrontendBindingsAreUpToDate` compares it byte-for-byte against a fresh render. Changing a Go signature means regenerating, then updating the in-browser mock (`makeMockApp`) and call sites in `bridge.ts`. Wails passes args positionally. The *data* types in `frontend/src/lib/types.ts` are still mirrored by hand and must keep the Go type's name — the generated interface references them by that name. `internal/eventwire` now owns the JSON event contract — the `event.Kind` → wire-string map, the wire structs, and the encoder. `desktop/wire.go` and `internal/serve/wire.go` are thin alias/delegation shims over it, so a new `event.Kind` is registered once (`eventwire.KindNames`); `TestKindNamesCoversEveryDeclaredKind` fails until it is.

- **`*App` must not import the Wails runtime.** Everything host-specific — event emit, native dialogs, opening a URL, raising the window — goes through the `Shell` interface (`desktop/shell.go`). `wailsShell` (`//go:build !web`) is the desktop impl; `webShell` backs the browser-based Web mode (`docs/Web模式.md`), where App methods are dispatched by reflection over `POST /rpc/<Method>` and events stream over one SSE. The HTTP surface is an explicit allowlist in `desktop/rpc_surface.go` (`rpcPublicMethods`) — adding an exported `*App` method does **not** expose it over HTTP; list it there and run `go generate ./...`, which regenerates the frontend's `AppBindings` from that same list. Allowlisted signatures must stay RPC-compatible (no variadics; returns limited to `()`/`(T)`/`(error)`/`(T, error)`). `TestAppMethodsAreRPCCompatible` and `TestFrontendBindingsAreUpToDate` guard both halves.

- **SaaS gateway hides the backend model — never leak it.** In OneCreat's online deployment, the client talks to the platform AI gateway (an OpenAI-compatible endpoint authed via `ONECREAT_GATEWAY_TOKEN`, see `internal/provider/openai/openai.go`) and the user only ever sees a subscription *tier* (标准/高级/旗舰). The real provider/model/route is a billing-and-routing secret: revealing it both leaks IP and lets users bypass tier-based metering. `config.ModelPrivacyPolicy` (`internal/config/config.go`) is injected at runtime to enforce this, and the client-side planner is disabled on the gateway path for the same reason. Don't add code paths or prompts that surface the underlying model name. Wallet/points readout lives in `internal/billing`.

### Evidence engine (the platform's reason to exist)

`internal/evidence` is the verification layer that turned this from a coding harness into a teaching-project platform: it matches `complete_step` calls against the latest `todo_write` list and accumulates a real evidence chain (commands run, files produced, device output) so a project's claims are backed by what actually happened, not by the model's say-so. It is intentionally **zero hardware coupling** — the same engine backs software and hardware projects. `internal/skill` resolves `/skill-name` invocations (built-ins in `skill/builtins.go`, indexed in `index.go`) and adapts them to the tool surface.

### Runtime scopes (ownership, not directories)

`internal/runtime` names the four lifetimes explicitly: **Process → Workspace → Session → Turn**. A Workspace is *shared* by every session on the same root (refcounted via `OpenWorkspace`/`Release`); Sessions and Turns are never shared. Closing a scope closes its children first, then its own resources in reverse registration order; `Defer` is a method on the scope you hold, so a resource can only be attached to a lifetime you actually have. Cancellation flows **downward only** — a cancelled Turn never touches its Session.

**Resource lifetimes are wired to those scopes.** `boot.Factory` (`internal/boot/factory.go`) owns the process- and workspace-scoped half: `OpenWorkspace` refcounts a project's shared services by root and registers their teardown on the **workspace** scope, so the LSP manager and the CodeGraph daemon survive any one session. `boot.Build` opens a **session** scope under that workspace and hangs the session's own resources (the MCP plugin host) there. Closing a tab therefore stops that tab's MCP subprocesses and nothing a sibling on the same project is using.

- A `Factory` is **explicit, never global**: pass it in `boot.Options.Factory`. Nil means "give this session a private one", which is right for a single-workspace process (CLI, headless, ACP) and reproduces the pre-Plan-05 behaviour.
- The desktop creates one in `NewApp` and **every** `boot.Build` call site must pass it (`TestEveryBuildSharesTheFactory` fails otherwise — a missed site silently un-shares that tab).
- The three rebuild paths (`SetModel`, `rebuildTabByID`, settings `rebuild`) close the old controller before building the new one, so they must hold a reference across the swap via `App.holdWorkspace` / `Factory.Hold`; without it the refcount touches zero and the project's services are stopped and immediately restarted (`TestRebuildPathsHoldTheWorkspace`).
- What is deliberately **not** workspace-scoped: the MCP host and jobs (session-owned), the skill/memory indexes (per-build snapshots — caching them would serve stale `AGENTS.md`), and the CodeGraph *binary path* (background auto-install must become visible next session, not "once every tab closes"). `runtime.Turn` is still unwired — turn-level ownership lands with Plans 07/08.

### Config & memory

- **Workspace is an explicit value, not the process cwd.** `internal/workspace.Context` (immutable, always absolute; zero value = process-cwd semantics) is threaded through `config.LoadIn`, `boot.Options.Workspace`, `builtin.ConfineWorkspace` and `control.Options.WorkspaceRoot`. Runtime workspace switching must never call `os.Chdir` — only *startup* may (`desktop.resolveStartupWorkspace`, the web `--workspace` flag). Anything workspace-relative (project config, `.mcp.json`, `.env`, memory, skills, file tools, bash, CodeGraph, LSP, hooks, checkpoint confinement) resolves against the Context.
- Resolution order: **flag > `<workspace>/onecreat.toml` > `~/.config/onecreat/config.toml` > built-in defaults** (`internal/config`).
- `[[plugins]]` and a project-root `.mcp.json` are merged; `onecreat.toml` wins on name collision. `.mcp.json` entries are tracked by `PluginEntry.Source` and are **not** written back into `onecreat.toml` on save.
- `internal/config/render.go` renders annotated TOML and **must round-trip** through `Load` — there's a `TestRenderTOMLRoundTrips` guard. If you add a config field, render it and extend that test.
- Project memory lives in `AGENTS.md` / `REASONIX.md` (generated by `/init` inside a session). The hardware MCP server is a separate binary (`cmd/reasonix-hardware-mcp`) providing Arduino/ESP-IDF/PlatformIO tools; see `docs/HARDWARE_MCP.md`.

## Notes

- Reference specs live in `docs/SPEC.md`, `docs/CHECKPOINTS.md`, `docs/HARDWARE_MCP.md`.
- Recent in-repo comments are often written in Chinese (this is the user's fork); match the surrounding file's comment language rather than imposing one.
