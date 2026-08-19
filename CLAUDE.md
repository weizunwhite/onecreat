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
- `internal/acp` — Agent Client Protocol (editor integrations).
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

- **Desktop multi-tab model.** `desktop/app.go` runs one independent `control.Controller` + event sink + session file **per tab** (`tabRuntime`), so background tabs run truly in parallel. The `App.ctrl`/`sink`/`model` fields are a **mirror of the active tab**; `a.mu` guards them and the `tabs` map. Methods that must reach a *specific* (possibly background) tab take a `tabID` and route via `a.tabs[tabID]` (e.g. `Approve`, `SetPlanMode`); the rest operate on the active mirror. Always read `a.ctrl` under `a.mu` (capture a local, then use it). `boot.Build` is seconds-long — never hold `a.mu` across it; build outside the lock, write back inside.

- **Desktop bridge is hand-mirrored.** `desktop/frontend/src/lib/bridge.ts` (`AppBindings`) mirrors `desktop/app.go`'s exported method set by hand — change a Go signature and you must update the TS interface, the in-browser mock (`makeMockApp`), and call sites. Wails passes args positionally. `desktop/wire.go` and `internal/serve/wire.go` separately map `event.Kind` → wire strings (keep both in step).

- **`*App` must not import the Wails runtime.** Everything host-specific — event emit, native dialogs, opening a URL, raising the window — goes through the `Shell` interface (`desktop/shell.go`). `wailsShell` (`//go:build !web`) is the desktop impl; `webShell` backs the browser-based Web mode (`docs/Web模式.md`), where the same 112 exported `*App` methods are dispatched by reflection over `POST /rpc/<Method>` and events stream over one SSE. Adding an `*App` method needs no routing work, but its signature must stay RPC-compatible (no variadics; returns limited to `()`/`(T)`/`(error)`/`(T, error)`) — `TestAppMethodsAreRPCCompatible` guards this.

- **SaaS gateway hides the backend model — never leak it.** In OneCreat's online deployment, the client talks to the platform AI gateway (an OpenAI-compatible endpoint authed via `ONECREAT_GATEWAY_TOKEN`, see `internal/provider/openai/openai.go`) and the user only ever sees a subscription *tier* (标准/高级/旗舰). The real provider/model/route is a billing-and-routing secret: revealing it both leaks IP and lets users bypass tier-based metering. `config.ModelPrivacyPolicy` (`internal/config/config.go`) is injected at runtime to enforce this, and the client-side planner is disabled on the gateway path for the same reason. Don't add code paths or prompts that surface the underlying model name. Wallet/points readout lives in `internal/billing`.

### Evidence engine (the platform's reason to exist)

`internal/evidence` is the verification layer that turned this from a coding harness into a teaching-project platform: it matches `complete_step` calls against the latest `todo_write` list and accumulates a real evidence chain (commands run, files produced, device output) so a project's claims are backed by what actually happened, not by the model's say-so. It is intentionally **zero hardware coupling** — the same engine backs software and hardware projects. `internal/skill` resolves `/skill-name` invocations (built-ins in `skill/builtins.go`, indexed in `index.go`) and adapts them to the tool surface.

### Config & memory

- Resolution order: **flag > `./onecreat.toml` > `~/.config/onecreat/config.toml` > built-in defaults** (`internal/config`).
- `[[plugins]]` and a project-root `.mcp.json` are merged; `onecreat.toml` wins on name collision. `.mcp.json` entries are tracked by `PluginEntry.Source` and are **not** written back into `onecreat.toml` on save.
- `internal/config/render.go` renders annotated TOML and **must round-trip** through `Load` — there's a `TestRenderTOMLRoundTrips` guard. If you add a config field, render it and extend that test.
- Project memory lives in `AGENTS.md` / `REASONIX.md` (generated by `/init` inside a session). The hardware MCP server is a separate binary (`cmd/reasonix-hardware-mcp`) providing Arduino/ESP-IDF/PlatformIO tools; see `docs/HARDWARE_MCP.md`.

## Notes

- Reference specs live in `docs/SPEC.md`, `docs/CHECKPOINTS.md`, `docs/HARDWARE_MCP.md`.
- Recent in-repo comments are often written in Chinese (this is the user's fork); match the surrounding file's comment language rather than imposing one.
