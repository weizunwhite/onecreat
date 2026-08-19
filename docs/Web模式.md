# Web 模式:单二进制 + 浏览器当 UI

> 一句话:**同一个 `*App`,换一层传输**。桌面版用 Wails 绑定 + 原生窗口;Web 模式起一个只绑
> 回环地址的本地 HTTP 服务,前端 bundle 内嵌在二进制里,浏览器打开就是完整 UI。
>
> **agent 仍然跑在用户本机**——要碰 USB 串口、烧录固件、读写本地工程目录。这不是云端部署,
> 是「本地服务 + 浏览器」。

动机很实际:桌面版每改一次要打 Mac/Windows 两套包、公证 DMG,分发和迭代都很重;Web 模式是
一个 18MB 的纯 Go 静态二进制,`go build` 完直接跑。

---

## 构建与运行

```bash
# 一条命令(前端 + 后端)
make build-web            # -> bin/onecreat-web

# 或者分开
cd desktop/frontend && pnpm build          # 前端产物必须先在 frontend/dist 里,才能被 embed
cd desktop && CGO_ENABLED=0 go build -tags web -o ../bin/onecreat-web .

# 跑
bin/onecreat-web                          # 默认 127.0.0.1:3700,自动开浏览器
bin/onecreat-web --no-open --port 3701    # 不开浏览器 / 换端口
bin/onecreat-web --workspace ~/projects/x # 指定工作目录(默认沿用上次记住的)
```

启动后 stdout 打印一条**带 token 的链接**,复制到浏览器即可:

```
OneCreat Web 已启动:
  http://127.0.0.1:3700/?token=45b8bc42…
(带 token 的链接只在本次进程有效;关掉进程即失效)
```

`Ctrl-C` 优雅退出:走的是和 Wails `OnShutdown` **同一个** `app.shutdown()`——每个标签存快照、
关 controller。

**纯 Go**:`CGO_ENABLED=0` 可编译,`wails` 依赖全部被 `//go:build !web` 隔离,所以能像内核一样
交叉编译(`GOOS=windows go build -tags web …`)。这正是 Web 模式相对桌面版最大的分发优势。

---

## 它是怎么工作的

### 1. `Shell` 接口(`desktop/shell.go`)

`*App` 原本直接调 `wails/v2/pkg/runtime`(事件推送 9 处、三种原生对话框各 1–2 处、开外链 1 处、
窗口操作 3 处)。这些全部收口到一个 `Shell` 接口:

| Shell 方法 | Wails 实现(`shell_wails.go`,`!web`) | Web 实现(`shell_web.go`,`web`) |
|---|---|---|
| `Emit(channel, payload)` | `runtime.EventsEmit` | 投给 SSE 广播器 |
| `OpenDirectoryDialog` | `runtime.OpenDirectoryDialog` | 返回 `ErrNoNativeDialog` |
| `OpenFileDialog` | `runtime.OpenFileDialog` | 返回 `ErrNoNativeDialog` |
| `OpenMultipleFilesDialog` | `runtime.OpenMultipleFilesDialog` | 返回 `ErrNoNativeDialog` |
| `BrowserOpenURL` | `runtime.BrowserOpenURL` | `open` / `xdg-open` / `ShellExecute` |
| `RaiseWindow` | `WindowShow`+`Unminimise`+`Center` | 空操作 |

单测里的裸 `&App{}` 没装 shell,由 `a.sh()` 兜底成 `noopShell`,不 panic。

### 2. 通用 RPC(`desktop/rpc.go`)

`POST /rpc/<方法名>`,body 是 **JSON 数组**(位置参数,和 Wails 绑定的调用语义一致),
用反射在 `*App` 的导出方法上分发。**112 个方法零手写路由**,加新方法不用改这里。

返回值折叠:

| Go 签名 | HTTP 响应 |
|---|---|
| `()` | `200 {"result": null}` |
| `(T)` | `200 {"result": T}` |
| `(error)` | nil → `200 {"result":null}`;非 nil → `500 {"error": "…"}` |
| `(T, error)` | 同上,成功时 `{"result": T}` |

其它情况:未知方法 `404`、参数个数/类型不对 `400`、方法 panic `500`(被 recover 兜住,不会打掉
整个进程)、非 POST `405`。

`rpc_test.go` 里的 `TestAppMethodsAreRPCCompatible` 守住这个前提:App 上一旦出现可变参数、
三个及以上返回值、或第二返回值不是 `error` 的方法,测试就红。

### 3. 事件流(`desktop/eventstream.go`)

`GET /events` 一条 SSE,每帧:

```
data: {"channel":"agent:event:main","payload":{"kind":"text","text":"你好"}}
```

前端只开**一个** `EventSource`,按 `channel` 分发给 `onEvent` / `onReady` /
`onUpdaterProgress` / `onSerialData` / `onSerialClosed`。

关键约束:`Emit` 跑在 agent 的运行循环 goroutine 上,**绝不能被慢客户端阻塞**。所以每个订阅者
是一个 512 帧的有缓冲 channel,写满就丢帧(丢的是渲染增量,不是状态——前端在 ready / turn 边界
会重新拉 `Meta`/`History` 对齐)。客户端断开自动清理订阅。

### 4. 前端(`desktop/frontend/src/lib/bridge.ts`)

`bridge.ts` 现在三级解析:

1. `window.go.main.App` 存在 → **Wails 绑定**;
2. 否则 `window.__ONECREAT_WEB__ === true`(服务端注入 index.html)→ **HTTP RPC + SSE**;
3. 否则 → **mock**(保住 `pnpm dev` 的裸浏览器开发体验,`makeMockApp` 一行未动)。

HTTP 路径用一个 `Proxy` 实现 `AppBindings`:任意方法名 → `POST /rpc/<name>`,参数数组透传,
不逐个手写。**React 组件一行未改。**

---

## 安全模型

Web 模式暴露的是一个**能在本机执行命令、读写文件、烧录设备的 agent**,不是一个只读页面。
所以四道锁:

1. **只绑回环** —— 默认 `127.0.0.1:3700`。`--host` 可以改,但改成非回环地址会在 stderr 打一条
   醒目警告(此时 Host 白名单自动放宽,只靠 token + Origin 守)。
2. **一次性 token** —— 启动时生成 32 字节随机 hex,不落盘,进程退出即失效。浏览器 URL 带
   `?token=…`,前端把它存进 `sessionStorage` **并从地址栏抹掉**(不进浏览器历史、复制链接不会
   带出去)。之后 `/rpc` 用 `Authorization: Bearer <token>`,`/events` 用 `?token=`
   (EventSource 不能带自定义 header)。比较用 `subtle.ConstantTimeCompare`。
3. **Host 白名单** —— 挡 DNS rebinding:恶意域名解析到 `127.0.0.1` 后拿浏览器当跳板打本地服务。
   请求的 `Host` 必须是 `localhost` / `127.0.0.1` / `::1` 且端口对得上。
4. **Origin 同源** —— 挡 CSRF:带 `Origin` 的请求必须与 `Host` 同源;无 `Origin`(curl 之类)放行。

静态资源(bundle)不校验 token——浏览器加载 JS/CSS 时还没有 header 可带——但同样受 Host 守卫,
且 bundle 本身不含任何用户数据。

**局限(诚实说)**:同一台机器上的任何本地进程都能访问 `127.0.0.1:3700`,只要它拿到 token。
token 不落盘,唯一泄漏面是「用户把带 token 的链接贴给别人 + 对方能连到这台机器」。单机自用场景
足够;**不要**把它当多用户服务来开。

---

## Web 下不可用 / 降级的功能

| 功能 | Wails 桌面版 | Web 模式 | 说明 |
|---|---|---|---|
| 选工作区文件夹 | app 内置 FolderPicker | ✅ 一样 | 主路径本来就没用系统对话框(`App.tsx` 走 `openFolderPicker`),`PickWorkspace` 前端根本没调 |
| 添加技能目录 | 系统目录对话框 | ✅ 改走内置 FolderPicker | bridge 层把 `PickSkillFolder` 映射到 `openFolderPicker()` |
| 上传参考资料(Composer 回形针) | 系统文件对话框 | ✅ 走 `<input type=file>` + `POST /upload` | 浏览器 File API 拿不到磁盘路径,所以前端把文件 POST 到 `/upload` 落到临时目录,再把路径喂给既有的 `ImportReferenceFile`。见「文件上传」节 |
| 知识库导入本地文件 | 系统多文件对话框 | ✅ 走 `<input type=file multiple>` + `POST /upload` | 同上,拿到路径后调 `KnowledgeImportPaths`(桌面版仍走 `KnowledgeImportFiles`) |
| 窗口显示/居中/取消最小化 | ✅ | 空操作 | 浏览器标签页没有这个概念 |
| 自动更新(`ApplyUpdate` 等) | ✅(本 fork 已禁用) | ✅ 检查+提示(不自替换) | Web 版有自己的更新渠道(`latest.json`),只提示不自更新;见「更新检查」节 |
| 打开外链 | webview 外的系统浏览器 | `window.open` | 行为等价 |
| 串口 / 烧录 / OTA / 硬件面板 | ✅ | ✅ **完全一样** | 都是 Go 侧本机能力,与传输层无关 |
| 多标签真并行 | ✅ | ✅ 一样 | 每个 tab 一条 `agent:event:<tabID>` 通道,SSE 照常分发 |
| 登录门 / 账号 / 档位 / 点数 | ✅ | ✅ 一样 | 实测 `ONECREAT_ACCOUNT_MODE=platform` 下 LoginGate 正常出现 |

---

## 单实例 + 「第二次双击」

老师的心智模型是「双击图标=打开软件」。Web 模式下软件是本地 HTTP 服务 + 浏览器标签;关掉标签后
再双击程序,不该报「端口被占」退出。所以:

- 启动成功后在配置目录写锁文件 `web.lock`(`0600`,内容 `{pid, port, token, startedAt}`),退出时删。
- **再次启动**先读锁:若 `pid` 仍存活且 `GET /healthz` 正常 → 说明已有实例在跑,用锁里的 token 把浏览器
  开到已有实例、打印「已在运行,已为你打开页面」、本进程退出码 0(不抢端口)。锁陈旧(进程已死/服务不通)
  则删掉照常启动。
- **端口自动避让**:请求端口被别的程序占用时,从该端口起向上探最多 20 个端口,落到第一个能监听的;
  实际端口会打印出来,也写进锁里。
- 只在回环绑定时启用单实例(`--host` 改成非回环是显式的多机场景,不套这层)。
- `GET /healthz` 是无鉴权探活端点(只回 `{"ok":true,"version":…}`),但同样受 Host 白名单守卫。

代码:`desktop/singleinstance.go`(锁读写/陈旧判定/端口探测)、`desktop/webserver.go` 的 `/healthz`。

## 更新检查

Web 模式是 onecreat 自己的主分发形态,有独立更新渠道,所以恢复「只检查、只提示、不自更新」的最小流程
(桌面版 `CheckUpdate` 仍刻意禁用,不 phone home 到上游 reasonix)。

- 客户端启动时 `CheckUpdate` → GET `latest.json`(5s 超时),比 semver;**失败一律静默返回无更新,绝不弹错**。
- `CanSelfUpdate=false`:Web 版只在页面顶部提示 + 「去下载」按钮(`UpdateBanner`),不替换自身。
- manifest 地址走 ldflags 注入(`-X main.webUpdateManifestURL=…`),默认
  `http://47.95.176.214/onecreat/latest.json`;下载落地页 `-X main.webDownloadPage=…`。
  验收/本地假 manifest 可用环境变量 `ONECREAT_UPDATE_MANIFEST_URL` 覆盖地址。

`latest.json` 最小格式(由 `scripts/web-build.sh` 在 `all` 模式下顺手生成):

```json
{
  "version": "v1.2.0",
  "downloadPage": "http://47.95.176.214/onecreat/",
  "assets": {
    "darwin-arm64": "http://47.95.176.214/onecreat/onecreat-web-darwin-arm64.tar.gz",
    "darwin-amd64": "http://47.95.176.214/onecreat/onecreat-web-darwin-amd64.tar.gz",
    "windows-amd64": "http://47.95.176.214/onecreat/onecreat-web-windows-amd64.zip",
    "linux-amd64": "http://47.95.176.214/onecreat/onecreat-web-linux-amd64.tar.gz",
    "linux-arm64": "http://47.95.176.214/onecreat/onecreat-web-linux-arm64.tar.gz"
  }
}
```

`assets` 的 key 是 `<os>-<arch>`(与 `update.PlatformKey` 一致);基址默认阿里云 nginx,打包时可用
`RELEASE_BASE_URL` 覆盖。代码:`desktop/updater_web.go`。

## 文件上传(参考资料 / 知识库导入)

浏览器 File API 只给 `File` 对象、拿不到磁盘绝对路径,而后端导入要的就是路径。所以 Web 模式加了一个
上传端点:

- `POST /upload`(multipart,字段名 `files`,鉴权同 `/rpc`,单文件上限 50MB)→ 把文件落到
  `os.TempDir()/onecreat-upload-*/<原文件名>`(挡目录穿越),返回绝对路径列表。
- 前端两处调用点(Composer 回形针 / KnowledgePanel「导入文件」)在 Web 模式下改走 `<input type=file>`
  → `uploadFiles()` → 拿到路径 → 调既有导入方法(`ImportReferenceFile` / `KnowledgeImportPaths`)。
  桌面版仍走原生对话框,行为不变。

代码:`desktop/webserver.go` 的 `serveUpload`、`bridge.ts` 的 `uploadFiles`。

## 退出

- Web 页面右上角有「退出 OneCreat」电源按钮(`WebQuitButton`,桌面版隐藏):二次确认 → `App.Quit()`
  → 后端走和 `Ctrl-C` 同一条优雅关闭路径(存快照 + 关 controller + 停服)→ 页面显示「已退出,可以关闭此标签」。
- `Ctrl-C` / 关终端窗口同样优雅退出。

## 已知问题

- **多个浏览器标签同时打开会各自订阅同一个事件流**。后端广播是扇出的,所以两个标签都能收到
  事件、都能操作同一个 agent——这是「同一个 App 的两个视图」,不是两个独立会话。想要独立会话
  请用 app 内的多标签(`CreateTab`),不是浏览器标签。
- **`--host` 改成非回环时,Host 白名单整体放宽**。此时只靠 token + Origin。真要给别人访问,
  自己在前面套一层反代 / Tailscale,别裸奔。
- 页面刷新会重建 EventSource,期间(毫秒级)产生的事件会丢;`useController` 挂载后会重新拉
  `Meta`/`History`,所以不会卡在 loading,但正在流式输出的那一小段增量看不到。

## 分发打包(主分发形态,2026-08-19 起)

```bash
scripts/web-build.sh darwin/arm64 v1.2.0   # 单平台
make release-web VERSION=v1.2.0            # 全平台 -> dist/onecreat-web-<os>-<arch>.{tar.gz,zip} + SHA256SUMS
```

- 发行包 = 目录:`onecreat-web(.exe)` + `onecreat-hardware-mcp(.exe)` + `README.txt`,解压即用。
  硬件 MCP 按"与主程序同目录"解析(`resolveHardwareMCP`),所以两者必须一起分发。
- 打包版注入 `-X main.defaultAccountMode=platform`(默认平台账号模式,与桌面版打包一致);
  `make build-web` 的开发版不注入 → 本地免登录。
- 纯 Go、`CGO_ENABLED=0`,一台机器交叉编译全平台;CI 走 `.github/workflows/release-web.yml`
  (tag `web-v*`,单 ubuntu runner)。不签名、不公证:macOS 首次运行需右键打开或去 quarantine,
  README.txt 里写了。Wails 桌面版(`desktop-build.sh` / `release-desktop.yml`)降为可选,脚本保留。

## 下一步建议

- ✅ **文件上传** —— 已实现(见「文件上传」节)。
- ✅ **单实例守卫 + 第二次双击** —— 已实现(见「单实例」节)。
- ✅ **更新检查** —— 已实现(见「更新检查」节)。
- ✅ **退出残留** —— codegraph 的 detached 守护进程曾在退出后残留最多 5 分钟(它有 5 分钟 idle
  超时),现由 `codegraph.StopDaemon` 按 `.codegraph/daemon.pid` 显式清理,Ctrl-C / Quit 后 3 秒内即净。
- **`internal/serve`**:老的 HTTP+SSE 服务(只覆盖 Controller 子集)和本模式功能重叠了。
  本次没动它,但长期看应该二选一。
- **多机/多用户**:当前是「本机自用」模型(见安全模型),不要当多用户服务开;真要给别人访问,
  前面套反代 / Tailscale。
