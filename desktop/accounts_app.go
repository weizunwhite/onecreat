package main

// 账号 / 权限体系(P1:登录 + 按权限门控)。当前是 mock 后端:内置几个演示账号,
// 登录成功后把会话(账号 / 是否超管 / 功能权限)持久化到配置目录,客户端据此显示/门控功能。
//
// 接真后端(P2)时只改 accountAuthenticate:换成"POST 登录接口拿 token + GET 权限接口
// 拿功能清单",其余(持久化、门控、前端)一律不动。AI 走网关(P3)再用 Token 配 provider。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// allFeatureKeys 是全部可被门控的功能 key(超管拥有全部)。和前端首页卡片 key + 各功能
// 入口一一对应:没在权限清单里的,客户端不显示 / 点不动。
var allFeatureKeys = []string{
	"hardware",  // 硬件项目 / 硬件编程工作台
	"proposal",  // 技术方案
	"paper",     // 竞赛论文
	"lesson",    // 课程教案
	"tutorial",  // 教师辅导手册
	"log",       // 研究日志
	"jinpeng",   // 金鹏材料
	"knowledge", // 知识库
	"ota",       // OTA 远程烧录
	"skills",    // MCP 与技能
}

// AccountSession 是发给前端的会话(不含 token)。
type AccountSession struct {
	LoggedIn    bool     `json:"loggedIn"`
	Account     string   `json:"account"`
	IsAdmin     bool     `json:"isAdmin"`
	Permissions []string `json:"permissions"`
}

// AccountLoginResult 是登录返回。
type AccountLoginResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// persistedSession 落盘的会话(含 token,内部用,不发前端)。
type persistedSession struct {
	Account     string   `json:"account"`
	IsAdmin     bool     `json:"isAdmin"`
	Permissions []string `json:"permissions"`
	Token       string   `json:"token"`
}

func accountSessionPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "onecreat", "session.json"), nil
}

// platformBaseURL 是 teacher 平台地址(onecreat 登录/权限/AI 网关都连它)。默认生产域名,
// 可用 ONECREAT_PLATFORM_URL 覆盖(本地联调指向 http://127.0.0.1:3000)。
func platformBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("ONECREAT_PLATFORM_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://t.weizunxy.com"
}

// accountAuthenticate 调 teacher 平台 /api/onecreat/login(手机号+密码)拿 token + 功能权限。
// 返回会话和错误信息(errMsg=="" 表示成功)。这是"问后端要账号+权限"的唯一入口。
func accountAuthenticate(account, password string) (persistedSession, string) {
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
		OK       bool     `json:"ok"`
		Token    string   `json:"token"`
		Account  string   `json:"account"`
		IsAdmin  bool     `json:"isAdmin"`
		Features []string `json:"features"`
		Error    string   `json:"error"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	if !r.OK || r.Token == "" {
		if strings.TrimSpace(r.Error) != "" {
			return persistedSession{}, r.Error
		}
		return persistedSession{}, "登录失败"
	}
	return persistedSession{Account: r.Account, IsAdmin: r.IsAdmin, Permissions: r.Features, Token: r.Token}, ""
}

// AccountLogin 校验账号密码,成功则持久化会话。
func (a *App) AccountLogin(account, password string) AccountLoginResult {
	sess, errMsg := accountAuthenticate(account, password)
	if errMsg != "" {
		return AccountLoginResult{Error: errMsg}
	}
	path, err := accountSessionPath()
	if err != nil {
		return AccountLoginResult{Error: err.Error()}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return AccountLoginResult{Error: err.Error()}
	}
	b, err := json.Marshal(sess)
	if err != nil {
		return AccountLoginResult{Error: err.Error()}
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return AccountLoginResult{Error: err.Error()}
	}
	return AccountLoginResult{OK: true}
}

// AccountLogout 清除会话。
func (a *App) AccountLogout() {
	if path, err := accountSessionPath(); err == nil {
		_ = os.Remove(path)
	}
}

// AccountSessionInfo 返回当前会话(未登录则 LoggedIn=false)。token 不外泄。
func (a *App) AccountSessionInfo() AccountSession {
	path, err := accountSessionPath()
	if err != nil {
		return AccountSession{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return AccountSession{}
	}
	var p persistedSession
	if json.Unmarshal(b, &p) != nil || p.Token == "" {
		return AccountSession{}
	}
	return AccountSession{LoggedIn: true, Account: p.Account, IsAdmin: p.IsAdmin, Permissions: p.Permissions}
}
