# Reasonix Hardware MCP

`reasonix-hardware-mcp` is a first-pass hardware programming tool server for
Reasonix. It adds structured tools for AI hardware teaching workflows:

- local toolchain and serial-port detection
- teaching-friendly project scaffolds
- AI-readable project completeness audits
- automatic no-flash project validation
- structured hardware verification evidence recording
- real-device verification plan generation with exact runner commands and
  missing inputs
- Arduino CLI compile/upload/short monitor capture
- PlatformIO run/upload/monitor targets
- ESP-IDF direct `idf.py` fallback tools
- ESP-IDF official Tools MCP config generation
- MicroPython `mpremote run`
- non-interactive SSH/SCP deploy-and-run for Unihiker, MaixCAM, and Raspberry Pi

## Build

```sh
make build
```

The hardware MCP binary is generated at:

```sh
bin/reasonix-hardware-mcp
```

## Configure Reasonix

The macOS desktop package installs the hardware-enabled app as
`/Applications/Reasonix.app`. In the desktop app, open **硬件** from the left
sidebar and click **启用**. The backend resolves the hardware MCP binary in this
order:

1. `REASONIX_HARDWARE_MCP`
2. bundled binary next to the desktop executable
3. `reasonix-hardware-mcp` on `PATH`
4. local `bin/reasonix-hardware-mcp` during development

Add this to `reasonix.toml`:

```toml
[[plugins]]
name = "hardware"
command = "bin/reasonix-hardware-mcp"
```

If `reasonix` is launched outside this repository, use the absolute path:

```toml
[[plugins]]
name = "hardware"
command = "/Users/localwork/06_System/reasonix_source/DeepSeek-Reasonix/bin/reasonix-hardware-mcp"
```

Then run:

```sh
reasonix chat
/mcp
```

## ESP-IDF official MCP

ESP-IDF v6.0+ includes Espressif's official local stdio Tools MCP server.
Use the hardware plugin to generate config:

```text
Call mcp__hardware__esp_idf_mcp_config with project_dir=/path/to/esp-idf-project
```

`esp_idf_mcp_config` chooses the strongest available launch path:

1. local ESP-IDF wrapper, when `REASONIX_ESP_IDF_PATH` or local search finds
   ESP-IDF and a matching Python environment.
2. EIM, when `use_eim=true` is requested or when EIM is available and no local
   ESP-IDF wrapper is found.
3. direct `idf.py -C <project> mcp-server`, when the shell environment already
   exposes `idf.py`.

On macOS the local wrapper also injects a temporary Python bootstrap and
`DYLD_LIBRARY_PATH` when Homebrew expat is present. This keeps nested ESP-IDF
CMake/Python child processes working in GUI-launched apps.

Useful environment overrides:

```sh
export REASONIX_ESP_IDF_PATH=/path/to/esp-idf
export REASONIX_IDF_PYTHON_ENV_PATH=/path/to/idf-python-venv
export REASONIX_IDF_PYTHON=/path/to/idf-python-venv/bin/python
export REASONIX_DYLD_LIBRARY_PATH=/opt/homebrew/opt/expat/lib
export REASONIX_EIM=/path/to/eim
```

Example EIM config:

```toml
[[plugins]]
name = "esp-idf-tools"
command = "eim"
args = ["run", "idf.py mcp-server"]
env = { IDF_MCP_WORKSPACE_FOLDER = "/path/to/project" }
```

If the ESP-IDF environment is already active, a direct config can also work:

```toml
[[plugins]]
name = "esp-idf-tools"
command = "idf.py"
args = ["-C", "/path/to/project", "mcp-server"]
```

## Slash Commands

Project commands are available under `.reasonix/commands/hardware/`:

- `/hardware:init`
- `/hardware:plan`
- `/hardware:debug`
- `/hardware:validate`
- `/hardware:review`

They are prompt workflows. The actual device operations are exposed through the
MCP tools, so they remain permission-gated by Reasonix.

## Automatic Validation

Use `hardware_project_audit` before `hardware_project_validate` to check whether
an existing project has the AI hardware context needed for reliable follow-up
work:

- `hardware_manifest.json` schema, platform, board, entrypoints, MCP tools, and
  verification command
- structured `connections` metadata for module role, protocol, pins, voltage,
  and notes
- `README.md`, `docs/wiring.md`, `docs/verification.md`, and
  `tests/hardware_checklist.md`
- platform/manifest consistency
- expected platform entrypoints
- connection risk checks for missing pins, 5V logic on 3.3V platforms, and
  duplicate GPIO/PWM/ADC/SPI/UART pin use

Example:

```text
Call mcp__hardware__hardware_project_audit with project_dir=/path/to/project
```

Use `hardware_project_validate` before flashing. It auto-detects the project type
and runs the strongest safe check available on the local machine:

- Arduino: `arduino-cli compile`
- PlatformIO: `pio run`
- ESP-IDF: `idf.py build` when `idf.py` is available, otherwise a skipped result
  with the next setup step
- Python/MicroPython/Unihiker/MaixCAM/Raspberry Pi: `python3 -m py_compile`

Example:

```text
Call mcp__hardware__hardware_project_validate with project_dir=/path/to/project
```

The output is structured JSON with `passed`, `failed`, or `skipped` results so
the agent can decide whether to fix code, install a toolchain, or ask for a real
board to be connected.

Use `hardware_evidence_record` after audit, compile/syntax checks, upload/flash,
serial monitor capture, `mpremote_run`, or `ssh_deploy_run`. It appends:

- `tests/hardware_evidence.jsonl`: one JSON record per verification step
- `tests/hardware_checklist.md`: a readable evidence entry for student defense

Each evidence record includes the current project fingerprint. The fingerprint is
computed from source, config, manifest, and documentation files while excluding
generated evidence files. This keeps old upload or serial logs traceable without
allowing them to prove that the latest code has been verified.

Runtime evidence is intentionally stricter than compile evidence. Stages such as
`monitor`, `serial`, `mpremote`, `ssh`, and `deploy` only satisfy
`hardware_evidence_status` when the record includes real output beyond command
headers. A manually recorded `monitor passed` line without pasted
serial/runtime output is kept for history, but it does not prove
`hardware_verified`. PlatformIO monitor, ESP-IDF monitor, `mpremote_run`, and
SSH remote commands also return an error when no runtime output is captured.

Example:

```text
Call mcp__hardware__hardware_evidence_record with stage=compile, status=passed, summary="pio run passed"
```

Use `hardware_evidence_status` after recording evidence. It reads
`tests/hardware_evidence.jsonl` and reports one of:

- `no_evidence`: no evidence has been recorded.
- `local_pending`: local compile/syntax evidence is incomplete.
- `hardware_pending`: local evidence is present, but real-device upload/deploy or
  runtime log evidence is missing.
- `hardware_verified`: local and real-device evidence are both present.
- `stale`: evidence exists, but it belongs to an older project fingerprint.
- `failed`: at least one evidence record failed.

Example:

```text
Call mcp__hardware__hardware_evidence_status with project_dir=/path/to/project
```

Use `hardware_device_verify_plan` before connecting or flashing real hardware.
It reads `hardware_manifest.json`, local toolchain status, detected serial
ports, and optional overrides such as `port`, `device`, or `host`. It returns:

- `localOnlyCommand`: a safe command for compile/syntax plus evidence smoke
- `realDeviceCommand`: the exact lab-runner command to execute after the board is
  connected
- `missingInputs`: values still needed before real-device verification
- `mcpSteps`: the equivalent MCP tool sequence for GUI/agent workflows

For SSH-based boards, scaffold defaults such as `10.1.2.3` or
`raspberrypi.local` stay in `realDeviceCommand` as editable examples, but they
still appear as missing `host` until the user explicitly supplies `host` or the
manifest contains a non-default target. This prevents the agent from treating an
example address as a confirmed reachable device.

Example:

```text
Call mcp__hardware__hardware_device_verify_plan with project_dir=/path/to/project
```

Every scaffold also includes the AI-readable project context needed for follow-up
hardware work:

- `hardware_manifest.json`: platform, board, entrypoints, structured
  `connections`, MCP tools, and local verification gate
- `docs/wiring.md`: wiring principles, platform defaults, voltage/protocol notes
- `docs/verification.md`: local validation, real-device validation, and failure
  triage
- `tests/hardware_checklist.md`: real-hardware and student defense checklist

For a full local regression of the first-version hardware platform layer, run:

```sh
make hardware-verify
```

This builds `bin/reasonix-hardware-mcp`, scaffolds every supported platform,
checks the common scaffold artifacts above, audits each generated project,
validates each generated project, records evidence for each validation, checks
the evidence status, and checks that hardware-dependent tools return diagnostic
errors when no board or SSH target is connected. Results are written under
`dist/hardware-verify-*`.

To confirm the hardware MCP itself still cross-compiles with the CLI release
matrix, run:

```sh
make cross
```

The `cross` target now builds both `reasonix-*` and
`reasonix-hardware-mcp-*` binaries for macOS, Linux, and Windows.

To verify the Windows installer packaging layer, run:

```sh
make windows-package-verify
```

This builds `dist/Reasonix-windows-amd64-installer.exe`, extracts it with
`7zz`, checks that `reasonix-desktop.exe` and `reasonix-hardware-mcp.exe` are
both present, verifies the hardware MCP Go metadata, and checks that the NSIS
script installs the hardware MCP into the per-user install directory. A native
Windows runner or VM is still required to verify process launch, silent
install/uninstall, and WebView2 runtime behavior.

On a native Windows runner or VM, run:

```powershell
scripts/windows-native-smoke.ps1 `
  -InstallerPath "dist/Reasonix-windows-amd64-installer.exe" `
  -SummaryPath "$env:RUNNER_TEMP/windows-native-smoke-summary.json"
```

This installs the NSIS package silently into a temporary per-user directory,
checks that `reasonix-desktop.exe` and `reasonix-hardware-mcp.exe` are present,
calls the bundled hardware MCP (`tools/list` and `hardware_detect`), launches the
desktop process briefly, uninstalls silently, and writes a JSON summary. The
desktop release workflow runs this automatically on the Windows build runner.

## Real Device Verification

After a board is connected, use `scripts/hardware-device-verify.sh` to run the
same evidence chain against real hardware. The script performs:

Start by calling `hardware_device_verify_plan`; copy its `realDeviceCommand`
after filling any `missingInputs`. The script performs:

1. `hardware_project_audit`
2. `hardware_project_validate`
3. `hardware_evidence_record` for local compile or syntax evidence
4. upload/flash/deploy/run through the platform tool
5. `hardware_evidence_record` for the real-device stage
6. `hardware_evidence_status`

The command exits successfully only when the final status is
`hardware_verified`, unless `--local-only` is used.

Examples:

```sh
# Arduino Uno
make hardware-device-verify ARGS="--platform arduino \
  --project-dir /path/to/arduino_project \
  --board uno \
  --fqbn arduino:avr:uno \
  --port /dev/cu.usbserial-xxxx"

# ESP32 with PlatformIO
make hardware-device-verify ARGS="--platform platformio \
  --project-dir /path/to/platformio_project \
  --board esp32dev \
  --environment esp32dev \
  --port /dev/cu.usbserial-xxxx"

# ESP-IDF
make hardware-device-verify ARGS="--platform esp_idf \
  --project-dir /path/to/esp-idf-project \
  --target esp32 \
  --port /dev/cu.usbserial-xxxx"

# MicroPython
make hardware-device-verify ARGS="--platform micropython \
  --project-dir /path/to/micropython_project \
  --script /path/to/micropython_project/src/main.py \
  --device /dev/cu.usbserial-xxxx"

# Unihiker / MaixCAM / Raspberry Pi over SSH
make hardware-device-verify ARGS="--platform unihiker_python \
  --project-dir /path/to/unihiker_project \
  --host 10.1.2.3 \
  --user root \
  --remote-path /root/reasonix/unihiker_project \
  --remote-command 'python3 /root/reasonix/unihiker_project/main.py'"
```

For a no-device smoke check of the local evidence path, add `--local-only`. The
expected final status in that mode is `hardware_pending`, with the missing real
device stage groups reported explicitly.

## Hardware-Dependent Tool Behavior

Tools that need a real device return MCP `isError=true` with the command output
included when the board or remote host is missing. This is intentional: the agent
should diagnose from the real CLI output instead of guessing.

- `arduino_monitor_sample` treats a timeout as success only when serial output was
  actually captured. A missing or silent port returns an error.
- `mpremote_run` requires runtime output by default. Use `require_output=false`
  only for explicit background-only runs.
- `ssh_deploy_run` uses non-interactive SSH/SCP defaults:
  - `BatchMode=yes`
  - `StrictHostKeyChecking=accept-new`
  - `ConnectTimeout=<seconds>`
  - `ServerAliveInterval=5`
  - `ServerAliveCountMax=1`
- `ssh_deploy_run` supports `ssh_port`, `identity_file`, and `connect_timeout`
  for classroom boards with custom network setups.
- `ssh_deploy_run` requires remote command output by default when `command` is
  provided. Use `require_output=false` only for deploy-only commands.

## Packaging Notes

The desktop release packages the hardware MCP next to the desktop executable:

- macOS: `Reasonix.app/Contents/MacOS/reasonix-hardware-mcp`
- Linux: `reasonix-hardware-mcp` in the release tarball
- Windows: `reasonix-hardware-mcp.exe` installed into `$INSTDIR` by NSIS

## First-Version Scope

This version intentionally does not replace Arduino IDE, PlatformIO IDE, MaixVision,
or ESP-IDF. It wraps their command-line or MCP interfaces so an AI agent can work
with real hardware evidence instead of only editing code.

Current validation evidence is tracked in `docs/HARDWARE_VALIDATION.md`.

## References

- ESP-IDF `idf.py` and MCP server:
  https://docs.espressif.com/projects/esp-idf/en/latest/esp32/api-guides/tools/idf-py.html
- Espressif ESP-IDF Tools local MCP server article:
  https://developer.espressif.com/blog/2026/04/esp-idf-tools-mcp-server/
- Arduino CLI commands:
  https://docs.arduino.cc/arduino-cli/commands-reference/
- PlatformIO Core:
  https://docs.platformio.org/en/latest/core/
- MicroPython mpremote:
  https://docs.micropython.org/en/latest/reference/mpremote.html
