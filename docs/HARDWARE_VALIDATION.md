# Hardware Platform Validation

Date: 2026-06-03 (local machine, Asia/Shanghai)

This document records the current verification status for the Reasonix hardware
programming platform work.

## Verified

- `gofmt` on the hardware MCP and desktop bridge files.
- `go test -count=1 ./cmd/reasonix-hardware-mcp`
- `go test ./...`
- `go test -count=1 ./...` in `desktop/`
- `pnpm build` in `desktop/frontend/`
- `make hardware-verify`
- `scripts/hardware-device-verify.sh --help`
- `make hardware-device-verify ARGS="--platform platformio --project-dir
  dist/hardware-verify-20260603-103946/platformio_verify --board esp32dev
  --environment esp32dev --local-only --timeout-seconds 240"`
- `make windows-package-verify`
- `make cross`
- `scripts/cache-guard.sh`:
  - original release cache-hit guard still passes.
- Browser smoke test at `http://127.0.0.1:5174/`:
  - hardware drawer opens from the sidebar.
  - `Hardware MCP`, `自动验证`, `ESP-IDF 官方 MCP`, device status, and toolchain
    status are visible.
  - browser console reported zero errors.
- Browser workflow test at `http://127.0.0.1:5175/`:
  - hardware drawer opens from the toolbar.
  - `Hardware MCP`, `自动验证`, `ESP-IDF 官方 MCP`, `真实验证链路`,
    `Arduino CLI`, and `SSH / SCP` are visible.
  - clicking `自动验证` sends a prompt containing
    `mcp__hardware__hardware_detect`.
  - no failed requests or console errors were reported.
  - screenshot: `dist/hardware-panel-ux-20260603.png`
- Browser workflow test at `http://127.0.0.1:5176/` with Playwright fallback
  because the Browser navigation API was unavailable:
  - hardware drawer opens from the sidebar.
  - clicking `自动验证` sends a prompt containing
    `mcp__hardware__hardware_detect`,
    `mcp__hardware__hardware_project_audit`, and
    `mcp__hardware__hardware_project_validate`.
  - no console errors or warnings were reported.
- Browser workflow test at `http://127.0.0.1:5177/` with Playwright fallback:
  - hardware drawer opens from the sidebar.
  - clicking `自动验证` sends a prompt containing
    `mcp__hardware__hardware_detect`,
    `mcp__hardware__hardware_project_audit`,
    `mcp__hardware__hardware_project_validate`, and
    `mcp__hardware__hardware_evidence_record`.
  - no console errors or warnings were reported.
- Browser workflow test at `http://127.0.0.1:5178/` with Playwright fallback:
  - hardware drawer opens from the sidebar.
  - clicking `自动验证` sends a prompt containing
    `mcp__hardware__hardware_detect`,
    `mcp__hardware__hardware_project_audit`,
    `mcp__hardware__hardware_project_validate`,
    `mcp__hardware__hardware_evidence_record`, and
    `mcp__hardware__hardware_evidence_status`.
  - no console errors or warnings were reported.
- Hardware drawer workflow buttons:
  - no longer depend on project-local `/hardware:*` slash commands.
  - auto-connect the `hardware` MCP server before sending the task prompt.
  - send cross-workspace Chinese prompts that explicitly instruct the agent to
    call `mcp__hardware__hardware_detect`, `hardware_project_audit`, and the
    relevant hardware MCP validation/device tools.
- Hardware scaffolds now generate and test common AI hardware project artifacts:
  - `hardware_manifest.json`
  - `docs/wiring.md`
  - `docs/verification.md`
  - `tests/hardware_checklist.md`
- `scripts/desktop-build.sh darwin/arm64 hardware-fingerprint-evidence`
- `scripts/desktop-build.sh darwin/arm64 hardware-window-visibility`
- `scripts/desktop-build.sh darwin/arm64 hardware-device-plan`
- `scripts/desktop-build.sh darwin/arm64 hardware-wiring-audit`
- `scripts/desktop-build.sh darwin/arm64 hardware-runtime-evidence`
- `scripts/desktop-build.sh darwin/arm64 hardware-runtime-output`
- Bundled macOS app contains:
  - `OneCreat.app/Contents/MacOS/onecreat-desktop`
  - `OneCreat.app/Contents/MacOS/onecreat-hardware-mcp`
- Bundled macOS zip was checked after extraction and no `._*` AppleDouble files
  were present.
- Bundled `onecreat-hardware-mcp` responds to MCP `tools/list`.
- Bundled `onecreat-hardware-mcp` responds to `hardware_detect`.
- Installed `/Applications/OneCreat.app` now contains the hardware platform
  build:
  - bundle name: `Reasonix`
  - bundle identifier: `dev.reasonix.desktop`
  - executable: `/Applications/OneCreat.app/Contents/MacOS/onecreat-desktop`
  - bundled MCP: `/Applications/OneCreat.app/Contents/MacOS/onecreat-hardware-mcp`
  - `codesign --verify --deep --strict --verbose=2 /Applications/OneCreat.app`
    passed.
  - CoreGraphics reports the Reasonix window onscreen at `1240 x 720`.
  - The visible sidebar includes the `硬件` entry.
  - screenshot: `dist/reasonix-installed-current-window-20260603.png`
- `hardware_project_audit` checks generated projects for manifest, wiring guide,
  verification guide, hardware checklist, entrypoints, and platform consistency.
- `hardware_project_validate` auto-detects and validates generated projects.
- `hardware_evidence_record` appends validation evidence to:
  - `tests/hardware_evidence.jsonl`
  - `tests/hardware_checklist.md`
  - each JSONL record includes the current project fingerprint, so evidence is
    tied to the code/config state that was actually validated.
- `hardware_evidence_status` summarizes recorded evidence and distinguishes:
  - `hardware_pending` for local-only validation evidence.
  - `hardware_verified` only when real-device upload/deploy and runtime evidence
    are both recorded.
  - runtime evidence stages such as `monitor`, `serial`, `mpremote`, `ssh`, and
    `deploy` only count when the evidence record includes real output beyond
    command headers.
  - PlatformIO monitor, ESP-IDF monitor, `mpremote_run`, and SSH remote commands
    return an error when no runtime output is captured.
  - `stale` when evidence exists but the source/config fingerprint has changed
    since it was recorded.
- `hardware_device_verify_plan` generates real-device verification plans:
  - returns `localOnlyCommand` and `realDeviceCommand`.
  - reports `missingInputs` before flashing or deployment.
  - returns MCP steps for GUI/agent workflows.
  - is included in generated `hardware_manifest.json` under `mcpTools`.
- `hardware-device-verify.sh` provides the real-device lab runner:
  - audits the project.
  - validates local compile/syntax.
  - records local evidence.
  - runs platform-specific upload/flash/deploy/log stages when device arguments
    are provided.
  - records real-device evidence and checks `hardware_evidence_status`.
  - local-only smoke result:
    `dist/hardware-device-verify-20260603-104247/summary.json`, final
    status `hardware_pending`, `currentRecordCount=1`, `staleRecordCount=4`,
    missing `device_upload` and `runtime_log`.
- Installed `/Applications/OneCreat.app` package-local MCP checks:
  - has bundle name `Reasonix`.
  - has bundle identifier `dev.reasonix.desktop`.
  - is running from `/Applications/OneCreat.app/Contents/MacOS/onecreat-desktop`.
  - contains `onecreat-hardware-mcp`, which responds to MCP `tools/list`.
  - package-local `tools/list` includes `hardware_project_audit`.
  - package-local `tools/list` includes `hardware_evidence_record`.
  - package-local `tools/list` includes `hardware_evidence_status`.
  - package-local `hardware_evidence_status` returns `hardware_pending` after
    compile-only evidence and reports missing `device_upload` and `runtime_log`.
  - package-local `hardware_evidence_status` returns `hardware_verified` only
    after upload and non-empty serial monitor evidence are both recorded.
  - package-local weak-evidence check generated
    `dist/installed-weak-evidence-20260603-031426/pio_weak`; compile and upload
    evidence plus an empty monitor record still returned `hardware_pending` with
    missing `runtime_log`.
  - package-local runtime-evidence check generated
    `dist/installed-runtime-evidence-20260603-032837/pio_weak`; compile and
    upload evidence plus a monitor record containing only the command header
    still returned `hardware_pending` with missing `runtime_log`.
  - package-local runtime-output check generated
    `dist/installed-runtime-output-20260603-104753/summary.json`;
    `mpremote_run` and `ssh_deploy_run` both returned `isError=true` when fake
    device commands produced no runtime output, while explicit
    `require_output=false` calls were allowed for background/deploy-only runs.
  - package-local `hardware_evidence_record` writes `projectFingerprint` and
    `fingerprintFileCount`.
  - package-local `hardware_evidence_status` returns `stale` after a source file
    changes from the fingerprint recorded by compile evidence.
  - package-local `hardware_project_audit` on a generated PlatformIO scaffold
    returns `13 passed, 0 failed, 0 warnings`.
  - package-local `hardware_evidence_record` on a generated PlatformIO scaffold
    writes `tests/hardware_evidence.jsonl` and appends the readable evidence
    summary into `tests/hardware_checklist.md`.
  - package-local `hardware_detect` sees the generated ESP-IDF project as
    `esp_idf` and reports `ESP-IDF v6.0`.
  - package-local `arduino_monitor_sample` on a missing port returns
    `isError=true` with the full command and the no-serial-output diagnostic.
  - contains no `._*` AppleDouble files.
- Windows NSIS packaging is configured to install `onecreat-hardware-mcp.exe`
  next to the desktop executable.
- Windows package verification:
  - `dist/Reasonix-windows-amd64-installer.exe` was built by
    `scripts/desktop-build.sh windows/amd64 windows-package-verify`.
  - `7zz` recognizes the installer as an NSIS archive and extracts it.
  - extracted payload contains `onecreat-desktop.exe`.
  - extracted payload contains `onecreat-hardware-mcp.exe`.
  - extracted `onecreat-hardware-mcp.exe` checksum matches the NSIS payload
    built at `desktop/build/windows/installer/onecreat-hardware-mcp.exe`.
  - `go version -m` confirms the hardware MCP payload is
    `reasonix/cmd/reasonix-hardware-mcp`, `GOOS=windows`, `GOARCH=amd64`.
  - NSIS config uses `$LOCALAPPDATA` per-user install and HKCU uninstall
    registry entries.
  - summary: `dist/windows-package-verify-20260603-104432/summary.json`
- Windows native smoke runner:
  - `scripts/windows-native-smoke.ps1` installs the NSIS package silently into a
    temporary per-user directory.
  - verifies `onecreat-desktop.exe`, `onecreat-hardware-mcp.exe`, and
    `uninstall.exe` exist after install.
  - calls bundled `onecreat-hardware-mcp.exe` with `tools/list` and
    `hardware_detect`.
  - launches the desktop process briefly.
  - runs silent uninstall and checks payload removal.
  - `.github/workflows/release-desktop.yml` runs this automatically on the
    Windows build runner and uploads `windows-native-smoke-summary.json` as a
    diagnostic artifact.
- `make cross` builds hardware MCP binaries for:
  - `darwin/amd64`
  - `darwin/arm64`
  - `linux/amd64`
  - `linux/arm64`
  - `windows/amd64`
  - `windows/arm64`

## Toolchain Detection

Current machine status:

- Available:
  - Arduino CLI: `/opt/homebrew/bin/arduino-cli` (`arduino-cli 1.2.2`)
  - PlatformIO: `/Users/zunwei/Library/Python/3.9/bin/pio` (`PlatformIO Core 6.1.19`)
  - ESP-IDF: `/Users/localwork/06_System/tools/esp/v6.0/esp-idf` (`ESP-IDF v6.0`)
  - ESP-IDF Python environment: `/Users/zunwei/.espressif/tools/python/v6.0/venv`
  - Espressif EIM CLI: `/Users/localwork/06_System/tools/eim/eim` (`eim 0.12.3`)
  - mpremote: `/Users/zunwei/Library/Python/3.9/bin/mpremote` (`mpremote 1.27.0`)
  - ssh/scp: `/usr/bin/ssh`, `/usr/bin/scp`
- Connected development boards:
  - None detected.
- `hardware_detect` returned:
  - project type: `esp_idf` for the generated ESP-IDF scaffold.
  - serial ports: empty list.
  - Arduino boards: empty list.
  - PlatformIO devices: empty list.
  - ESP-IDF idf.py available through the local wrapper.

System-only ports such as `/dev/cu.Bluetooth-Incoming-Port` and
`/dev/cu.debug-console` are filtered out by `hardware_detect`.

## ESP-IDF Local Environment

The direct Homebrew EIM formula was not used for the working local path on this
machine. The verified runtime uses:

- EIM CLI release zip extracted to `/Users/localwork/06_System/tools/eim/eim`
- ESP-IDF v6.0 checked out at `/Users/localwork/06_System/tools/esp/v6.0/esp-idf`
- ESP-IDF Python venv at `/Users/zunwei/.espressif/tools/python/v6.0/venv`
- Homebrew expat at `/opt/homebrew/opt/expat/lib`

The hardware MCP local ESP-IDF wrapper now exports:

- `IDF_PYTHON_ENV_PATH`
- `DYLD_LIBRARY_PATH` when Homebrew expat is needed
- a temporary `sitecustomize.py` bootstrap through `PYTHONPATH`

The bootstrap fixes macOS Python child processes where `platform.mac_ver()` can
return an empty version when `DYLD_LIBRARY_PATH` is stripped by nested ESP-IDF
build tools.

## Scaffold Validation

Generated by `hardware_project_scaffold` under:

```text
dist/hardware-verify-20260603-103946
```

Audit and validation results from `make hardware-verify`:

- Every supported scaffold passed 5 artifact checks:
  - `README.md`
  - `hardware_manifest.json`
  - `docs/wiring.md`
  - `docs/verification.md`
  - `tests/hardware_checklist.md`
  - `hardware_manifest.json` includes structured `connections`, and
    `docs/wiring.md` includes the generated `Manifest 连接清单` table.
- Every supported scaffold passed `hardware_project_audit`:
  - Arduino: `10 passed, 0 failed, 0 warnings`
  - PlatformIO: `13 passed, 0 failed, 0 warnings`
  - ESP-IDF: `12 passed, 0 failed, 0 warnings`
  - MicroPython: `10 passed, 0 failed, 0 warnings`
  - Unihiker Python: `11 passed, 0 failed, 0 warnings`
  - MaixCAM Python: `11 passed, 0 failed, 0 warnings`
  - Raspberry Pi Python: `10 passed, 0 failed, 0 warnings`
- Every supported scaffold wrote one `hardware_evidence_record` entry after
  local validation:
  - `tests/hardware_evidence.jsonl`
  - `tests/hardware_checklist.md`
- Every supported scaffold returned `hardware_pending` from
  `hardware_evidence_status`, proving the tool does not confuse local validation
  with real-hardware completion when no board is connected.
- Every supported scaffold returned a `hardware_device_verify_plan` artifact:
  - Arduino: missing `port`
  - PlatformIO: missing `port`
  - ESP-IDF: missing `port`
  - MicroPython: missing `device`
  - Unihiker Python: missing `host`
  - MaixCAM Python: missing `host`
  - Raspberry Pi Python: missing `host`
  - SSH scaffolds keep editable default hosts in the command template, but those
    defaults no longer count as real-device readiness unless the user explicitly
    supplies `host`.
- The PlatformIO scaffold was edited after evidence recording; a second
  `hardware_evidence_status` call returned `stale`, proving old compile/upload
  evidence cannot verify changed source code.
- A PlatformIO weak-evidence regression recorded upload plus an empty monitor
  stage; `hardware_evidence_status` stayed `hardware_pending` and still reported
  missing `runtime_log`.
- A monitor evidence regression with only a command header now also stays
  `hardware_pending`; command headers are not treated as runtime logs.
- Arduino Uno scaffold:
  - `1 passed, 0 failed, 0 skipped`
  - compiled with `arduino-cli compile`
- PlatformIO ESP32 scaffold:
  - `1 passed, 0 failed, 0 skipped`
  - built with `pio run`
- ESP-IDF ESP32 scaffold:
  - `2 passed, 0 failed, 0 skipped`
  - `set-target esp32` passed
  - `idf.py build` passed through the local ESP-IDF wrapper
- MicroPython scaffold:
  - `1 passed, 0 failed, 0 skipped`
  - Python syntax check passed
- Unihiker Python scaffold:
  - `1 passed, 0 failed, 0 skipped`
  - Python syntax check passed
- MaixCAM Python scaffold:
  - `1 passed, 0 failed, 0 skipped`
  - Python syntax check passed
- Raspberry Pi Python scaffold:
  - `1 passed, 0 failed, 0 skipped`
  - Python syntax check passed

The summary artifact is:

```text
dist/hardware-verify-20260603-103946/summary.json
```

## ESP-IDF Official MCP

`esp_idf_mcp_config` generated Reasonix `.mcp.json` and `reasonix.toml`
snippets for the generated ESP-IDF scaffold.

The official ESP-IDF MCP server was started from the generated ESP-IDF project
with the Reasonix local ESP-IDF wrapper. Verified results:

- `list_tools` returned:
  - `build_project`
  - `set_target`
  - `flash_project`
  - `clean_project`
- MCP `call_tool("build_project")` returned:
  - `Successfully built project`

## No-Hardware Negative Validation

Generated under:

```text
dist/hardware-verify-20260603-103946
```

These checks verify that hardware-dependent tools fail fast and return enough
evidence for the AI agent to diagnose the next step when no board or SSH device
is connected.

- `arduino_upload` to `/dev/cu.reasonix_missing`:
  - returned `isError=true`
  - included the full `arduino-cli upload` command
  - included `avrdude: ser_open(): can't open device ... No such file or directory`
- `arduino_monitor_sample` on `/dev/cu.reasonix_missing`:
  - returned `isError=true`
  - included the full `arduino-cli monitor` command
  - reported `arduino monitor produced no serial output`
- `mpremote_run` on `/dev/cu.reasonix_missing`:
  - returned `isError=true`
  - included the full `mpremote connect ... run ...` command
  - reported the missing/in-use device error
- `mpremote_run` with a fake device command that produced no output:
  - returned `isError=true`
  - included the full `mpremote connect auto run ...` command
  - reported `mpremote produced no runtime output`
- `ssh_deploy_run` to `127.0.0.1:65000`:
  - returned `isError=true`
  - included non-interactive SSH/SCP options:
    `BatchMode=yes`, `StrictHostKeyChecking=accept-new`, `ConnectTimeout=2`
  - reported `Connection refused`
- `ssh_deploy_run` with fake `scp`/`ssh` commands that produced no remote output:
  - returned `isError=true`
  - included both the `scp` command and the remote `ssh` command
  - reported `ssh remote command produced no runtime output`

## Not Yet Verified

- Arduino upload and serial monitor with a real Arduino board.
- ESP32 upload and serial monitor with a real ESP32 board.
- ESP-IDF flash and monitor with a real ESP32 board.
- MicroPython `mpremote_run` against a real MicroPython board.
- SSH deploy/run against Unihiker, MaixCAM, or Raspberry Pi hardware.
- The newly added `scripts/windows-native-smoke.ps1` still needs an actual
  Windows runner/VM execution result in this branch to prove native process
  launch, silent install/uninstall, and WebView2 runtime behavior.

These items require connected hardware or a native Windows test machine.
