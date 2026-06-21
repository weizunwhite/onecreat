#!/usr/bin/env bash
# End-to-end verification for Reasonix's hardware MCP layer.
#
# It scaffolds every supported first-version platform, validates each project,
# and checks that hardware-dependent tools fail with diagnostic output when no
# board or SSH target is connected.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
RUN_DIR="${REASONIX_HARDWARE_VERIFY_DIR:-$ROOT/dist/hardware-verify-$(date +%Y%m%d-%H%M%S)}"
MCP="$ROOT/bin/onecreat-hardware-mcp"

mkdir -p "$RUN_DIR"

echo "==> building hardware MCP -> $MCP"
CGO_ENABLED=0 go build -ldflags "-s -w -X main.version=hardware-verify" \
	-o "$MCP" "$ROOT/cmd/reasonix-hardware-mcp"

echo "==> running hardware verification -> $RUN_DIR"
python3 - "$ROOT" "$RUN_DIR" "$MCP" <<'PY'
import json
import os
import pathlib
import subprocess
import sys

root = pathlib.Path(sys.argv[1])
run_dir = pathlib.Path(sys.argv[2])
mcp = pathlib.Path(sys.argv[3])
allow_skips = bool(int(os.environ.get("REASONIX_HARDWARE_VERIFY_ALLOW_SKIPS", "0")))
run_dir.mkdir(parents=True, exist_ok=True)

summary = {
    "runDir": str(run_dir),
    "scaffolds": [],
    "audits": [],
    "validations": [],
    "evidence": [],
    "evidenceStatus": [],
    "devicePlans": [],
    "weakEvidence": [],
    "staleEvidence": [],
    "negative": [],
}


def rel(path):
    path = pathlib.Path(path)
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


def call_tool(tool, args, artifact, expect_error=False, env=None):
    req = {
        "jsonrpc": "2.0",
        "id": 1,
        "method": "tools/call",
        "params": {"name": tool, "arguments": args},
    }
    proc = subprocess.run(
        [str(mcp)],
        input=json.dumps(req, ensure_ascii=False) + "\n",
        text=True,
        capture_output=True,
        timeout=max(30, int(args.get("timeout_seconds", 30)) + 20),
        cwd=root,
        env=env,
    )
    raw_path = run_dir / artifact
    raw_path.write_text(proc.stdout, encoding="utf-8")
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
        raise AssertionError(f"{tool} expected isError={expect_error}, got {is_error}. Output:\n{text[:2000]}")
    return text, raw_path


def validate_project(name, platform, extra=None):
    args = {"project_dir": str(run_dir / name), "platform": platform, "timeout_seconds": 300}
    if extra:
        args.update(extra)
    text, raw_path = call_tool("hardware_project_validate", args, f"{platform}_validate.json")
    report = json.loads(text)
    bad = [item for item in report["results"] if item["status"] == "failed" or (item["status"] == "skipped" and not allow_skips)]
    entry = {
        "platform": platform,
        "project": name,
        "summary": report["summary"],
        "artifact": rel(raw_path),
    }
    summary["validations"].append(entry)
    print(f"  {platform}: {report['summary']}")
    if bad:
        raise AssertionError(f"{platform} validation not complete: {json.dumps(bad, ensure_ascii=False, indent=2)[:2000]}")
    return report


def audit_project(name, platform):
    text, raw_path = call_tool(
        "hardware_project_audit",
        {"project_dir": str(run_dir / name), "platform": platform},
        f"{platform}_audit.json",
    )
    report = json.loads(text)
    bad = [item for item in report["results"] if item["status"] == "failed"]
    entry = {
        "platform": platform,
        "project": name,
        "summary": report["summary"],
        "artifact": rel(raw_path),
    }
    summary["audits"].append(entry)
    print(f"  audit {platform}: {report['summary']}")
    if bad:
        raise AssertionError(f"{platform} audit failed: {json.dumps(bad, ensure_ascii=False, indent=2)[:2000]}")


def assert_scaffold_artifacts(name, platform, board):
    project_dir = run_dir / name
    required = {
        "README.md": ["教学要求", "验证命令", "hardware_manifest.json"],
        "hardware_manifest.json": ["reasonix-hardware-project/v1", platform, board, "connections", "hardware_project_validate", "hardware_device_verify_plan"],
        "docs/wiring.md": ["UART", "I2C", "hardware_manifest.json", "Manifest 连接清单"],
        "docs/verification.md": ["hardware_detect", "hardware_device_verify_plan", "hardware_project_validate", "hardware_evidence_record", "失败判断"],
        "tests/hardware_checklist.md": ["真实硬件验证", "学生答辩检查", "hardware_project_validate", "hardware_evidence_record"],
    }
    for rel_path, needles in required.items():
        path = project_dir / rel_path
        if not path.exists():
            raise AssertionError(f"{platform} scaffold missing {rel_path}")
        body = path.read_text(encoding="utf-8")
        missing = [needle for needle in needles if needle not in body]
        if missing:
            raise AssertionError(f"{platform} {rel_path} missing {missing}.\n{body[:2000]}")

    manifest = json.loads((project_dir / "hardware_manifest.json").read_text(encoding="utf-8"))
    if manifest["platform"] != platform or manifest["board"] != board:
        raise AssertionError(f"{platform} manifest metadata mismatch: {manifest}")
    if "hardware_detect" not in manifest["mcpTools"] or "hardware_project_audit" not in manifest["mcpTools"] or "hardware_project_validate" not in manifest["mcpTools"] or "hardware_evidence_record" not in manifest["mcpTools"] or "hardware_evidence_status" not in manifest["mcpTools"] or "hardware_device_verify_plan" not in manifest["mcpTools"]:
        raise AssertionError(f"{platform} manifest missing MCP tools: {manifest}")
    if manifest["verification"]["minimumLocalGate"] != "compile_or_syntax":
        raise AssertionError(f"{platform} manifest has wrong local gate: {manifest}")
    connections = manifest.get("connections", [])
    if not connections:
        raise AssertionError(f"{platform} manifest missing structured connections: {manifest}")
    for index, conn in enumerate(connections):
        missing = [field for field in ("name", "role", "protocol", "voltage") if not str(conn.get(field, "")).strip()]
        if missing:
            raise AssertionError(f"{platform} connection {index} missing {missing}: {conn}")
        if conn.get("protocol") not in {"USB", "USB_SERIAL", "USB_OR_WIFI", "USB_C_OR_GPIO_POWER", "GPIO", "UART", "I2C", "SPI", "CSI", "INTERNAL", "WiFi", "MQTT", "HTTP"} and not conn.get("pins"):
            raise AssertionError(f"{platform} connection {index} has no pins for protocol {conn.get('protocol')}: {conn}")


platforms = [
    ("arduino", "arduino_verify", "uno", {"fqbn": "arduino:avr:uno"}),
    ("platformio", "platformio_verify", "esp32dev", {"environment": "esp32dev"}),
    ("esp_idf", "espidf_verify", "esp32", {"target": "esp32"}),
    ("micropython", "micropython_verify", "esp32", {}),
    ("unihiker_python", "unihiker_verify", "unihiker", {}),
    ("maixcam_python", "maixcam_verify", "maixcam", {}),
    ("raspberry_pi_python", "rpi_verify", "raspberry_pi", {}),
]

for platform, name, board, extra in platforms:
    text, raw_path = call_tool(
        "hardware_project_scaffold",
        {"project_name": name, "project_dir": str(run_dir / name), "platform": platform, "board": board, "overwrite": True},
        f"{platform}_scaffold.json",
    )
    assert_scaffold_artifacts(name, platform, board)
    summary["scaffolds"].append({"platform": platform, "project": name, "artifact": rel(raw_path), "artifactChecks": 5})
    audit_project(name, platform)
    validation_report = validate_project(name, platform, extra)
    command = next((item.get("command", "") for item in validation_report["results"] if item.get("command")), "")
    text, raw_path = call_tool(
        "hardware_evidence_record",
        {
            "project_dir": str(run_dir / name),
            "platform": platform,
            "board": board,
            "stage": "compile" if platform not in {"micropython", "unihiker_python", "maixcam_python", "raspberry_pi_python"} else "syntax",
            "status": "passed",
            "summary": validation_report["summary"],
            "command": command,
            "output": validation_report["results"][0].get("output", "") if validation_report["results"] else "",
        },
        f"{platform}_evidence_record.json",
    )
    evidence_report = json.loads(text)
    if not evidence_report["record"].get("projectFingerprint"):
        raise AssertionError(f"{platform} evidence record is missing projectFingerprint")
    evidence_jsonl = run_dir / name / "tests" / "hardware_evidence.jsonl"
    checklist = run_dir / name / "tests" / "hardware_checklist.md"
    if not evidence_jsonl.exists() or platform not in evidence_jsonl.read_text(encoding="utf-8"):
        raise AssertionError(f"{platform} evidence JSONL was not written correctly")
    if not checklist.exists() or "验证证据记录" not in checklist.read_text(encoding="utf-8"):
        raise AssertionError(f"{platform} checklist evidence entry was not written correctly")
    summary["evidence"].append({
        "platform": platform,
        "project": name,
        "summary": evidence_report["record"]["summary"],
        "artifact": rel(raw_path),
    })
    print(f"  evidence {platform}: recorded")
    text, raw_path = call_tool(
        "hardware_evidence_status",
        {"project_dir": str(run_dir / name), "platform": platform},
        f"{platform}_evidence_status.json",
    )
    status_report = json.loads(text)
    if status_report["status"] != "hardware_pending":
        raise AssertionError(f"{platform} evidence status should be hardware_pending before real device checks: {json.dumps(status_report, ensure_ascii=False, indent=2)[:2000]}")
    summary["evidenceStatus"].append({
        "platform": platform,
        "project": name,
        "status": status_report["status"],
        "missingGroups": status_report["missingGroups"],
        "artifact": rel(raw_path),
    })
    print(f"  evidence status {platform}: {status_report['status']}")
    text, raw_path = call_tool(
        "hardware_device_verify_plan",
        {"project_dir": str(run_dir / name), "platform": platform, "board": board},
        f"{platform}_device_verify_plan.json",
    )
    plan_report = json.loads(text)
    if not plan_report.get("realDeviceCommand") or not plan_report.get("localOnlyCommand"):
        raise AssertionError(f"{platform} plan missing commands: {json.dumps(plan_report, ensure_ascii=False, indent=2)[:2000]}")
    if not any(step.get("tool") == "hardware_evidence_status" for step in plan_report.get("mcpSteps", [])):
        raise AssertionError(f"{platform} plan missing evidence status step: {json.dumps(plan_report, ensure_ascii=False, indent=2)[:2000]}")
    if platform in {"unihiker_python", "maixcam_python", "raspberry_pi_python"}:
        if "host" not in plan_report.get("missingInputs", []):
            raise AssertionError(f"{platform} SSH plan should require an explicit host, not trust scaffold defaults: {json.dumps(plan_report, ensure_ascii=False, indent=2)[:2000]}")
        if plan_report.get("readyForRealDevice"):
            raise AssertionError(f"{platform} SSH plan should not be ready before a host is explicitly supplied: {json.dumps(plan_report, ensure_ascii=False, indent=2)[:2000]}")
    summary["devicePlans"].append({
        "platform": platform,
        "project": name,
        "readyForRealDevice": plan_report.get("readyForRealDevice"),
        "missingInputs": plan_report.get("missingInputs", []),
        "artifact": rel(raw_path),
    })
    print(f"  device plan {platform}: {', '.join(plan_report.get('missingInputs', [])) or 'ready'}")

for index, (stage, command, output) in enumerate([
    ("upload", "pio run -e esp32dev -t upload --upload-port /dev/cu.fake", "upload command completed"),
    ("monitor", "pio run -e esp32dev -t monitor --monitor-port /dev/cu.fake", ""),
    ("monitor", "pio run -e esp32dev -t monitor --monitor-port /dev/cu.fake", "$ pio run -e esp32dev -t monitor --monitor-port /dev/cu.fake\n"),
]):
    call_tool(
        "hardware_evidence_record",
        {
            "project_dir": str(run_dir / "platformio_verify"),
            "platform": "platformio",
            "board": "esp32dev",
            "stage": stage,
            "status": "passed",
            "summary": f"weak evidence regression: {stage}",
            "command": command,
            "output": output,
        },
        f"platformio_weak_{index}_{stage}_evidence_record.json",
    )
text, raw_path = call_tool(
    "hardware_evidence_status",
    {"project_dir": str(run_dir / "platformio_verify"), "platform": "platformio"},
    "platformio_weak_monitor_evidence_status.json",
)
weak_report = json.loads(text)
if weak_report["status"] != "hardware_pending" or "runtime_log" not in weak_report.get("missingGroups", []):
    raise AssertionError(f"platformio empty monitor evidence must not verify runtime logs: {json.dumps(weak_report, ensure_ascii=False, indent=2)[:2000]}")
summary["weakEvidence"].append({
    "platform": "platformio",
    "project": "platformio_verify",
    "status": weak_report["status"],
    "missingGroups": weak_report["missingGroups"],
    "artifact": rel(raw_path),
})
print("  weak evidence platformio: runtime_log still missing")

stale_file = run_dir / "platformio_verify" / "src" / "main.cpp"
stale_file.write_text(stale_file.read_text(encoding="utf-8") + "\n// Reasonix stale evidence regression marker.\n", encoding="utf-8")
text, raw_path = call_tool(
    "hardware_evidence_status",
    {"project_dir": str(run_dir / "platformio_verify"), "platform": "platformio"},
    "platformio_stale_evidence_status.json",
)
stale_report = json.loads(text)
if stale_report["status"] != "stale" or stale_report["currentRecordCount"] != 0 or stale_report["staleRecordCount"] == 0:
    raise AssertionError(f"platformio stale evidence status should be stale after source edit: {json.dumps(stale_report, ensure_ascii=False, indent=2)[:2000]}")
summary["staleEvidence"].append({
    "platform": "platformio",
    "project": "platformio_verify",
    "status": stale_report["status"],
    "currentRecordCount": stale_report["currentRecordCount"],
    "staleRecordCount": stale_report["staleRecordCount"],
    "artifact": rel(raw_path),
})
print("  stale evidence platformio: stale")

negative_script = run_dir / "negative_main.py"
negative_script.write_text('print("hello from negative verification")\n', encoding="utf-8")
fake_bin = run_dir / "fake-bin"
fake_bin.mkdir(exist_ok=True)


def write_fake_command(name, body):
    path = fake_bin / name
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


write_fake_command("mpremote", "#!/bin/sh\nexit 0\n")
write_fake_command("scp", "#!/bin/sh\nexit 0\n")
write_fake_command("ssh", "#!/bin/sh\nexit 0\n")
fake_env = os.environ.copy()
fake_env["PATH"] = str(fake_bin) + os.pathsep + fake_env.get("PATH", "")

negative_cases = [
    (
        "arduino_upload_missing_port",
        "arduino_upload",
        {
            "sketch_dir": str(run_dir / "arduino_verify" / "arduino_verify"),
            "fqbn": "arduino:avr:uno",
            "port": "/dev/cu.reasonix_missing",
            "timeout_seconds": 8,
        },
        ["$ arduino-cli upload", "error: arduino-cli failed"],
    ),
    (
        "arduino_monitor_missing_port",
        "arduino_monitor_sample",
        {"port": "/dev/cu.reasonix_missing", "baud": 115200, "seconds": 4},
        ["$ arduino-cli monitor", "no serial output"],
    ),
    (
        "mpremote_missing_device",
        "mpremote_run",
        {"script": str(negative_script), "device": "/dev/cu.reasonix_missing", "timeout_seconds": 8},
        ["$ mpremote connect", "error: mpremote failed"],
    ),
    (
        "ssh_unreachable",
        "ssh_deploy_run",
        {
            "host": "127.0.0.1",
            "user": "root",
            "ssh_port": 65000,
            "local_path": str(negative_script),
            "remote_path": "/root/reasonix/main.py",
            "command": "python3 /root/reasonix/main.py",
            "connect_timeout": 2,
            "timeout_seconds": 8,
        },
        ["$ scp", "BatchMode=yes", "ConnectTimeout=2", "error: scp failed"],
        None,
    ),
    (
        "mpremote_no_output",
        "mpremote_run",
        {"script": str(negative_script), "device": "auto", "timeout_seconds": 8},
        ["$ mpremote connect auto run", "no runtime output"],
        fake_env,
    ),
    (
        "ssh_no_runtime_output",
        "ssh_deploy_run",
        {
            "host": "10.1.2.3",
            "user": "root",
            "local_path": str(negative_script),
            "remote_path": "/root/reasonix/main.py",
            "command": "python3 /root/reasonix/main.py",
            "timeout_seconds": 8,
        },
        ["$ scp", "$ ssh", "no runtime output"],
        fake_env,
    ),
]

for case in negative_cases:
    if len(case) == 4:
        name, tool, args, needles = case
        env = None
    else:
        name, tool, args, needles, env = case
    text, raw_path = call_tool(tool, args, f"{name}.json", expect_error=True, env=env)
    missing = [needle for needle in needles if needle not in text]
    if missing:
        raise AssertionError(f"{name} missing expected diagnostics {missing}. Output:\n{text[:2000]}")
    summary["negative"].append({"case": name, "artifact": rel(raw_path)})
    print(f"  negative {name}: ok")

(run_dir / "summary.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(f"summary: {run_dir / 'summary.json'}")
PY
