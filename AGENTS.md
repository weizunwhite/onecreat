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

**One seam to the thing that actually runs a turn: `internal/engine`.** `Controller` holds a single `engine.TurnEngine` — `Start(ctx, TurnRequest) (TurnHandle, error)`, and `TurnHandle` is `Cancel`/`Wait`. That is the entire boundary, and it is meant to stay that way: `internal/engine/boundary_test.go` fails if `TurnEngine` grows a second method, if any of the interfaces sprouts an application-policy verb (`Approve`, `History`, `Resume`, `Fork`, `SetPlanMode`, …), or if `internal/engine` or the dsh adapter imports `control`/`toolpolicy`/`permission`/`checkpoint`/`evidence`/`memory`/`billing`/`tool`/`plugin`. OneCreat's policies — memory, evidence, permission, billing, checkpoints — sit above that seam, but **they only reach an engine that runs tools on this side of it**, and that distinction is the whole safety story. `internal/engine/native` (wrapping the `agent.Runner`) declares `hosted-tools`: every call goes through the `toolpolicy` pipeline. `internal/engine/dsh` cannot — it runs its own tools in its own process, so by the time a `tool/call` reaches us the file is already written and the shell has already run; an after-the-fact look is not a gate. So `boot.selectEngine` **fails closed**: `engine = "dsh"` is refused at assembly with a typed `engine.UnsupportedError`, and there is deliberately no config switch to bypass it — a safety gate one line of TOML can disable is not a gate. `engine` is otherwise a live config key (a bad value fails at assembly instead of silently running native). Capabilities are **enforced, not just declared**: `Controller.Supports` is the read-only query, and `Fork`/`Branch`/`SwitchBranch`/`Rewind`/`NewSession`/`Compact`/`Resume` each call `requireCap` *before touching any state*, so an unsupported op can never leave a half-mutated shadow session behind a "success" the UI believes. An engine that declares nothing supports nothing. The session record carries the engine that actually ran it, never a hardcoded `native`. Cancellation has exactly one trigger, the turn context; `TurnHandle.Cancel` is how that reaches an engine that would not die with it.

**Product policy around a tool call lives in `internal/toolpolicy`, not in the loop.** One `Pipeline` carries plan mode, the permission gate, PreToolUse/PostToolUse hooks, the checkpoint pre-edit seam, the evidence ledger, and the job/memory handles a tool reaches through its context — plus the end-of-turn todo reconcile reminder. Its stage order is a published contract: `plan mode → gate → PreToolUse hook → checkpoint snapshot → context injection`, with the snapshot deliberately last so a *refused* call never leaves a rewind point for a change that never happened. `agent.Agent` holds one `policy` field and calls `Before`/`After` around `t.Execute`; `internal/agent/policy_boundary_test.go` fails if the package imports `evidence`/`memory`/`diff`/`permission`/`checkpoint`/`hook`/`billing` again, or if `executeOne` grows past 55 lines. Add tool-call policy to the pipeline, never to the loop — any second engine gets it by calling the same two methods. Compaction is deliberately *not* here: keeping `messages` inside the context window is part of running a model loop, not product policy.

**`control.Controller` is a compat facade over six services**, each owning its own state *and its own lock*: `approvalBroker` (`approval.go` — approval/ask prompts, session grants, YOLO/bypass, the just-approved-plan window), `checkpointService` (`checkpoints.go` — store, monotonic turn counter, turn→message-index boundaries), `sessionStore` (`session_store.go` — session dir, active file, per-turn autosave; it owns the *file*, never the message log), `mcpService` (`mcp.go` — plugin host, live tool registry, hot-add context), `memoryService` (`memory.go` — snapshot + the notes queued to ride the next turn), and `turnState` (`turn_state.go` — running/busy mutual exclusion, cancel, turn counter, plan mode, coaching persona). The Controller itself holds **no mutex**; `internal/control/facade_test.go` fails if one reappears on the struct or if `controller.go` grows past 950 lines. Add domain state to a service, not to the Controller.

**Events flow outward through a sink.** The Agent/Controller emit `internal/event.Event`s to an `event.Sink`; each frontend implements the sink to render the stream (TUI redraw, SSE frame, Wails `EventsEmit`). Frontends call *in* via Controller methods (`Submit`, `Approve`, `Rewind`, …) and observe *out* via the sink. There is no direct frontend→model path.

**Not every event may be dropped.** `Kind.Durable()` (`internal/event`) classifies each kind: only `reasoning`, `text` and `tool_progress` are ephemeral — a later event supersedes them — and **everything else is durable by default**, so a new kind is state-bearing until proven otherwise. `internal/eventstream` is the one delivery implementation both SSE transports use: ephemeral frames are dropped when a subscriber falls behind, durable frames queue, and a client that will not consume them is disconnected (`stream_reset`) to re-sync rather than served a stream with invisible holes. `Publish` never blocks — it runs on the agent's run-loop goroutine. Every frame carries the V2 envelope (`schemaVersion`/`eventId`/`sequence`/`sessionId`/`tabId`/`timestamp`/`durable`) stamped by an `eventwire.Stamper`, one per stream; `sequence` is gap-free so a client can *detect* loss. `Encode` produces the payload only — envelopes belong to streams, not events.

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

- **The Go↔TS method contract is generated.** `desktop/frontend/src/lib/bindings.generated.ts` (`AppBindings`) is produced by `desktop/cmd/gen-bindings` (`cd desktop && go generate ./...`) from `rpcPublicMethods` plus the listed methods' Go signatures and doc comments — see `desktop/internal/tsgen`. Never hand-edit it; `TestFrontendBindingsAreUpToDate` compares it byte-for-byte against a fresh render. Changing a Go signature means regenerating, then updating the in-browser mock (`makeMockApp`) and call sites in `bridge.ts`. Wails passes args positionally. The *data* types in `frontend/src/lib/types.ts` are still mirrored by hand and must keep the Go type's name — the generated interface references them by that name. `internal/eventwire` now owns the JSON event contract — the `event.Kind` → wire-string map, the wire structs, and the encoder. `desktop/wire.go` and `internal/serve/wire.go` are thin alias/delegation shims over it, so a new `event.Kind` is registered once (`eventwire.KindNames`); `TestKindNamesCoversEveryDeclaredKind` fails until it is. In Web mode the browser-facing HTTP surface is that same explicit allowlist (`desktop/rpc_surface.go`, `rpcPublicMethods`): a new exported `*App` method does **not** become an endpoint until it is listed there — and listing it is what puts it into the generated `AppBindings`.

- **The platform account is an object, not environment variables.** `internal/account` owns it: a `Gateway` (URL, bearer token, tier — with its own lock) and the `CredentialSource` a provider asks *on every request*, so a refreshed token reaches sessions that are already running without a rebuild. Whoever owns the account holds the `Gateway` and passes it down (`boot.Options.Gateway`, `control.Options.Gateway`); the desktop shares one object across every tab. `ONECREAT_GATEWAY_URL` / `_TOKEN` / `ONECREAT_TIER` survive only as a **transport**: `account.FromEnv()` imports them once at process start, `Gateway.Env()` projects a session into a subprocess. Reading or writing them anywhere else fails `internal/account/boundary_test.go`, which scans the whole tree. A provider client also learns it is on the gateway from the explicit `provider.Config.Gateway` flag (never by matching a key-env name) — that flag is what stops an upstream error body, which names the real model, from being surfaced verbatim.

- **A session's identity and metadata belong to `internal/session`; its transcript belongs to the engine.** A `Record` holds the id, engine, `Store` (the engine's transcript reference, which the registry never parses), workspace, title, kind, timestamps and display map; a `Registry` keeps them in one `.sessions.json` index per session directory. Preview/turns/timestamps still come from `agent.ListSessions` — that is the engine reading its own file. Title is freely editable; **workspace and kind are write-once** (which project a conversation belongs to should not change because the user switched folders later). `control.sessionStore` registers every session, so a CLI or ACP session has identity too, not just a desktop one. Concurrency: the registry serialises read-modify-write, but an atomic rename does not prevent a *lost update* — so all of the desktop's metadata writes go through the single `sessionService.reg` instance, and `desktop/session_owner_test.go` fails if a second `session.Open` appears. The four legacy sidecars (`.titles/.display/.cwds/.kinds`) are imported once and deliberately left on disk for downgrades.

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
