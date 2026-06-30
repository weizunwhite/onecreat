package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// ReferenceFileResult is what Composer's "上传参考资料" button gets back from
// ImportReferenceFile. It's a flat structure the frontend folds into a pasted
// block, so the file's text becomes part of the next user message.
type ReferenceFileResult struct {
	Name       string `json:"name"`       // basename or URL
	Path       string `json:"path"`       // absolute path or original URL
	Text       string `json:"text"`       // extracted plain text
	CharCount  int    `json:"charCount"`  // characters in Text
	Truncated  bool   `json:"truncated"`  // text was cut to fit budget
	Source     string `json:"source"`     // "file" | "url"
	FormatHint string `json:"formatHint"` // "txt"/"md"/"pdf"/"docx"/"html"/...
}

const (
	refFileMaxBytes = 8 << 20 // 8MB cap on source files
	refFileMaxText  = 60_000  // ~15k tokens — fits in DeepSeek context budget
)

// PickReferenceFile pops the OS file picker so the student can choose a Word /
// PDF / HTML / Markdown / code file to attach as reference material. Returns the
// absolute path; empty string when the user cancels.
func (a *App) PickReferenceFile() (string, error) {
	if a.ctx == nil {
		return "", errors.New("file picker not ready")
	}
	cwd, _ := os.Getwd()
	return runtime.OpenFileDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "选择参考资料",
		DefaultDirectory: cwd,
		Filters: []runtime.FileFilter{
			{DisplayName: "支持的资料 (PDF, Word, HTML, Markdown, 代码, 文本)", Pattern: "*.pdf;*.doc;*.docx;*.rtf;*.html;*.htm;*.md;*.markdown;*.txt;*.csv;*.json;*.yaml;*.yml;*.toml;*.py;*.js;*.ts;*.tsx;*.jsx;*.go;*.c;*.h;*.cpp;*.hpp;*.ino;*.pde;*.sh"},
			{DisplayName: "所有文件", Pattern: "*.*"},
		},
	})
}

// ImportReferenceFile reads `pathOrURL` and returns its extracted text so the
// student can paste study material (Word/PDF/HTML/Markdown/code) into Composer
// as one "pasted block". Accepts:
//
//   - http(s):// URL  → fetch + HTML strip
//   - local file path → extension-dispatched parser
//
// Note: PDF/DOC/DOCX/RTF use macOS system tools (textutil / pdftotext) so we
// avoid adding heavy Go deps. Linux/Windows fall back to "tell the user to
// convert" until those binaries are bundled.
func (a *App) ImportReferenceFile(pathOrURL string) (ReferenceFileResult, error) {
	p := strings.TrimSpace(pathOrURL)
	if p == "" {
		return ReferenceFileResult{}, errors.New("路径或链接为空")
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return importWebPage(p)
	}
	return importLocalFile(p)
}

func importLocalFile(path string) (ReferenceFileResult, error) {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return ReferenceFileResult{}, err
	}
	if info.IsDir() {
		return ReferenceFileResult{}, errors.New("不支持文件夹,请选择具体文件")
	}
	if info.Size() > refFileMaxBytes {
		return ReferenceFileResult{}, fmt.Errorf("文件大于 8MB (%.1fMB),请拆分后上传", float64(info.Size())/1024/1024)
	}

	ext := strings.ToLower(filepath.Ext(path))
	res := ReferenceFileResult{
		Name:       filepath.Base(path),
		Path:       path,
		Source:     "file",
		FormatHint: strings.TrimPrefix(ext, "."),
	}

	text, err := extractTextByExt(path, ext)
	if err != nil {
		return res, err
	}
	res.Text, res.Truncated = clampText(text, refFileMaxText)
	res.CharCount = len([]rune(res.Text))
	if strings.TrimSpace(res.Text) == "" {
		return res, errors.New("文件解析后是空文本,可能格式不支持或文件本身没有可提取内容")
	}
	return res, nil
}

func extractTextByExt(path, ext string) (string, error) {
	switch ext {
	case ".txt", ".md", ".markdown", ".csv", ".tsv", ".log", ".json", ".jsonl",
		".yaml", ".yml", ".toml", ".xml", ".sh", ".bash", ".zsh",
		".py", ".js", ".ts", ".tsx", ".jsx", ".go", ".rs", ".java", ".kt",
		".c", ".h", ".cpp", ".hpp", ".cc", ".cxx", ".ino", ".pde", ".m", ".mm",
		".swift", ".rb", ".php", ".lua", ".sql":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return string(b), nil

	case ".html", ".htm":
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return stripHTML(string(b)), nil

	case ".pdf":
		// pdftotext 来自 poppler;macOS 学生可以 brew install poppler。
		return runConverter("pdftotext", []string{"-layout", path, "-"})

	case ".docx", ".doc", ".rtf":
		// macOS 内置 textutil,任何 Mac 都有,零依赖。
		if goruntime.GOOS != "darwin" {
			return "", errors.New("Word/RTF 解析当前只在 macOS 可用(用 textutil)。请先转为 PDF 或 TXT 后上传")
		}
		return runConverter("textutil", []string{"-convert", "txt", "-stdout", path})

	default:
		return "", fmt.Errorf("暂不支持的文件类型 %q;支持文本/代码/Markdown/HTML/PDF/Word", ext)
	}
}

func runConverter(name string, args []string) (string, error) {
	if _, err := exec.LookPath(name); err != nil {
		hint := ""
		if name == "pdftotext" {
			hint = ";macOS 装法:`brew install poppler`"
		}
		return "", fmt.Errorf("缺少 %s 命令,无法解析此文件%s", name, hint)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s 解析失败: %s", name, msg)
	}
	return stdout.String(), nil
}

// importWebPage 拉一个 URL 的 HTML,strip 出主体文本。
// 用 net/http 标准库,不依赖任何第三方。
func importWebPage(rawURL string) (ReferenceFileResult, error) {
	res := ReferenceFileResult{
		Name:       rawURL,
		Path:       rawURL,
		Source:     "url",
		FormatHint: "html",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("User-Agent", "OneCreat/reference-fetch")
	httpRes, err := http.DefaultClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("拉取网页失败: %w", err)
	}
	defer httpRes.Body.Close()
	if httpRes.StatusCode >= 400 {
		return res, fmt.Errorf("网页返回 HTTP %d", httpRes.StatusCode)
	}
	var buf bytes.Buffer
	limited := http.MaxBytesReader(nil, httpRes.Body, refFileMaxBytes)
	if _, err := buf.ReadFrom(limited); err != nil {
		return res, fmt.Errorf("读取网页内容失败: %w", err)
	}
	text := stripHTML(buf.String())
	res.Text, res.Truncated = clampText(text, refFileMaxText)
	res.CharCount = len([]rune(res.Text))
	if strings.TrimSpace(res.Text) == "" {
		return res, errors.New("网页解析后没有正文文本")
	}
	if title := extractHTMLTitle(buf.String()); title != "" {
		res.Name = title
	}
	return res, nil
}

// --- 简易 HTML strip ---
// 不用 net/html 是为了零依赖 + 对学生场景"粗略提取正文"足够。
// 删 script/style/noscript 块,再用 tag 正则去 tag,实体保留常用。

var (
	// Go 的 RE2 不支持反向引用,所以三种"裸 tag 块"分别写。
	scriptRe     = regexp.MustCompile(`(?si)<script\b[^>]*>.*?</script>`)
	styleRe      = regexp.MustCompile(`(?si)<style\b[^>]*>.*?</style>`)
	noscriptRe   = regexp.MustCompile(`(?si)<noscript\b[^>]*>.*?</noscript>`)
	tagRe        = regexp.MustCompile(`<[^>]+>`)
	titleRe      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	multiBlankRe = regexp.MustCompile(`\n[ \t]*\n([ \t]*\n)+`)
	trailingWsRe = regexp.MustCompile(`[ \t]+\n`)
)

func stripHTML(s string) string {
	s = scriptRe.ReplaceAllString(s, "")
	s = styleRe.ReplaceAllString(s, "")
	s = noscriptRe.ReplaceAllString(s, "")
	s = tagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'",
	).Replace(s)
	s = trailingWsRe.ReplaceAllString(s, "\n")
	s = multiBlankRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func extractHTMLTitle(s string) string {
	m := titleRe.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	t := strings.TrimSpace(m[1])
	if len(t) > 80 {
		t = t[:80] + "…"
	}
	return t
}

func clampText(s string, maxRunes int) (string, bool) {
	if maxRunes <= 0 {
		return s, false
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s, false
	}
	return string(runes[:maxRunes]) + "\n\n…(已截断,只取前 " + fmt.Sprintf("%d", maxRunes) + " 字)", true
}
