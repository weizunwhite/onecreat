package numbering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PreviewPath 是 NAS 侧的只读预告端点。
//
// 选它而不选 /api/project-code/preview 的理由:后者(main.py:4678)走 _require_admin,
// 鉴权是**浏览器会话 Cookie**(_require_login → _current_user),OneCreat 是本机程序、
// 手上没有会话;内部版(main.py:4669)走 _require_internal(main.py:2766),鉴权是
// Authorization: Bearer <CLUSTER_INTERNAL_TOKEN>,与 brain 的 photo-uploader-client.ts:371
// 走的是同一个端点、同一套 token。两者返回体完全相同。
const PreviewPath = "/api/internal/project-code/preview"

// nasDefaultTimeout 与 brain 侧 requestJson 的默认值(30s)取同一量级,但发号是交互路径,收紧到 15s。
const nasDefaultTimeout = 15 * time.Second

// nasMaxErrorBody 出错时最多读多少字节的响应体用于报错。
const nasMaxErrorBody = 8 << 10

// NAS 是迁移期(M1–M4)的发号权威适配:预告走 NAS HTTP 接口,真实占号仍在 NASApp 完成。
//
// 为什么 Issue 不通:01_M0决策记录 D-1「并行 ≠ 双入口」+ §2.2 E29 —— 真实立项(发号 +
// 七目录 + project.json + P 卡)在 M4 上线日才从 NASApp 切到 OneCreat。M1–M3 期间
// OneCreat 如果自己去打 NAS 的建档接口,就成了第二个写入口,而编号是**不可逆**数据。
type NAS struct {
	baseURL string
	token   string
	client  *http.Client
}

var _ Issuer = (*NAS)(nil)

// NewNAS 构造 NAS 适配。baseURL 形如 http://192.168.6.131:4678(尾部斜杠会被去掉),
// token 是 NAS 的 CLUSTER_INTERNAL_TOKEN。client 传 nil 时用一个带默认超时的客户端。
//
// token 只会出现在请求头里:不进日志、不进错误体(surfaceBody 还会再兜底擦一遍)。
func NewNAS(baseURL, token string, client *http.Client) *NAS {
	if client == nil {
		client = &http.Client{Timeout: nasDefaultTimeout}
	}
	return &NAS{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client:  client,
	}
}

// previewResponse 是 main.py:4669-4676 的返回体:{"ok": true, "code": "P26C-032", "line": "C"}。
type previewResponse struct {
	OK   bool   `json:"ok"`
	Code string `json:"code"`
	Line string `json:"line"`
}

// errorResponse 是 FastAPI HTTPException 的标准体:{"detail": "..."}。
type errorResponse struct {
	Detail string `json:"detail"`
}

// Preview 只读预告下一编号。号是 NAS 在 _project_codes_lock 里算的,这边不缓存、不推算。
func (n *NAS) Preview(ctx context.Context, line string) (string, error) {
	clean, err := NormalizeLine(line)
	if err != nil {
		return "", err
	}
	if n.baseURL == "" {
		return "", errors.New("NAS 发号地址未配置")
	}
	if n.token == "" {
		// 与 NAS 侧 _require_internal 的 fail-closed 同口径:没 token 就别发请求。
		return "", errors.New("NAS 内部 token 未配置，拒绝发号请求")
	}

	endpoint := n.baseURL + PreviewPath + "?" + url.Values{"line": {clean}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("构造发号预告请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+n.token)
	req.Header.Set("Accept", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		// url.Error 的文本里带的是 URL(不含 token),但仍走一遍擦除兜底。
		return "", fmt.Errorf("请求 NAS 发号预告失败: %s", n.redact(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("NAS 发号预告返回 HTTP %d: %s", resp.StatusCode, n.surfaceBody(resp.Body))
	}

	var payload previewResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, nasMaxErrorBody)).Decode(&payload); err != nil {
		return "", fmt.Errorf("NAS 发号预告返回体无法解析: %w", err)
	}
	if !payload.OK {
		return "", errors.New("NAS 发号预告返回 ok=false")
	}
	// 严口径校验:预告出来的必须是带业务线的新号,且业务线要与请求一致。
	// 号是三端身份证,格式错了宁可当场失败,也不能让它流进建档链。
	if !ValidCode(payload.Code) {
		return "", fmt.Errorf("%w:NAS 预告返回 %q", ErrBadCode, payload.Code)
	}
	if payload.Line != clean || !strings.HasPrefix(payload.Code, codePrefix+clean+"-") {
		return "", fmt.Errorf("NAS 预告的业务线与请求不一致:请求 %s，返回 %s/%s", clean, payload.Line, payload.Code)
	}
	return payload.Code, nil
}

// Issue 在迁移期(M1–M4)一律拒绝:真实立项仍走 NASApp 单入口。
//
// M4 上线日打开这条路时要接的端点是 NAS 的 POST /api/internal/project-code/next
// (main.py:4715),鉴权同 Preview(Bearer CLUSTER_INTERNAL_TOKEN),
// body {line, name, student, expected_code},返回 {"ok":true,"code":"P26C-032","line":"C"};
// 号变了对端回 409(main.py:4746),这边必须映射成 ErrCodeMoved。
// 打开的同时必须同步停用 NASApp 侧的 E29 入口,否则就成了双写。
func (n *NAS) Issue(_ context.Context, req Request) (string, error) {
	if _, err := validateRequest(req); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%w:迁移期真实立项仍走 NASApp(见 docs/科创工作流迁移/01_M0决策记录.md D-1 与 §2.2 E29)，"+
		"OneCreat 这一侧只提供 Preview", ErrNotEnabled)
}

// surfaceBody 把对端错误体读成一行人话。只取 detail 字段;读不动就退回原文截断。
func (n *NAS) surfaceBody(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, nasMaxErrorBody))
	if err != nil || len(raw) == 0 {
		return "(无响应体)"
	}
	var e errorResponse
	if json.Unmarshal(raw, &e) == nil && e.Detail != "" {
		return n.redact(e.Detail)
	}
	return n.redact(strings.TrimSpace(string(raw)))
}

// redact 兜底擦掉可能被对端回显的 token —— 硬规则:密钥不出现在日志与错误体。
func (n *NAS) redact(s string) string {
	if n.token == "" {
		return s
	}
	return strings.ReplaceAll(s, n.token, "[REDACTED]")
}
