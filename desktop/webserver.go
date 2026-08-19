package main

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// Web 模式的 HTTP 服务:同一个 *App,换一层传输。
//
// 路由:
//
//	GET  /            嵌入的 React 前端(SPA,任何未命中的路径回落到 index.html)
//	POST /rpc/<方法>   通用反射 RPC(见 rpc.go)
//	GET  /events      SSE 事件流(见 eventstream.go)
//
// 安全模型(见 docs/Web模式.md):
//  1. 默认只绑 127.0.0.1,不给局域网;
//  2. 启动时生成一次性随机 token,浏览器 URL 带 ?token=…,前端存进 sessionStorage
//     并从地址栏抹掉,之后 /rpc 用 Authorization: Bearer,/events 用 ?token=;
//  3. Host 头必须是本服务自己的 host:port —— 挡 DNS rebinding(恶意域名解析到
//     127.0.0.1 后用浏览器当跳板打本地服务);
//  4. 带 Origin 的请求要求 Origin 与 Host 同源 —— 挡跨站表单/脚本发起的 CSRF。
//
// 静态资源不校验 token(浏览器加载 bundle 时还没有 header 可带),但同样受 Host
// 守卫;bundle 本身不含任何用户数据。

// webIndexMarker 注入到 index.html 里,让 bridge.ts 知道自己跑在 Web 模式。
const webIndexMarker = `<script>window.__ONECREAT_WEB__=true;</script>`

type webServer struct {
	app    *App
	rpc    *rpcServer
	events *eventBroadcaster

	token     string
	assets    fs.FS
	indexHTML []byte
	version   string

	port         string
	allowedHosts map[string]bool
	// anyHost 在显式绑定非回环地址时置位:此时用户就是要从别的机器按 IP/域名访问,
	// Host 白名单没法预知,退回到只靠 token + Origin 同源。
	anyHost bool
}

// newWebServer 组装服务。assets 必须是「前端 dist 的根」(即直接含 index.html)。
func newWebServer(app *App, assets fs.FS, events *eventBroadcaster, token, host string, port int, anyHost bool, version string) (*webServer, error) {
	index, err := fs.ReadFile(assets, "index.html")
	if err != nil {
		return nil, fmt.Errorf("读取前端 index.html 失败(是否忘了 pnpm build?): %w", err)
	}

	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	if h := strings.ToLower(strings.TrimSpace(host)); h != "" {
		allowed[h] = true
	}

	return &webServer{
		app:          app,
		rpc:          newRPCServer(app),
		events:       events,
		token:        token,
		assets:       assets,
		indexHTML:    injectWebMarker(index),
		version:      version,
		port:         strconv.Itoa(port),
		allowedHosts: allowed,
		anyHost:      anyHost,
	}, nil
}

// injectWebMarker 在 </head> 前插入模式标记;没有 </head> 就前置到文件头,
// 保证它一定先于 bundle 执行。
func injectWebMarker(index []byte) []byte {
	if bytes.Contains(index, []byte(webIndexMarker)) {
		return index
	}
	if i := bytes.Index(index, []byte("</head>")); i >= 0 {
		out := make([]byte, 0, len(index)+len(webIndexMarker))
		out = append(out, index[:i]...)
		out = append(out, []byte(webIndexMarker)...)
		out = append(out, index[i:]...)
		return out
	}
	return append([]byte(webIndexMarker), index...)
}

// Handler 返回带全部守卫的顶层 handler。
func (s *webServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/rpc/", s.requireToken(s.rpc))
	mux.Handle("/events", s.requireToken(http.HandlerFunc(s.events.serveSSE)))
	// /healthz 不校验 token —— 供「再次启动」的探活用(单实例守卫,见 singleinstance.go)。
	// 只回 {ok,version},不含任何用户数据;同样受 guardHost 守卫,只有本机回环能连。
	mux.HandleFunc("/healthz", s.serveHealthz)
	mux.HandleFunc("/", s.serveStatic)
	return s.guardHost(mux)
}

// guardHost 挡 DNS rebinding:请求的 Host 必须是本服务自己的 host:port。
func (s *webServer) guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeRPCError(w, http.StatusForbidden, "Host 不被允许")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *webServer) hostAllowed(hostHeader string) bool {
	if hostHeader == "" {
		return false
	}
	if s.anyHost {
		return true
	}
	h, p, err := net.SplitHostPort(hostHeader)
	if err != nil {
		h, p = hostHeader, ""
	}
	if p != "" && p != s.port {
		return false
	}
	return s.allowedHosts[strings.ToLower(strings.Trim(h, "[]"))]
}

// originAllowed:无 Origin(curl / EventSource 之外的本地工具)放行;有 Origin 则必须同源。
func originAllowed(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// requireToken 校验一次性 token:优先 Authorization: Bearer,其次 ?token=
// (EventSource 无法自定义 header,只能走查询参数)。
func (s *webServer) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !originAllowed(r) {
			writeRPCError(w, http.StatusForbidden, "跨源请求被拒绝")
			return
		}
		got := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		} else {
			got = r.URL.Query().Get("token")
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			writeRPCError(w, http.StatusUnauthorized, "token 无效或缺失")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveStatic 提供嵌入的前端。未命中的路径回落到 index.html(SPA)。
func (s *webServer) serveStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRPCError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" || name == "." || name == "index.html" {
		s.serveIndex(w, r)
		return
	}
	data, err := fs.ReadFile(s.assets, name)
	if err != nil {
		s.serveIndex(w, r) // SPA 回落
		return
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// serveHealthz 是无鉴权探活端点:另一个 onecreat-web 进程再次启动时,用它确认「已有实例
// 还活着」,活着就复用、把浏览器开到已有实例(见 singleinstance.go)。只回最小信息。
func (s *webServer) serveHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeRPCError(w, http.StatusMethodNotAllowed, "只接受 GET")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `{"ok":true,"version":%q}`, s.version)
}

func (s *webServer) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(s.indexHTML))
}

// newWebToken 生成一次性访问 token(每次启动都不同,不落盘)。
func newWebToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
