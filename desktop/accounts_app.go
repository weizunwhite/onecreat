package main

// 账号 / 权限体系现在是可选平台层。默认 local-first:不登录、不读旧平台会话、不改写
// provider,直接使用本机 onecreat.toml / .env 里的 API 配置。以后需要联 teacher 平台时,
// 显式设置 ONECREAT_ACCOUNT_MODE=platform 才启用登录、权限、点数和网关改写。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AccountTier 是一个档位(订阅制)。客户端只见 index+name,不知道背后是什么模型(平台映射)。
type AccountTier struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
}

// AccountSession 是发给前端的会话(不含 token)。
type AccountSession struct {
	LoggedIn     bool          `json:"loggedIn"`
	Account      string        `json:"account"`
	IsAdmin      bool          `json:"isAdmin"`
	Permissions  []string      `json:"permissions"`
	Tiers        []AccountTier `json:"tiers"`        // 三档(订阅制);超管 / 未配为空
	Points       *float64      `json:"points"`       // 机构点数余额(登录时快照);超管=null 不限
	SelectedTier int           `json:"selectedTier"` // 当前选中档位 1/2/3
}

// AccountLoginResult 是登录返回。
type AccountLoginResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// persistedSession 落盘的会话(含 token,内部用,不发前端)。
type persistedSession struct {
	Account      string        `json:"account"`
	IsAdmin      bool          `json:"isAdmin"`
	Permissions  []string      `json:"permissions"`
	Token        string        `json:"token"`
	RefreshToken string        `json:"refreshToken"` // 长效刷新令牌:access token 快过期时用它静默换新,免重登
	ExpiresAt    int64         `json:"expiresAt"`    // access token 过期时刻(unix 秒);0=未知
	Tiers        []AccountTier `json:"tiers"`
	Points       *float64      `json:"points"`
	SelectedTier int           `json:"selectedTier"`
}

func accountSessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "onecreat", "session.json"), nil
}

const accountModeEnv = "ONECREAT_ACCOUNT_MODE"

// platformAccountEnabled 报告是否启用平台账号层。默认不启用,保证桌面端开发和交付都先
// 走本地 API key;teacher 平台只是以后显式打开的集成通道。
func platformAccountEnabled() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv(accountModeEnv)))
	return mode == "platform" || mode == "teacher" || mode == "gateway"
}

// platformBaseURL 是 teacher 平台地址(onecreat 登录/权限/AI 网关都连它)。只在
// ONECREAT_ACCOUNT_MODE=platform 时使用。可用 ONECREAT_PLATFORM_URL 覆盖本地联调地址。
func platformBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("ONECREAT_PLATFORM_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://t.weizunxy.com"
}

// 「网关模式」用的两个进程环境变量。boot.go 的 applyOnecreatGateway 看到 URL 非空,就把模型
// 请求改走平台 AI 网关、用 TOKEN 当 key(B 端客户不必自带 DeepSeek key,用量统一走平台计费)。
const (
	gatewayEnvURL   = "ONECREAT_GATEWAY_URL"
	gatewayEnvToken = "ONECREAT_GATEWAY_TOKEN"
	gatewayEnvTier  = "ONECREAT_TIER" // 选中档位 "tier-1/2/3";boot 用它覆盖网关 provider 的 model
)

// gatewayActive 报告当前是否处于「onecreat 网关模式」(已登录、AI 走平台网关)。判据是
// applyGatewayEnvFromSession 设的网关 URL env 是否存在。此模式下 provider / 模型 / 密钥由平台
// 统一分配,客户端不得再自行改 —— 否则可加一个自带 key 的非网关 provider 完全绕开网关与计量
// (H4)。前端已隐藏相关设置入口,这是后端兜底(防 devtools / 直接调 IPC 绕过)。
func gatewayActive() bool {
	return platformAccountEnabled() && strings.TrimSpace(os.Getenv(gatewayEnvURL)) != ""
}

// errGatewayManaged 是网关模式下拒绝本地改 provider/模型/key 时返回的统一错误。
var errGatewayManaged = fmt.Errorf("已登录账号:AI 由平台统一分配,无法在本地修改模型 / 服务商 / 密钥(如需自配请先退出登录)")

// sessionFileMu 串行化对 session.json 的所有读-改-写。session.json 是单文件(每个系统用户
// 一份):切档(SetOnecreatTier)与每轮结束触发的 RefreshAccountSession 会并发读改写它。无锁
// 时 refresh 会把「进入时读到的旧 SelectedTier」连同整个结构体回写,覆盖掉用户刚切的新档
// (H3:UI 显示新档、实际下次按旧档送模型 + 计费)。所有读改写都必须在持有本锁时进行。
var sessionFileMu sync.Mutex

// loadSessionFileLocked 读取持久化会话;调用方必须已持有 sessionFileMu。ok=false 表示
// 文件不存在 / 解析失败 / 无 token(视为未登录)。
func loadSessionFileLocked() (persistedSession, bool) {
	path, err := accountSessionPath()
	if err != nil {
		return persistedSession{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return persistedSession{}, false
	}
	var p persistedSession
	if json.Unmarshal(b, &p) != nil || p.Token == "" {
		return persistedSession{}, false
	}
	return p, true
}

// saveSessionFileLocked 落盘会话(0600);调用方必须已持有 sessionFileMu。
func saveSessionFileLocked(p persistedSession) error {
	path, err := accountSessionPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

func clearGatewayEnvVars() {
	_ = os.Unsetenv(gatewayEnvURL)
	_ = os.Unsetenv(gatewayEnvToken)
	_ = os.Unsetenv(gatewayEnvTier)
}

// applyGatewayEnvFromSession 按当前会话设/清网关环境变量:已登录 → 指向平台 AI 网关 +
// 写入 token;未登录 → 清空(回到 config 里的直连 provider)。每次 boot.Build 之前都要保证
// 它已被调用过(startup 里调一次),登录/登出后再调一次并重建 controller 让其立即生效。
func applyGatewayEnvFromSession() {
	if !platformAccountEnabled() {
		clearGatewayEnvVars()
		return
	}
	sessionFileMu.Lock()
	p, ok := loadSessionFileLocked()
	sessionFileMu.Unlock()
	if !ok {
		clearGatewayEnvVars()
		return
	}
	_ = os.Setenv(gatewayEnvURL, platformBaseURL()+"/api/onecreat/v1")
	_ = os.Setenv(gatewayEnvToken, p.Token)
	tier := p.SelectedTier
	if tier < 1 || tier > 3 {
		tier = 1
	}
	_ = os.Setenv(gatewayEnvTier, fmt.Sprintf("tier-%d", tier))
}

// accountAuthenticate 调 teacher 平台 /api/onecreat/login(手机号+密码)拿 token + 功能权限。
// 返回会话和错误信息(errMsg=="" 表示成功)。这是"问后端要账号+权限"的唯一入口。
func accountAuthenticate(account, password string) (persistedSession, string) {
	if !platformAccountEnabled() {
		return persistedSession{}, "平台账号模式未启用;当前为本地 API 模式"
	}
	payload, _ := json.Marshal(map[string]string{"phone": strings.TrimSpace(account), "password": password})
	req, err := http.NewRequest(http.MethodPost, platformBaseURL()+"/api/onecreat/login", bytes.NewReader(payload))
	if err != nil {
		return persistedSession{}, "请求构造失败"
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return persistedSession{}, "连不上服务器(检查网络/平台地址)"
	}
	defer resp.Body.Close()
	var r struct {
		OK           bool          `json:"ok"`
		Token        string        `json:"token"`
		RefreshToken string        `json:"refresh_token"`
		ExpiresIn    int64         `json:"expires_in"` // access token 有效秒数
		Account      string        `json:"account"`
		IsAdmin      bool          `json:"isAdmin"`
		Features     []string      `json:"features"`
		Tiers        []AccountTier `json:"tiers"`
		Points       *float64      `json:"points"`
		Error        string        `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if !r.OK || r.Token == "" {
		if strings.TrimSpace(r.Error) != "" {
			return persistedSession{}, r.Error
		}
		return persistedSession{}, "登录失败"
	}
	return persistedSession{
		Account: r.Account, IsAdmin: r.IsAdmin, Permissions: r.Features, Token: r.Token,
		RefreshToken: r.RefreshToken, ExpiresAt: expiresAtFrom(r.ExpiresIn),
		Tiers: r.Tiers, Points: r.Points, SelectedTier: 1, // 默认选第一档
	}, ""
}

// expiresAtFrom 把「有效秒数」换成 unix 过期时刻;<=0(平台没返回)时回退一个保守值(50 分钟),
// 让续期逻辑仍能按时触发(Supabase access token 默认 1 小时)。
func expiresAtFrom(expiresIn int64) int64 {
	if expiresIn <= 0 {
		expiresIn = 50 * 60
	}
	return time.Now().Unix() + expiresIn
}

// refreshMu 串行化 token 续期:避免后台 ticker / 启动 / 多处并发刷新导致 refresh_token 轮换
// 冲突(Supabase refresh token 单次使用、会轮换)。整个续期(含网络)在本锁内,只刷新不阻塞
// 其它会话操作。
var refreshMu sync.Mutex

// ensureFreshToken 在网关 access token 快过期时,用 refresh_token 静默换新——用户无需重新登录。
// 阈值 20 分钟;refresh_token 缺失(未登录 / 老会话)或网络/令牌失效时静默保持原状(到期会有
// 「请重新登录」提示兜底)。成功后刷新 ONECREAT_GATEWAY_TOKEN env;provider 请求时读 env,
// 新 token 立即对所有标签生效,无需重建。
func ensureFreshToken() {
	if !platformAccountEnabled() {
		return
	}
	refreshMu.Lock()
	defer refreshMu.Unlock()

	sessionFileMu.Lock()
	p, ok := loadSessionFileLocked()
	sessionFileMu.Unlock()
	if !ok || p.RefreshToken == "" {
		return // 未登录,或老会话没有 refresh_token(需手动重登一次以启用自动续期)
	}
	if p.ExpiresAt > 0 && time.Now().Unix() < p.ExpiresAt-20*60 {
		return // 还早,不刷
	}

	payload, _ := json.Marshal(map[string]string{"refresh_token": p.RefreshToken})
	req, err := http.NewRequest(http.MethodPost, platformBaseURL()+"/api/onecreat/refresh", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return // 网络问题:保持原 token,到期再提示重登
	}
	defer resp.Body.Close()
	var r struct {
		OK           bool   `json:"ok"`
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if !r.OK || r.Token == "" {
		return // refresh_token 失效(太久没用):保持原状,到期提示重登
	}

	sessionFileMu.Lock()
	cur, stillIn := loadSessionFileLocked()
	if stillIn {
		// 只更新令牌相关字段,其余(SelectedTier/Points 等)用重读到的最新值保留(H3 纪律)。
		cur.Token = r.Token
		if r.RefreshToken != "" {
			cur.RefreshToken = r.RefreshToken
		}
		cur.ExpiresAt = expiresAtFrom(r.ExpiresIn)
		_ = saveSessionFileLocked(cur)
	}
	sessionFileMu.Unlock()
	if stillIn {
		applyGatewayEnvFromSession() // 把新 token 写进 ONECREAT_GATEWAY_TOKEN
	}
}

// AccountLogin 校验账号密码,成功则持久化会话。
func (a *App) AccountLogin(account, password string) AccountLoginResult {
	if !platformAccountEnabled() {
		return AccountLoginResult{Error: "当前为本地 API 模式,无需登录平台账号"}
	}
	sess, errMsg := accountAuthenticate(account, password)
	if errMsg != "" {
		return AccountLoginResult{Error: errMsg}
	}
	sessionFileMu.Lock()
	err := saveSessionFileLocked(sess)
	sessionFileMu.Unlock()
	if err != nil {
		return AccountLoginResult{Error: err.Error()}
	}
	// 登录后启用网关:设 env + 重建「所有」标签的 controller,让 AI 立即改走平台网关(用本次
	// token 鉴权),无需重启 app。重建所有 tab(非仅活动 tab):否则后台 tab 仍走直连 / 旧态。
	applyGatewayEnvFromSession()
	a.rebuildAllTabs()
	return AccountLoginResult{OK: true}
}

// AccountLogout 清除会话,并把 AI 切回直连(清网关 env + 重建 controller)。
func (a *App) AccountLogout() {
	sessionFileMu.Lock()
	if path, err := accountSessionPath(); err == nil {
		_ = os.Remove(path)
	}
	sessionFileMu.Unlock()
	// 清网关 env + 重建「所有」标签:登出后撤销的 token 不能再被任何后台 tab 使用(否则
	// 后台 tab 仍持旧 token 继续打计费端点)。
	applyGatewayEnvFromSession()
	a.rebuildAllTabs()
}

// AccountSessionInfo 返回当前会话(未登录则 LoggedIn=false)。token 不外泄。
func (a *App) AccountSessionInfo() AccountSession {
	if !platformAccountEnabled() {
		return AccountSession{LoggedIn: false, Account: "本地模式", SelectedTier: 1}
	}
	sessionFileMu.Lock()
	p, ok := loadSessionFileLocked()
	sessionFileMu.Unlock()
	if !ok {
		return AccountSession{}
	}
	sel := p.SelectedTier
	if sel < 1 || sel > 3 {
		sel = 1
	}
	return AccountSession{
		LoggedIn: true, Account: p.Account, IsAdmin: p.IsAdmin, Permissions: p.Permissions,
		Tiers: p.Tiers, Points: p.Points, SelectedTier: sel,
	}
}

// SetOnecreatTier 切换当前档位(1/2/3):写回 session.json + 刷新网关 env + 重建 controller,
// 让下一条消息按新档位走。背后是什么模型客户端不知道(平台映射)。
func (a *App) SetOnecreatTier(index int) {
	if !platformAccountEnabled() {
		return
	}
	if index < 1 || index > 3 {
		return
	}
	sessionFileMu.Lock()
	p, ok := loadSessionFileLocked()
	if ok {
		p.SelectedTier = index
		_ = saveSessionFileLocked(p)
	}
	sessionFileMu.Unlock()
	if !ok {
		return
	}
	// 切档后重建「所有」标签:tier 在 boot.Build 时被烤进每个 tab 的 provider,只重建活动 tab
	// 会让后台 tab 继续按旧档计费(H2)。
	applyGatewayEnvFromSession()
	a.rebuildAllTabs()
}

// RefreshAccountSession 向平台 /api/onecreat/session 拉最新 points/tiers(每轮对话结束后调,
// 让余额实时下降、看得到消耗)。网络问题或 token 失效则保持本地快照不变(AI 调用自己会报
// 401 提示重登)。不重建 controller,纯刷新展示数据。
func (a *App) RefreshAccountSession() AccountSession {
	if !platformAccountEnabled() {
		return a.AccountSessionInfo()
	}
	sessionFileMu.Lock()
	p, ok := loadSessionFileLocked()
	sessionFileMu.Unlock()
	if !ok {
		return AccountSession{}
	}
	req, err := http.NewRequest(http.MethodGet, platformBaseURL()+"/api/onecreat/session", nil)
	if err != nil {
		return a.AccountSessionInfo()
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	resp, err := (&http.Client{Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return a.AccountSessionInfo() // 网络问题:回退本地快照
	}
	defer resp.Body.Close()
	var r struct {
		LoggedIn bool          `json:"loggedIn"`
		IsAdmin  bool          `json:"isAdmin"`
		Features []string      `json:"features"`
		Tiers    []AccountTier `json:"tiers"`
		Points   *float64      `json:"points"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if !r.LoggedIn {
		return a.AccountSessionInfo() // token 失效:保持本地
	}
	// 关键(H3):网络调用期间用户可能切了档(SetOnecreatTier 已把新 SelectedTier 落盘)。
	// 这里必须重新读盘拿最新会话,只把服务端字段(points/tiers/features)合并进去回写,绝不
	// 用进入本函数时读到的旧 SelectedTier 覆盖它 —— 否则就把用户刚切的新档冲回旧值了。
	sessionFileMu.Lock()
	cur, stillIn := loadSessionFileLocked()
	if !stillIn {
		sessionFileMu.Unlock()
		return AccountSession{} // 刷新期间登出了
	}
	cur.Points = r.Points
	if len(r.Tiers) > 0 {
		cur.Tiers = r.Tiers
	}
	if len(r.Features) > 0 {
		cur.Permissions = r.Features
	}
	_ = saveSessionFileLocked(cur)
	sessionFileMu.Unlock()

	sel := cur.SelectedTier
	if sel < 1 || sel > 3 {
		sel = 1
	}
	return AccountSession{LoggedIn: true, Account: cur.Account, IsAdmin: r.IsAdmin, Permissions: cur.Permissions, Tiers: cur.Tiers, Points: cur.Points, SelectedTier: sel}
}
