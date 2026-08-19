//go:build web

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Web 模式的更新检查。
//
// 与 Wails 桌面版不同:桌面版 CheckUpdate 是刻意禁用的(见 updater_app.go 注释——防止 phone
// home 到上游 reasonix)。Web 模式是 onecreat 自己的主分发形态,有自己的更新渠道(阿里云
// nginx 上的 latest.json),所以这里恢复「只检查、只提示、不自更新」的最小更新流程。
//
// 这两个变量走 ldflags 注入(-X main.webUpdateManifestURL=… / -X main.webDownloadPage=…),
// 默认值就是发行渠道地址;只在 web 标签构建里存在,Wails 版行为完全不变(仍禁用)。
var (
	// webUpdateManifestURL 是 latest.json 的地址(UPDATE_CHANNEL = 阿里云 nginx)。
	webUpdateManifestURL = "http://47.95.176.214/onecreat/latest.json"
	// webDownloadPage 是「去下载」落地页(GitHub Releases 作镜像页,人可读)。
	webDownloadPage = "https://github.com/weizunwhite/onecreat/releases/latest"
)

// webManifest 是 latest.json 的最小格式(见 docs/Web模式.md):
//
//	{"version":"v1.2.0","downloadPage":"http://47.95.176.214/onecreat/",
//	 "assets":{"darwin-arm64":"…url…","windows-amd64":"…url…",…}}
//
// Web 版只提示不自更新,所以 assets 这里不是必需的(留着供人和将来自更新用)。
type webManifest struct {
	Version      string            `json:"version"`
	DownloadPage string            `json:"downloadPage"`
	Assets       map[string]string `json:"assets"`
}

// webManifestURL 取实际要拉的 manifest 地址:优先环境变量 ONECREAT_UPDATE_MANIFEST_URL
// (给验收/本地假 manifest 用),否则用 ldflags 注入的默认地址。
func webManifestURL() string {
	if v := strings.TrimSpace(os.Getenv("ONECREAT_UPDATE_MANIFEST_URL")); v != "" {
		return v
	}
	return strings.TrimSpace(webUpdateManifestURL)
}

// fetchWebManifest 拉取并解析 latest.json,5s 超时。
func fetchWebManifest(ctx context.Context, url string) (*webManifest, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "OneCreat-web/update-check")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("manifest HTTP %d", resp.StatusCode)
	}
	var m webManifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// webUpdateOverride 是 Web 模式下的 CheckUpdate 实现。返回 (info, true) 表示由 Web 路径
// 接管;失败一律「静默返回无更新」——绝不弹错、绝不 fall through 到上游禁用路径。
func (a *App) webUpdateOverride() (*UpdateInfo, bool) {
	base := &UpdateInfo{Current: version, Latest: version, Available: false, CanSelfUpdate: false, DownloadURL: webDownloadPage}
	url := webManifestURL()
	if url == "" {
		return base, true
	}
	m, err := fetchWebManifest(a.reqCtx(), url)
	if err != nil || m == nil {
		return base, true // 静默:拉不到就当无更新
	}
	page := strings.TrimSpace(m.DownloadPage)
	if page == "" {
		page = webDownloadPage
	}
	info := &UpdateInfo{
		Current:       version,
		Latest:        strings.TrimSpace(m.Version),
		Available:     webSemverNewer(m.Version, version),
		CanSelfUpdate: false, // Web 版只提示,不替换自身
		DownloadURL:   page,
	}
	if info.Latest == "" {
		info.Latest = version
	}
	return info, true
}

// webSemverNewer 报告 latest 是否严格新于 current。任一不是合法 semver(如未注入的 "dev")
// 都返回 false —— dev 构建永不提示更新。
func webSemverNewer(latest, current string) bool {
	lv, lok := normalizeVersion(latest)
	cv, cok := normalizeVersion(current)
	if !lok || !cok {
		return false
	}
	return semver.Compare(lv, cv) > 0
}

// webDownloadURL 供 OpenDownloadPage 在 Web 模式下打开下载页。
func webDownloadURL() string { return strings.TrimSpace(webDownloadPage) }
