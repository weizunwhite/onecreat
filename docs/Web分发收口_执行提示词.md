# 任务:OneCreat Web 模式"分发收口"——把能跑的 Web 版做成能交给老师学生用的发行版

> 仓库 `/Users/localwork/06_System/onecreat`。先读 `CLAUDE.md`、`docs/Web模式.md`、`docs/开发工作流.md`。
> 背景:Web 模式(单二进制起本地服务 + 浏览器当 UI)已实现并定为主分发形态(`feat/web-mode` 分支,
> 打包脚本 `scripts/web-build.sh` / `make release-web` / `.github/workflows/release-web.yml` 已就绪)。
> 本任务是把"能跑"推到"能分发":分支收拢、单实例体验、更新检查、文件上传、退出残留、退出入口、全套验收。

## 决策参数(用户已拍板;没改就按这里执行)

- `MERGE_STRATEGY = 全部合入 main-v2`:把 `feat/web-mode`(含 WIP 账号改动、dsh config 字段、`internal/engine/dsh` 驱动层骨架、Web 模式)整分支合进 `main-v2`。dsh 驱动层是纯增量且 `engine` 默认 `native`,不影响运行。
- `UPDATE_CHANNEL = 阿里云 nginx`:发行包与 `latest.json` 放 `http://47.95.176.214/onecreat/`(这台机已跑固件 nginx,根目录 `/var/www`,详见记忆/`ota-remote-flash` skill),GitHub Releases 作镜像。**本任务只做客户端与打包侧**,不碰服务器(上传由用户/CI secret 另行处理);更新检查 URL 通过 ldflags 注入,默认值就写这个地址。
- `MAC_SIGNING = 暂不签名`:README 说明右键打开;不要花时间研究公证。
- `WINDOWS_VERIFY = 交叉编译产物 + 用户手测`:你没有 Windows 机器,保证 `GOOS=windows` 构建通过、脚本逻辑无平台分支错误即可,把需要用户在 Windows 上点测的清单写进报告。
- `DESKTOP_WAILS = 停更保留`:不动 `desktop-build.sh` / `release-desktop.yml`,但任何改动必须保证 `cd desktop && go build ./... && go vet ./... && go test ./...`(默认 !web 标签)仍绿。

## 硬规则

- 不派子代理;全程无人值守,不停下来问;需要拍板的按上面参数或保守默认做,写进报告"替用户决定"一节。
- 遵守 CLAUDE.md:Session 单写者/`Snapshot()`;`a.mu` 不跨 `boot.Build`;改 Go 签名同步 `bridge.ts` 的 `AppBindings` + mock;`desktop/wire.go` 与 `internal/serve/wire.go` 一致;不引入模型名泄漏点;新代码中文注释。
- 每个步骤一个或多个 commit,中文 message,末尾 `Co-Authored-By: Claude Opus <noreply@anthropic.com>`;**不 push**。
- 诚实汇报:做了/没做/猜的/替用户决定的。

## 步骤

### Step 1 — 分支收拢
1. 确认 `feat/web-mode` 工作树干净;`git log --oneline main-v2..feat/web-mode` 列出将合入的提交。
2. `git checkout main-v2 && git merge --no-ff feat/web-mode`(冲突就解,优先保留 web-mode 侧);合并后在 main-v2 跑:根 `go build/vet/test`,`cd desktop && go build/vet/test`(!web 与 `-tags web` 各一遍),`cd desktop/frontend && pnpm tsc --noEmit && pnpm build`,`gofmt -l .` 空。
3. 后续步骤都在 **main-v2** 上做。`spike/dsh-engine` / `feat/web-mode` 分支保留不删。

### Step 2 — 单实例 + "第二次双击"体验(`desktop/main_web.go` / `webserver.go`)
目标:老师关掉浏览器标签后再双击程序,**直接回到已在运行的实例**,而不是报端口占用退出。
1. 启动成功后写锁文件(用户配置目录,如 `~/.config/onecreat/web.lock` 或现有配置目录约定,权限 0600)内容 `{pid, port, token, startedAt}`;退出时删。
2. 再次启动时:若锁文件存在且 `pid` 存活且 `GET http://127.0.0.1:<port>/healthz`(新加的无鉴权只返 `{"ok":true,"version":…}` 的端点,同样受 Host 守卫)返回正常 → 用锁文件里的 token 打开浏览器到已有实例,然后本进程退出码 0,并在 stdout 说明"已在运行,已为你打开页面"。否则视为陈旧锁,删掉照常启动。
3. 端口被**别的程序**占用时:自动换端口(3700 起向上探 20 个),不要直接死。
4. 单测:锁文件写/读/陈旧判定;端口探测。

### Step 3 — 更新检查(`desktop/updater_app.go` / `updater.go` + 前端 `UpdateBanner`)
现状:`CheckUpdate` 为防泄漏到上游被整个禁用(注释写得很清楚,读它)。
1. 新增 ldflags 变量 `webUpdateManifestURL`(默认 `http://47.95.176.214/onecreat/latest.json`)和 `webDownloadPage`(默认同目录 `index.html` 或 GitHub Releases,二选一写清);只在 `web` 标签构建里生效,Wails 版行为不变(仍禁用)。
2. `CheckUpdate`:Web 模式下 GET manifest(超时 5s,失败静默返回"无更新",**绝不弹错**),比较 semver,返回 `Available/Latest/DownloadPage`;`CanSelfUpdate=false`(Web 版只提示不自更新)。`ApplyUpdate`/`OpenDownloadPage` 在 Web 下打开下载页(用 `Shell.BrowserOpenURL`)。
3. 定义并在 `docs/Web模式.md` 写清 `latest.json` 格式(最小:`{"version":"web-v1.2.0","downloadPage":"…","assets":{"darwin-arm64":"url",…}}`),并让 `scripts/web-build.sh` 在 `all` 模式下**顺手生成** `dist/latest.json`(assets URL 按 UPDATE_CHANNEL 基址拼,基址可用环境变量 `RELEASE_BASE_URL` 覆盖)。
4. 前端:确认 `UpdateBanner` 在 `Available=true` 时出现并能点开下载页;不能就最小改动修。
5. 单测:manifest 解析/semver 比较/失败静默。

### Step 4 — 文件上传(Web 下参考文件 / 知识库导入)
现状:`PickReferenceFile` / `KnowledgeImportFiles` 走原生文件对话框拿磁盘路径,Web 下返回明确错误。
1. 后端加 `POST /upload`(multipart,鉴权同 `/rpc`,单文件上限 50MB,落到 `os.TempDir()/onecreat-upload/<随机>/<原文件名>`,返回绝对路径列表)。
2. 前端:在 bridge 层提供 `uploadFiles(files: File[]): Promise<string[]>`;两处调用点(找 `PickReferenceFile`/`KnowledgeImportFiles` 的 React 调用处)在 Web 模式下改走 `<input type=file>` → 上传 → 拿到路径后调用**现有的**后端导入方法(`ImportReferenceFile` / 知识库按路径导入的那个方法,看实际名字)。Wails 模式下行为不变。**这是本任务唯一允许改 React 组件的地方,改动最小化。**
3. 单测:上传端点鉴权/大小限制/路径落盘;前端 `pnpm tsc` 绿。

### Step 5 — 退出残留与退出入口
1. 复现:起 `bin/onecreat-web --no-open`,Ctrl-C 后 `pgrep -fl "codegraph.js serve --mcp"` 仍有一个残留。定位 `app.shutdown()` → `ctrl.Close()` → MCP host 关闭链路为何漏杀(怀疑:进程组/stdin 关闭后未等退出/多 tab 各自一个 host 只关了 active)。修到 Ctrl-C 后 3 秒内无残留;同样逻辑 Wails 版也受益。
2. Web 下加"退出 OneCreat"入口:后端加 RPC `Quit()`(或复用已有的关闭方法),前端在设置面板/状态栏最不打扰的位置加一个按钮(Wails 模式隐藏);点击 → 优雅关闭 → 页面显示"已退出,可以关闭此标签"。
3. 单测能写的写;端到端用 curl/pgrep 实跑。

### Step 6 — 打包与文档同步
1. `scripts/web-build.sh`:README.txt 补"再次双击会回到已打开的页面 / 如何退出 / 如何更新";生成 `latest.json`(Step 3.3)。
2. `docs/Web模式.md`:更新"降级清单"(上传已支持)、新增单实例/更新/退出三节;`docs/开发工作流.md` B 段加"发布三步:打 tag → CI 出包 → 上传阿里云 + latest.json"。
3. `CLAUDE.md` 若有命令变化同步。

### Step 7 — 端到端验收(全部实跑,记录原始输出)
1. `make release-web VERSION=v0.0.0-rc`(可 `SKIP_FRONTEND=1` 复用已 build 的前端)→ 5 个包 + SHA256SUMS + latest.json。
2. 解开 darwin-arm64 包到临时目录,**不带任何环境变量**(模拟老师机器)运行:
   - 自动开浏览器(用 Browser 工具验证页面出现 LoginGate——打包版是平台模式);
   - 再开一个终端再次运行同一二进制 → 应打开已有实例页面并退出;
   - `HardwareMCP` RPC 解析到同目录的 `onecreat-hardware-mcp`;
   - 上传一个小文件走 `/upload` 并成功导入为参考文件(可用 curl 模拟 multipart);
   - 用一个临时 `latest.json`(本地 python http.server)注入 ldflags 或环境覆盖,验证 `CheckUpdate` 返回 `Available=true`;
   - 点/调 `Quit` → 进程退出、锁文件删除、无 codegraph 残留。
3. `GOOS=windows` / `linux` 交叉编译通过;windows zip 内容清单正确。
4. 三套 build/vet/test/tsc 全绿(main-v2)。

## 交付

最后一条消息(给审核用的原始数据,不是给用户的):
- main-v2 上新增的 commit 列表;改了哪些文件;
- Step 7 每一项的实际输出摘要;
- 没做到的、替用户决定的、发现的坑;
- **"需要用户在 Windows 上手测"清单**(一条一句:双击 exe、SmartScreen、COM 串口检测、编译烧录、第二次双击、退出);
- **"需要用户做的服务器侧动作"清单**(阿里云 nginx 建 `/var/www/onecreat/`、放发行包与 `latest.json`、CI 上传 secret)。
