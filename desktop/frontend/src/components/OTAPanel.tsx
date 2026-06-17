import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, FolderPlus, Globe, Loader2, UploadCloud, Wifi } from "lucide-react";
import { app, openExternal } from "../lib/bridge";
import type { HardwareRunResult, OTAScaffoldResult } from "../lib/types";

// 远程烧录(OTA)面板:把三种"不插 USB 下载"的能力放一处。
// - 新建 OTA 项目(脚手架):生成含底座的项目骨架,第一次仍 USB 烧一次。
// - WiFi 烧录(①)+ 升级页(②):局域网把固件推给已刷底座的板子。
// - 发布固件(③):编译并发布到远程服务器(NAS),板子自己拉取升级。
// 调的都是已真机验证的后端(HardwareScaffoldOTA / HardwareOTAUpload / HardwarePublishFirmware)。

type Mode = "lan" | "web" | "cloud";
const MODES: { key: Mode; label: string; hint: string }[] = [
  { key: "lan", label: "局域网直推", hint: "同一 WiFi,onecreat 往板子 IP 推(ArduinoOTA)" },
  { key: "web", label: "浏览器拖拽", hint: "板子开网页,把 .bin 拖进去" },
  { key: "cloud", label: "云端拉取", hint: "发布到 NAS,板子自己拉(真·远程,跨网段)" },
];

function ResultLine({ label, r }: { label: string; r: HardwareRunResult }) {
  const ok = r.status === "passed";
  const tone = ok ? "ok" : r.status === "skipped" ? "warn" : "fail";
  return (
    <div className={`ota__msg ota__msg--${tone}`}>
      {ok ? <CheckCircle2 size={12} /> : <AlertTriangle size={12} />}
      <span>
        {label}：{r.status}
        {r.summary ? ` — ${r.summary}` : ""}
      </span>
      {r.nextStep && <span className="ota__next">{r.nextStep}</span>}
      {r.error && <code className="ota__err">{r.error}</code>}
    </div>
  );
}

export function OTAPanel({
  projectDir,
  board,
  mcpReady,
  onOpenWorkspace,
}: {
  projectDir: string;
  board: string;
  mcpReady: boolean;
  onOpenWorkspace?: (path?: string) => void;
}) {
  // 新建 OTA 项目(脚手架)
  const [mode, setMode] = useState<Mode>("lan");
  const [scName, setScName] = useState("");
  const [ssid, setSsid] = useState("");
  const [wifiPwd, setWifiPwd] = useState("");
  const [scaffolding, setScaffolding] = useState(false);
  const [scaffoldMsg, setScaffoldMsg] = useState<{ ok: boolean; text: string; path?: string } | null>(null);

  // WiFi 烧录(①)
  const [ip, setIp] = useState("");
  const [otaPwd, setOtaPwd] = useState("oneup1234");
  const [wifiUploading, setWifiUploading] = useState(false);
  const [wifiResult, setWifiResult] = useState<HardwareRunResult | null>(null);

  // 发布固件(③)+ 固件服务器配置(各人填自己的 NAS/VPS,存 localStorage)
  const [pubName, setPubName] = useState("");
  const [pubVer, setPubVer] = useState("1.0.0");
  const [pubServer, setPubServer] = useState(() => localStorage.getItem("ota.pub.server") ?? "");
  const [pubSsh, setPubSsh] = useState(() => localStorage.getItem("ota.pub.ssh") ?? "");
  const [pubDir, setPubDir] = useState(() => localStorage.getItem("ota.pub.dir") ?? "");
  const [publishing, setPublishing] = useState(false);
  const [pubResult, setPubResult] = useState<HardwareRunResult | null>(null);

  useEffect(() => {
    localStorage.setItem("ota.pub.server", pubServer);
    localStorage.setItem("ota.pub.ssh", pubSsh);
    localStorage.setItem("ota.pub.dir", pubDir);
  }, [pubServer, pubSsh, pubDir]);

  const scaffold = useCallback(async () => {
    setScaffolding(true);
    setScaffoldMsg(null);
    try {
      const r: OTAScaffoldResult = await app.HardwareScaffoldOTA({
        projectName: scName.trim(),
        mode,
        wifiSSID: ssid.trim(),
        wifiPassword: wifiPwd,
      });
      if (r.ok) setScaffoldMsg({ ok: true, text: `已生成：${r.path ?? ""}`, path: r.path });
      else setScaffoldMsg({ ok: false, text: r.error ?? "生成失败" });
    } catch (e) {
      setScaffoldMsg({ ok: false, text: String((e as Error)?.message ?? e) });
    } finally {
      setScaffolding(false);
    }
  }, [scName, mode, ssid, wifiPwd]);

  const wifiUpload = useCallback(async () => {
    setWifiUploading(true);
    setWifiResult(null);
    try {
      const r = await app.HardwareOTAUpload({ projectDir, platform: "arduino", board, address: ip.trim(), otaPassword: otaPwd.trim() });
      setWifiResult(r);
    } catch (e) {
      setWifiResult({ status: "failed", summary: "调用失败", error: String((e as Error)?.message ?? e) });
    } finally {
      setWifiUploading(false);
    }
  }, [projectDir, board, ip, otaPwd]);

  const publish = useCallback(async () => {
    setPublishing(true);
    setPubResult(null);
    try {
      const r = await app.HardwarePublishFirmware({
        projectDir,
        board,
        projectName: pubName.trim(),
        version: pubVer.trim(),
        baseURL: pubServer.trim(),
        sshHost: pubSsh.trim(),
        remoteDir: pubDir.trim(),
      });
      setPubResult(r);
    } catch (e) {
      setPubResult({ status: "failed", summary: "调用失败", error: String((e as Error)?.message ?? e) });
    } finally {
      setPublishing(false);
    }
  }, [projectDir, board, pubName, pubVer, pubServer, pubSsh, pubDir]);

  return (
    <details className="hardware-section ota">
      <summary>远程烧录（OTA · 不插 USB）</summary>
      <p className="ota__intro">
        第一次用「新建 OTA 项目」生成含底座的骨架、USB 烧一次；之后就能 WiFi 烧 / 浏览器传 / 远程拉，再不用插线。
      </p>

      {/* 新建 OTA 项目(脚手架) */}
      <div className="ota__card">
        <div className="ota__card-head">
          <FolderPlus size={14} /> 新建 OTA 项目（含底座）
        </div>
        <div className="ota__modes">
          {MODES.map((m) => (
            <button key={m.key} className={`ota__mode ${mode === m.key ? "is-on" : ""}`} onClick={() => setMode(m.key)} title={m.hint}>
              {m.label}
            </button>
          ))}
        </div>
        <div className="ota__row">
          <input className="ota__in" placeholder="项目名(英文/数字)" value={scName} onChange={(e) => setScName(e.target.value)} />
          <input className="ota__in" placeholder="WiFi 名称" value={ssid} onChange={(e) => setSsid(e.target.value)} />
          <input className="ota__in" type="password" placeholder="WiFi 密码" value={wifiPwd} onChange={(e) => setWifiPwd(e.target.value)} />
          <button className="btn btn--primary" disabled={scaffolding || !scName.trim() || !ssid.trim()} onClick={() => void scaffold()}>
            {scaffolding ? <Loader2 size={13} className="hardware-spin" /> : <FolderPlus size={13} />} 生成
          </button>
        </div>
        {scaffoldMsg && (
          <div className={`ota__msg ota__msg--${scaffoldMsg.ok ? "ok" : "fail"}`}>
            {scaffoldMsg.ok ? <CheckCircle2 size={12} /> : <AlertTriangle size={12} />}
            <span>{scaffoldMsg.text}</span>
            {scaffoldMsg.ok && scaffoldMsg.path && onOpenWorkspace && (
              <button className="ota__link" onClick={() => onOpenWorkspace(scaffoldMsg.path)}>
                打开
              </button>
            )}
          </div>
        )}
      </div>

      {/* WiFi 烧录(①) + 升级页(②) */}
      <div className="ota__card">
        <div className="ota__card-head">
          <Wifi size={14} /> WiFi 烧录（局域网，板子需已刷底座）
        </div>
        <div className="ota__row">
          <input className="ota__in" placeholder="板子 IP 或 esp32-onecreat.local" value={ip} onChange={(e) => setIp(e.target.value)} />
          <input className="ota__in ota__in--sm" placeholder="OTA 口令" value={otaPwd} onChange={(e) => setOtaPwd(e.target.value)} />
          <button
            className="btn btn--primary"
            disabled={!mcpReady || wifiUploading || !projectDir || !ip.trim()}
            onClick={() => void wifiUpload()}
            title={!projectDir ? "先打开硬件项目" : "通过 WiFi 烧录当前项目"}
          >
            {wifiUploading ? <Loader2 size={13} className="hardware-spin" /> : <UploadCloud size={13} />} WiFi 烧录
          </button>
          <button className="btn" disabled={!ip.trim()} onClick={() => openExternal(`http://${ip.trim()}/`)} title="打开板子的浏览器升级页(②)">
            <Globe size={13} /> 升级页
          </button>
        </div>
        {wifiResult && <ResultLine label="WiFi 烧录" r={wifiResult} />}
      </div>

      {/* 发布固件(③) */}
      <div className="ota__card">
        <div className="ota__card-head">
          <UploadCloud size={14} /> 发布固件到远程（你的 NAS / VPS，板子自动拉取）
        </div>
        {/* 固件服务器配置(各人填自己的,存在本机) */}
        <div className="ota__row">
          <input className="ota__in" placeholder="服务器URL 如 http://192.168.1.9:9000" value={pubServer} onChange={(e) => setPubServer(e.target.value)} />
          <input className="ota__in ota__in--sm" placeholder="SSH 目标 如 nas" value={pubSsh} onChange={(e) => setPubSsh(e.target.value)} />
          <input className="ota__in" placeholder="远程目录 如 /share/Public/fw" value={pubDir} onChange={(e) => setPubDir(e.target.value)} />
        </div>
        <div className="ota__row">
          <input className="ota__in" placeholder="项目名(= 服务器上的文件夹)" value={pubName} onChange={(e) => setPubName(e.target.value)} />
          <input className="ota__in ota__in--sm" placeholder="版本号 1.0.1" value={pubVer} onChange={(e) => setPubVer(e.target.value)} />
          <button
            className="btn btn--primary"
            disabled={!mcpReady || publishing || !projectDir || !pubName.trim() || !pubVer.trim()}
            onClick={() => void publish()}
            title={!projectDir ? "先打开硬件项目" : "编译当前项目并发布到 NAS"}
          >
            {publishing ? <Loader2 size={13} className="hardware-spin" /> : <UploadCloud size={13} />} 发布固件
          </button>
        </div>
        {pubResult && <ResultLine label="发布固件" r={pubResult} />}
      </div>
    </details>
  );
}
