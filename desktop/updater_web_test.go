//go:build web

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebSemverNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"v1.2.0", "v1.1.0", true},
		{"v1.2.0", "v1.2.0", false},
		{"v1.1.0", "v1.2.0", false},
		{"1.2.0", "1.1.0", true},         // 允许无 v 前缀
		{"v1.2.0", "dev", false},         // 未注入版本永不提示
		{"garbage", "v1.0.0", false},     // 非法版本
		{"v2.0.0-rc", "v0.0.0-rc", true}, // prerelease 也能比
	}
	for _, c := range cases {
		if got := webSemverNewer(c.latest, c.current); got != c.want {
			t.Errorf("webSemverNewer(%q,%q)=%v, 期望 %v", c.latest, c.current, got, c.want)
		}
	}
}

// manifest 版本高于当前 → Available=true,DownloadURL 用 manifest 里的下载页。
func TestWebUpdateOverrideAvailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v99.0.0","downloadPage":"http://example.test/dl","assets":{"darwin-arm64":"http://example.test/a.tgz"}}`))
	}))
	defer srv.Close()
	t.Setenv("ONECREAT_UPDATE_MANIFEST_URL", srv.URL)

	// 测试里 version 默认是未注入的 "dev"(永不提示);临时给个真版本号验证比较逻辑。
	old := version
	version = "v1.0.0"
	defer func() { version = old }()

	a := newBareApp(nil, nil)
	info, ok := a.webUpdateOverride()
	if !ok || info == nil {
		t.Fatal("Web 模式应接管 CheckUpdate")
	}
	if !info.Available {
		t.Fatalf("v99.0.0 应判为有更新;info=%+v", info)
	}
	if info.Latest != "v99.0.0" {
		t.Errorf("Latest=%q, 期望 v99.0.0", info.Latest)
	}
	if info.CanSelfUpdate {
		t.Error("Web 版不应自更新")
	}
	if info.DownloadURL != "http://example.test/dl" {
		t.Errorf("DownloadURL=%q, 期望 manifest 里的下载页", info.DownloadURL)
	}
}

// manifest 拉不到(服务器 500)→ 静默返回无更新,绝不弹错、绝不 available。
func TestWebUpdateOverrideSilentOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("ONECREAT_UPDATE_MANIFEST_URL", srv.URL)

	a := newBareApp(nil, nil)
	info, ok := a.webUpdateOverride()
	if !ok || info == nil {
		t.Fatal("失败也应由 Web 路径接管(ok=true)")
	}
	if info.Available {
		t.Error("拉取失败时不应报告有更新")
	}
}

// fetchWebManifest 能解析最小格式。
func TestFetchWebManifest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v1.2.0","downloadPage":"http://d","assets":{"linux-amd64":"http://x"}}`))
	}))
	defer srv.Close()
	m, err := fetchWebManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchWebManifest: %v", err)
	}
	if m.Version != "v1.2.0" || m.Assets["linux-amd64"] != "http://x" {
		t.Fatalf("解析结果不对: %+v", m)
	}
}
