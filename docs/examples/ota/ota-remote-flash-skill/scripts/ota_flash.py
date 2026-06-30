#!/usr/bin/env python3
# ota_flash.py —— ESP32 远程烧录(OTA)自包含 CLI,三种方式:
#   lan-push       局域网直推(ArduinoOTA / 底层 espota):电脑往板子 IP 推固件
#   web-push       浏览器拖拽底座的命令行版:curl 把 .bin POST 到板子的 /update
#   cloud-publish  云端拉取:编译 + 发布到固件服务器,板子定时自己拉取升级
#
# 只依赖 arduino-cli + curl + ssh,不需要 onecreat 或它的 MCP 服务器,
# 因此任意 Claude Code / Codex 直接调用本脚本就能远程烧录。
#
# 铁律:三种方式都要求板子已经"第一次用 USB 烧过对应底座"(空板子是哑的)。
# 底座模板见 docs/examples/ota/ 或 onecreat 硬件面板「新建 OTA 项目」。

import argparse
import glob
import os
import shutil
import subprocess
import sys
import tempfile

DEFAULT_FQBN = "esp32:esp32:esp32"
DEFAULT_OTA_PASSWORD = "oneup1234"          # 和 lan 底座里 OTA_PASSWORD 一致
DEFAULT_REMOTE_DIR = "/share/Public/onecreat-firmware"  # NAS 固件根目录


def info(msg):
    print(f"  {msg}", flush=True)


def fail(msg, code=1):
    """如实报告失败并退出,绝不假称成功。"""
    print(f"❌ {msg}", file=sys.stderr, flush=True)
    sys.exit(code)


def find_arduino_cli(explicit, dry_run=False):
    """解析 arduino-cli 路径:--arduino-cli > $ARDUINO_CLI > PATH > onecreat 一键安装目录。"""
    candidates = []
    if explicit:
        candidates.append(explicit)
    if os.environ.get("ARDUINO_CLI"):
        candidates.append(os.environ["ARDUINO_CLI"])
    on_path = shutil.which("arduino-cli")
    if on_path:
        candidates.append(on_path)
    # onecreat 一键安装把 arduino-cli 装在用户目录,顺手找一下
    home = os.path.expanduser("~")
    candidates += [
        os.path.join(home, ".onecreat", "arduino-cli", "arduino-cli"),
        os.path.join(home, "Library", "Application Support", "onecreat", "arduino-cli", "arduino-cli"),
    ]
    for c in candidates:
        if c and os.path.isfile(c) and os.access(c, os.X_OK):
            return c
    if dry_run:
        return "arduino-cli"   # 演练模式只展示命令,没装也无妨
    fail("找不到 arduino-cli。装一个(onecreat 硬件面板有一键安装),"
         "或用 --arduino-cli 指定路径 / 设环境变量 ARDUINO_CLI。")


def run(cmd, dry_run=False, stdin_file=None):
    """跑一条命令:先打印它(方便排错),再执行。返回 CompletedProcess。"""
    printable = " ".join(cmd)
    info(f"$ {printable}")
    if dry_run:
        return subprocess.CompletedProcess(cmd, 0)
    stdin = open(stdin_file, "rb") if stdin_file else None
    try:
        return subprocess.run(cmd, stdin=stdin)
    finally:
        if stdin:
            stdin.close()


def compile_sketch(cli, fqbn, sketch, dry_run=False):
    """编译 sketch,返回 app 固件 .bin 的路径(OTA 推的就是这个 app bin,不是 merged 全镜像)。"""
    sketch = os.path.abspath(sketch)
    if not os.path.isdir(sketch):
        fail(f"sketch 目录不存在:{sketch}")
    out_dir = tempfile.mkdtemp(prefix="ota_build_")
    rc = run([cli, "compile", "--fqbn", fqbn, "--output-dir", out_dir, sketch],
             dry_run=dry_run)
    if not dry_run and rc.returncode != 0:
        fail("编译失败(上面是 arduino-cli 的报错)。先把代码编过再烧。")
    if dry_run:
        return os.path.join(out_dir, "sketch.ino.bin")
    # app bin 命名是 <name>.ino.bin;排除 .merged/.bootloader/.partitions 全镜像
    bins = glob.glob(os.path.join(out_dir, "*.ino.bin"))
    if not bins:
        # 退路:任意 .bin 但排掉全镜像/引导/分区表
        for b in glob.glob(os.path.join(out_dir, "*.bin")):
            low = b.lower()
            if not any(x in low for x in (".merged.", ".bootloader.", ".partitions.")):
                bins.append(b)
    if not bins:
        fail(f"编译完成但没找到固件 .bin(在 {out_dir})。")
    return bins[0]


# ---------------------------------------------------------------------------
# ① 局域网直推:电脑往板子 IP 推(ArduinoOTA / espota)。要求同一局域网。
# ---------------------------------------------------------------------------
def cmd_lan_push(args):
    cli = find_arduino_cli(args.arduino_cli, dry_run=args.dry_run)
    sketch = os.path.abspath(args.sketch)
    info(f"局域网直推 → 板子 {args.ip}(密码 {args.password})")
    # 先编译(确保和待推固件一致),再用网络口烧录
    rc = run([cli, "compile", "--fqbn", args.fqbn, sketch], dry_run=args.dry_run)
    if not args.dry_run and rc.returncode != 0:
        fail("编译失败,没有可推的固件。")
    rc = run([cli, "upload", "-p", args.ip, "--fqbn", args.fqbn,
              "--upload-field", f"password={args.password}", sketch], dry_run=args.dry_run)
    if not args.dry_run and rc.returncode != 0:
        fail("WiFi 烧录失败。排查:板子是否在同一局域网、IP 对不对、"
             "底座 OTA_PASSWORD 是否和 --password 一致、板子是否还在跑 ArduinoOTA 底座。")
    print("✅ 局域网直推完成,板子会自动重启进新固件。")


# ---------------------------------------------------------------------------
# ② 浏览器拖拽(命令行版):把 .bin POST 到板子自带的 /update 上传页。要求同一局域网。
# ---------------------------------------------------------------------------
def cmd_web_push(args):
    if args.bin:
        bin_path = os.path.abspath(args.bin)
        if not os.path.isfile(bin_path):
            fail(f"--bin 指定的固件不存在:{bin_path}")
    else:
        cli = find_arduino_cli(args.arduino_cli, dry_run=args.dry_run)
        bin_path = compile_sketch(cli, args.fqbn, args.sketch, dry_run=args.dry_run)
    url = f"http://{args.ip}/update"
    info(f"浏览器拖拽(curl)→ {url},固件 {os.path.basename(bin_path)}")
    rc = run(["curl", "-f", "-S", "-F", f"firmware=@{bin_path}", url], dry_run=args.dry_run)
    if not args.dry_run and rc.returncode != 0:
        fail("上传失败。排查:板子是否在同一局域网、IP 对不对、"
             "板子是否还在跑「浏览器拖拽」底座(开着 http://板子IP/)。")
    print("✅ 已上传,板子收到后会自动重启进新固件。")


# ---------------------------------------------------------------------------
# ③ 云端拉取:编译 + 发布 firmware.bin/version.txt 到固件服务器,板子定时自己拉。真·远程。
# ---------------------------------------------------------------------------
def cmd_cloud_publish(args):
    if args.bin:
        bin_path = os.path.abspath(args.bin)
        if not os.path.isfile(bin_path):
            fail(f"--bin 指定的固件不存在:{bin_path}")
    else:
        cli = find_arduino_cli(args.arduino_cli, dry_run=args.dry_run)
        bin_path = compile_sketch(cli, args.fqbn, args.sketch, dry_run=args.dry_run)

    proj_dir = f"{args.remote_dir.rstrip('/')}/{args.project}"
    base_url = args.base_url.rstrip("/")
    info(f"发布到 {args.host}:{proj_dir}  版本 {args.version}")

    # 用 ssh + stdin 传 bin,绕开 scp 对中文/空格远程路径的转义坑;远程路径用单引号包住。
    put_bin = run(["ssh", args.host, f"mkdir -p '{proj_dir}' && cat > '{proj_dir}/firmware.bin'"],
                  dry_run=args.dry_run, stdin_file=None if args.dry_run else bin_path)
    if not args.dry_run and put_bin.returncode != 0:
        fail(f"传固件失败(scp/ssh)。排查 ssh 别名 {args.host} 能否免密登录、远程目录权限。")

    put_ver = run(["ssh", args.host, f"printf '%s' '{args.version}' > '{proj_dir}/version.txt'"],
                  dry_run=args.dry_run)
    if not args.dry_run and put_ver.returncode != 0:
        fail("写 version.txt 失败。固件可能已传上去但版本号没更新,板子不会升级。")

    print("✅ 发布完成。")
    print(f"   固件 URL: {base_url}/{args.project}/firmware.bin")
    print(f"   版本    : {args.version}")
    print(f"   板子(已刷云端拉取底座)会在下次轮询(默认 30 秒)自动拉取升级。")
    print(f"   ⚠️ 记得把板子底座源码里的 CURRENT_VERSION 在下次编译时对齐为 {args.version},避免反复升级。")


def build_parser():
    p = argparse.ArgumentParser(
        prog="ota_flash.py",
        description="ESP32 远程烧录(OTA)三合一:lan-push / web-push / cloud-publish。"
                    "三种都要求板子已用 USB 烧过对应底座。")
    p.add_argument("--arduino-cli", help="arduino-cli 路径(默认自动找 PATH / $ARDUINO_CLI / onecreat 安装目录)")
    p.add_argument("--fqbn", default=DEFAULT_FQBN, help=f"开发板 FQBN(默认 {DEFAULT_FQBN})")
    p.add_argument("--dry-run", action="store_true", help="只打印将执行的命令,不真的跑(无硬件时用)")
    sub = p.add_subparsers(dest="cmd", required=True)

    a = sub.add_parser("lan-push", help="① 局域网直推(同一 WiFi,电脑→板子 IP)")
    a.add_argument("--sketch", required=True, help="项目目录(含 .ino)")
    a.add_argument("--ip", required=True, help="板子 IP(串口日志里有,如 192.168.1.23)")
    a.add_argument("--password", default=DEFAULT_OTA_PASSWORD, help=f"OTA 口令(默认 {DEFAULT_OTA_PASSWORD},要和底座一致)")
    a.set_defaults(func=cmd_lan_push)

    b = sub.add_parser("web-push", help="② 浏览器拖拽的命令行版(同一 WiFi,curl 传 .bin)")
    b.add_argument("--ip", required=True, help="板子 IP")
    b.add_argument("--sketch", help="项目目录(不给 --bin 时,现编)")
    b.add_argument("--bin", help="直接推一个编译好的 .bin(给了就不编)")
    b.set_defaults(func=cmd_web_push)

    c = sub.add_parser("cloud-publish", help="③ 云端拉取:发布到固件服务器(真·远程)")
    c.add_argument("--project", required=True, help="服务器上的项目文件夹名(要和板子底座 URL 里的一致)")
    c.add_argument("--version", required=True, help="新版本号,要比板子当前大(如 1.0.2)")
    c.add_argument("--host", required=True, help="固件服务器的 ssh 别名/地址(如 nas)")
    c.add_argument("--sketch", help="项目目录(不给 --bin 时,现编)")
    c.add_argument("--bin", help="直接发布一个编译好的 .bin")
    c.add_argument("--remote-dir", default=DEFAULT_REMOTE_DIR, help=f"服务器固件根目录(默认 {DEFAULT_REMOTE_DIR})")
    c.add_argument("--base-url", default="http://你的固件服务器:9000", help="固件服务器对外基址(用于打印 URL)")
    c.set_defaults(func=cmd_cloud_publish)
    return p


def main():
    args = build_parser().parse_args()
    # web-push / cloud-publish:--sketch 和 --bin 至少给一个
    if args.cmd in ("web-push", "cloud-publish") and not args.sketch and not args.bin:
        fail(f"{args.cmd} 需要 --sketch(现编)或 --bin(已编好)二选一。")
    args.func(args)


if __name__ == "__main__":
    main()
