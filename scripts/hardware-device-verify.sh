#!/usr/bin/env bash
# Verify a single real hardware project through the Reasonix hardware MCP.
#
# This script is intentionally separate from hardware-verify.sh:
# - hardware-verify.sh is the no-device regression suite.
# - hardware-device-verify.sh is the lab runner for a connected board.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_DIR="${REASONIX_HARDWARE_DEVICE_VERIFY_DIR:-$ROOT/dist/hardware-device-verify-$(date +%Y%m%d-%H%M%S)}"
MCP="${REASONIX_HARDWARE_MCP:-$ROOT/bin/onecreat-hardware-mcp}"

if [[ ! -x "$MCP" ]]; then
	echo "==> building hardware MCP -> $MCP"
	mkdir -p "$(dirname "$MCP")"
	CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=hardware-device-verify" \
		-o "$MCP" "$ROOT/cmd/reasonix-hardware-mcp"
fi

mkdir -p "$RUN_DIR"

python3 - "$ROOT" "$RUN_DIR" "$MCP" "$@" <<'PY'
import argparse
import json
import pathlib
import re
import subprocess
import sys

root = pathlib.Path(sys.argv[1]).resolve()
run_dir = pathlib.Path(sys.argv[2]).resolve()
mcp = pathlib.Path(sys.argv[3]).resolve()
argv = sys.argv[4:]

PLATFORMS = (
    "arduino",
    "platformio",
    "esp_idf",
    "micropython",
    "unihiker_python",
    "maixcam_python",
    "raspberry_pi_python",
)


class ToolError(RuntimeError):
    def __init__(self, tool, text):
        super().__init__(f"{tool} failed")
        self.tool = tool
        self.text = text


def parse_args():
    parser = argparse.ArgumentParser(
        description="Run local validation plus real-device upload/deploy/log evidence for one hardware project."
    )
    parser.add_argument("--platform", required=True, choices=PLATFORMS)
    parser.add_argument("--project-dir", required=True)
    parser.add_argument("--board", default="")
    parser.add_argument("--fqbn", default="")
    parser.add_argument("--environment", default="")
    parser.add_argument("--target", default="")
    parser.add_argument("--port", default="")
    parser.add_argument("--baud", type=int, default=115200)
    parser.add_argument("--monitor-seconds", type=int, default=8)
    parser.add_argument("--timeout-seconds", type=int, default=180)
    parser.add_argument("--local-only", action="store_true", help="Stop after audit/local validation/evidence status.")

    parser.add_argument("--sketch-dir", default="", help="Arduino sketch directory. Defaults to the first .ino parent.")
    parser.add_argument("--script", default="", help="MicroPython script. Defaults to src/main.py or the first .py file.")
    parser.add_argument("--device", default="", help="mpremote device. Defaults to auto when omitted.")

    parser.add_argument("--host", default="", help="SSH target host for Unihiker/MaixCAM/RPi.")
    parser.add_argument("--user", default="root")
    parser.add_argument("--ssh-port", type=int, default=22)
    parser.add_argument("--identity-file", default="")
    parser.add_argument("--local-path", default="", help="Local file/directory for SSH deployment. Defaults to src/ or project dir.")
    parser.add_argument("--remote-path", default="", help="Remote destination. Defaults to /root/reasonix/<project-name>.")
    parser.add_argument("--remote-command", default="", help="Remote command. Defaults to python3 <remote>/main.py when possible.")
    parser.add_argument("--recursive", action="store_true", help="Force recursive SSH copy.")
    parser.add_argument("--allow-local-skips", action="store_true")
    return parser.parse_args(argv)


args = parse_args()
project_dir = pathlib.Path(args.project_dir).expanduser().resolve()
if not project_dir.exists():
    raise SystemExit(f"project directory does not exist: {project_dir}")
run_dir.mkdir(parents=True, exist_ok=True)

summary = {
    "runDir": str(run_dir),
    "projectDir": str(project_dir),
    "platform": args.platform,
    "board": args.board,
    "artifacts": [],
    "records": [],
    "finalStatus": None,
}


def rel(path):
    path = pathlib.Path(path)
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


def artifact_name(name):
    safe = re.sub(r"[^A-Za-z0-9_.-]+", "_", name).strip("_")
    return safe or "artifact"


def call_tool(tool, tool_args, artifact, expect_error=False):
    req = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": tool, "arguments": tool_args},
    }
    timeout = max(30, int(tool_args.get("timeout_seconds", args.timeout_seconds)) + 30)
    proc = subprocess.run(
        [str(mcp)],
        input=json.dumps(req, ensure_ascii=False) + "\n",
        text=True,
        capture_output=True,
        timeout=timeout,
        cwd=root,
    )
    raw_path = run_dir / artifact
    raw_path.write_text(proc.stdout + ("\n--- stderr ---\n" + proc.stderr if proc.stderr else ""), encoding="utf-8")
    summary["artifacts"].append(rel(raw_path))
    if proc.returncode != 0:
        raise RuntimeError(f"{tool} exited {proc.returncode}: {proc.stderr}")
    line = next((line for line in proc.stdout.splitlines() if line.strip()), "")
    data = json.loads(line)
    if data.get("error"):
        raise RuntimeError(f"{tool} RPC error: {data['error']}")
    result = data["result"]
    is_error = bool(result.get("isError"))
    text = result.get("content", [{}])[0].get("text", "")
    if is_error != expect_error:
        if is_error:
            raise ToolError(tool, text)
        raise AssertionError(f"{tool} expected isError={expect_error}, got {is_error}")
    return text, raw_path


def call_json(tool, tool_args, artifact):
    text, raw_path = call_tool(tool, tool_args, artifact)
    return json.loads(text), raw_path


def evidence_args(stage, status, summary_text, command="", output="", port="", artifact_path=""):
    out = {
        "project_dir": str(project_dir),
        "platform": args.platform,
        "board": args.board,
        "stage": stage,
        "status": status,
        "summary": summary_text,
        "command": command,
        "output": output[-12000:],
        "port": port,
        "artifact_path": artifact_path,
        "allow_outside_workspace": True,
    }
    return out


def record_evidence(stage, status, summary_text, command="", output="", port="", artifact_path=""):
    data, raw_path = call_json(
        "hardware_evidence_record",
        evidence_args(stage, status, summary_text, command, output, port, artifact_path),
        f"evidence_{artifact_name(stage)}_{artifact_name(status)}.json",
    )
    summary["records"].append(
        {
            "stage": stage,
            "status": status,
            "summary": summary_text,
            "artifact": rel(raw_path),
        }
    )
    print(f"  evidence {stage}: {status} - {summary_text}")
    return data


def evidence_status(label):
    status, raw_path = call_json(
        "hardware_evidence_status",
        {"project_dir": str(project_dir), "platform": args.platform, "allow_outside_workspace": True},
        f"evidence_status_{artifact_name(label)}.json",
    )
    print(f"  evidence status after {label}: {status['status']}")
    if status.get("missingGroups"):
        print("  missing groups: " + ", ".join(status["missingGroups"]))
    return status, raw_path


def first_command_result(report):
    for item in report.get("results", []):
        if item.get("command") or item.get("output"):
            return item
    return report.get("results", [{}])[0] if report.get("results") else {}


def default_fqbn(board):
    board = (board or "").lower()
    if board in ("uno", "arduino_uno"):
        return "arduino:avr:uno"
    if board in ("nano", "arduino_nano"):
        return "arduino:avr:nano"
    if board in ("mega", "mega2560", "arduino_mega"):
        return "arduino:avr:mega:cpu=atmega2560"
    if board in ("esp32", "esp32dev", "esp32_devkit"):
        return "esp32:esp32:esp32"
    if board in ("esp32s3", "esp32-s3"):
        return "esp32:esp32:esp32s3"
    return ""


def find_arduino_sketch_dir():
    if args.sketch_dir:
        return pathlib.Path(args.sketch_dir).expanduser().resolve()
    matches = sorted(project_dir.rglob("*.ino"))
    if not matches:
        raise SystemExit("Arduino project needs --sketch-dir or a .ino file under --project-dir.")
    return matches[0].parent


def infer_platformio_env():
    if args.environment:
        return args.environment
    ini = project_dir / "platformio.ini"
    if not ini.exists():
        return ""
    for line in ini.read_text(encoding="utf-8", errors="ignore").splitlines():
        line = line.strip()
        if line.startswith("[env:") and line.endswith("]"):
            return line[5:-1]
    return ""


def find_python_script():
    if args.script:
        return pathlib.Path(args.script).expanduser().resolve()
    preferred = project_dir / "src" / "main.py"
    if preferred.exists():
        return preferred
    matches = sorted(p for p in project_dir.rglob("*.py") if ".venv" not in p.parts and "__pycache__" not in p.parts)
    if not matches:
        raise SystemExit("Python device verification needs --script or a .py file under --project-dir.")
    return matches[0]


def ssh_defaults():
    local_path = pathlib.Path(args.local_path).expanduser().resolve() if args.local_path else None
    if local_path is None:
        src = project_dir / "src"
        local_path = src if src.exists() else project_dir
    remote_path = args.remote_path or f"/root/reasonix/{project_dir.name}"
    recursive = args.recursive or local_path.is_dir()
    if args.remote_command:
        command = args.remote_command
    elif local_path.is_dir():
        command = f"python3 {remote_path}/main.py"
    else:
        command = f"python3 {remote_path}"
    return local_path, remote_path, recursive, command


def validate_locally():
    print("==> audit project")
    audit, _ = call_json(
        "hardware_project_audit",
        {"project_dir": str(project_dir), "platform": args.platform},
        "audit.json",
    )
    failed = [item for item in audit.get("results", []) if item.get("status") == "failed"]
    if failed:
        raise SystemExit("hardware_project_audit failed; see audit.json")

    print("==> local validate")
    validate_args = {
        "project_dir": str(project_dir),
        "platform": args.platform,
        "board": args.board,
        "timeout_seconds": args.timeout_seconds,
    }
    if args.fqbn:
        validate_args["fqbn"] = args.fqbn
    if args.environment:
        validate_args["environment"] = args.environment
    if args.target:
        validate_args["target"] = args.target

    report, _ = call_json("hardware_project_validate", validate_args, "validate.json")
    bad = [
        item
        for item in report.get("results", [])
        if item.get("status") == "failed" or (item.get("status") == "skipped" and not args.allow_local_skips)
    ]
    local_stage = "compile" if args.platform in ("arduino", "platformio", "esp_idf") else "syntax"
    item = first_command_result(report)
    status = "passed" if not bad else "failed"
    record_evidence(
        local_stage,
        status,
        report.get("summary", "local validation finished"),
        item.get("command", "hardware_project_validate"),
        item.get("output", ""),
        args.port,
        rel(run_dir / "validate.json"),
    )
    if bad:
        raise SystemExit("local validation failed or skipped; see validate.json")
    evidence_status("local")


def run_and_record(tool, tool_args, stage, summary_text, port=""):
    artifact = f"{artifact_name(stage)}_{artifact_name(tool)}.json"
    try:
        text, raw_path = call_tool(tool, tool_args, artifact)
    except ToolError as exc:
        record_evidence(stage, "failed", summary_text, f"mcp:{tool}", exc.text, port)
        raise
    record_evidence(stage, "passed", summary_text, f"mcp:{tool}", text, port, rel(raw_path))
    return text


def verify_arduino():
    fqbn = args.fqbn or default_fqbn(args.board)
    if not fqbn:
        raise SystemExit("Arduino verification needs --fqbn or a known --board such as uno/nano/esp32.")
    if not args.port:
        raise SystemExit("Arduino verification needs --port, for example /dev/cu.usbserial-xxxx.")
    sketch_dir = find_arduino_sketch_dir()
    run_and_record(
        "arduino_upload",
        {"sketch_dir": str(sketch_dir), "fqbn": fqbn, "port": args.port, "timeout_seconds": args.timeout_seconds},
        "upload",
        "Arduino sketch uploaded to the connected board",
        args.port,
    )
    run_and_record(
        "arduino_monitor_sample",
        {"port": args.port, "fqbn": fqbn, "baud": args.baud, "seconds": args.monitor_seconds},
        "monitor",
        "Arduino serial monitor produced runtime output",
        args.port,
    )


def verify_platformio():
    if not args.port:
        raise SystemExit("PlatformIO verification needs --port, for example /dev/cu.usbserial-xxxx.")
    env = infer_platformio_env()
    base = {"project_dir": str(project_dir), "timeout_seconds": args.timeout_seconds}
    if env:
        base["environment"] = env
    run_and_record(
        "platformio_run",
        {**base, "targets": ["upload"], "upload_port": args.port},
        "upload",
        "PlatformIO firmware uploaded to the connected board",
        args.port,
    )
    run_and_record(
        "platformio_run",
        {**base, "targets": ["monitor"], "monitor_port": args.port, "timeout_seconds": args.monitor_seconds + 10},
        "monitor",
        "PlatformIO monitor produced runtime output",
        args.port,
    )


def verify_esp_idf():
    if not args.port:
        raise SystemExit("ESP-IDF verification needs --port, for example /dev/cu.usbserial-xxxx.")
    base = {"project_dir": str(project_dir), "port": args.port, "baud": args.baud, "timeout_seconds": args.timeout_seconds}
    run_and_record(
        "esp_idf_run",
        {**base, "action": "flash"},
        "flash",
        "ESP-IDF firmware flashed to the connected ESP32 board",
        args.port,
    )
    run_and_record(
        "esp_idf_run",
        {**base, "action": "monitor", "timeout_seconds": args.monitor_seconds + 10},
        "monitor",
        "ESP-IDF monitor produced runtime output",
        args.port,
    )


def verify_micropython():
    script = find_python_script()
    tool_args = {"script": str(script), "timeout_seconds": args.timeout_seconds}
    if args.device:
        tool_args["device"] = args.device
    run_and_record(
        "mpremote_run",
        tool_args,
        "mpremote",
        "MicroPython script ran on the connected board",
        args.device or "auto",
    )


def verify_ssh_device():
    if not args.host:
        raise SystemExit(f"{args.platform} verification needs --host for SSH deployment.")
    local_path, remote_path, recursive, command = ssh_defaults()
    tool_args = {
        "host": args.host,
        "user": args.user,
        "ssh_port": args.ssh_port,
        "identity_file": args.identity_file,
        "local_path": str(local_path),
        "remote_path": remote_path,
        "recursive": recursive,
        "command": command,
        "timeout_seconds": args.timeout_seconds,
    }
    run_and_record(
        "ssh_deploy_run",
        tool_args,
        "deploy",
        f"{args.platform} code deployed over SSH and command ran",
        args.host,
    )


print(f"==> hardware device verification -> {run_dir}")
print(f"  project: {project_dir}")
print(f"  platform: {args.platform}")

validate_locally()

if args.local_only:
    print("==> local-only mode; real-device stages were not executed")
else:
    print("==> real-device stage")
    if args.platform == "arduino":
        verify_arduino()
    elif args.platform == "platformio":
        verify_platformio()
    elif args.platform == "esp_idf":
        verify_esp_idf()
    elif args.platform == "micropython":
        verify_micropython()
    elif args.platform in ("unihiker_python", "maixcam_python", "raspberry_pi_python"):
        verify_ssh_device()
    else:
        raise SystemExit(f"unsupported platform: {args.platform}")

final_status, raw_path = evidence_status("final")
summary["finalStatus"] = final_status
summary_path = run_dir / "summary.json"
summary_path.write_text(json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8")

print(f"summary: {summary_path}")
if not args.local_only and final_status.get("status") != "hardware_verified":
    raise SystemExit("real-device verification did not reach hardware_verified")
PY
