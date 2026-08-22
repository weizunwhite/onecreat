package main

// fileService 是「工作区文件」那一片的后端:@ 引用菜单的目录列举、侧栏的只读预览、
// 用系统默认程序打开 / 在文件管理器里定位、以及粘贴图片 / 附件落盘。
//
// 它只依赖一个东西:当前选中的项目文件夹。所有相对路径都按它解析 —— 不是进程 cwd
// (那正是 Plan 01 根除的隐式状态),所以两个开在不同项目的标签各看各的文件树。

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"unicode/utf8"

	"reasonix/internal/control"
	fileenc "reasonix/internal/fileutil/encoding"
)

type fileService struct {
	rootFn func() string
}

func newFileService(root func() string) *fileService { return &fileService{rootFn: root} }

// root 是当前选中的项目文件夹;没有注入(测试里的零值 service)时退回进程 cwd。
func (fs *fileService) root() string {
	if fs == nil || fs.rootFn == nil {
		wd, _ := os.Getwd()
		return wd
	}
	return fs.rootFn()
}

// DirEntry is one entry in the "@" file-reference menu.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
}

// FilePreview is a bounded, read-only file payload for the workspace side panel.
type FilePreview struct {
	Path      string `json:"path"`
	Body      string `json:"body"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Err       string `json:"err,omitempty"`
}

// atSkip are entries the "@" menu hides as noise.
var atSkip = map[string]bool{".git": true, "node_modules": true, ".DS_Store": true}

const filePreviewLimit = 256 * 1024

func trimUTF8PartialSuffix(data []byte) []byte {
	if utf8.Valid(data) {
		return data
	}
	for i := len(data) - 1; i >= 0 && len(data)-i <= utf8.UTFMax; i-- {
		if !utf8.RuneStart(data[i]) {
			continue
		}
		if !utf8.Valid(data[:i]) || utf8.FullRune(data[i:]) {
			return data
		}
		return data[:i]
	}
	return data
}

// workspacePath resolves a frontend-supplied path against the open project and
// refuses anything that escapes it. base is that project's root — the confinement
// boundary moves with the selected workspace, not with the process.
func workspacePath(base, rel string) (string, bool, error) {
	if base == "" {
		return "", false, os.ErrInvalid
	}
	if rel == "" {
		return "", false, os.ErrInvalid
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, rel)
	}
	path = filepath.Clean(path)
	r, err := filepath.Rel(base, path)
	if err != nil {
		return "", false, err
	}
	if r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", false, os.ErrPermission
	}
	return path, true, nil
}

// ListDir lists one directory level (directories first, then files, each
// alphabetical) for the "@" file-reference menu. rel resolves against the open
// project's root; "" lists that root. The menu navigates one level at a time,
// never recursively — bounded for huge trees.
// FolderListing 是内置文件夹选择器的一页:某个绝对路径下的子文件夹列表 + 导航锚点。
type FolderListing struct {
	Path    string   `json:"path"`    // 当前绝对路径
	Parent  string   `json:"parent"`  // 上级目录(已在根则等于自身)
	Dirs    []string `json:"dirs"`    // 子文件夹名(排序后)
	Home    string   `json:"home"`    // 用户主目录(快捷入口)
	Desktop string   `json:"desktop"` // 桌面(快捷入口)
	Error   string   `json:"error,omitempty"`
}

// BrowseDir 列出某个绝对路径下的子文件夹,供 app 内置文件夹选择器导航——绕开 macOS
// 原生选择对话框在隐藏标题栏窗口下会开到窗口后面的 bug。path 为空时从主目录开始。
func (fs *fileService) BrowseDir(path string) FolderListing {
	home, _ := os.UserHomeDir()
	desktop := ""
	if home != "" {
		desktop = filepath.Join(home, "Desktop")
	}
	p := strings.TrimSpace(path)
	if p == "" {
		p = home
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return FolderListing{Path: home, Parent: home, Home: home, Desktop: desktop, Error: err.Error()}
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		// 打不开(权限/不存在)就退回上级,别让用户卡死
		return FolderListing{Path: abs, Parent: filepath.Dir(abs), Home: home, Desktop: desktop, Error: "打不开这个目录:" + err.Error()}
	}
	dirs := []string{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue // 隐藏目录不显示,减少噪音
		}
		if e.IsDir() {
			dirs = append(dirs, name)
			continue
		}
		// 软链可能指向目录
		if e.Type()&os.ModeSymlink != 0 {
			if info, serr := os.Stat(filepath.Join(abs, name)); serr == nil && info.IsDir() {
				dirs = append(dirs, name)
			}
		}
	}
	sort.Strings(dirs)
	return FolderListing{Path: abs, Parent: filepath.Dir(abs), Dirs: dirs, Home: home, Desktop: desktop}
}

func (fs *fileService) ListDir(rel string) []DirEntry {
	base := fs.root()
	if base == "" {
		return nil
	}
	dir := base
	if rel != "" {
		if filepath.IsAbs(rel) {
			dir = filepath.Clean(rel)
		} else {
			dir = filepath.Join(base, rel)
		}
	}
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var dirs, files []DirEntry
	for _, e := range es {
		name := e.Name()
		if atSkip[name] {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, DirEntry{Name: name, IsDir: true})
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, DirEntry{Name: name, IsDir: false})
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })
	sort.Slice(files, func(i, j int) bool { return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name) })
	return append(dirs, files...)
}

// ReadFile returns a small text preview for a file under the current workspace.
func (fs *fileService) ReadFile(rel string) FilePreview {
	out := FilePreview{Path: rel}
	path, ok, err := workspacePath(fs.root(), rel)
	if err != nil || !ok {
		out.Err = "invalid path"
		return out
	}
	info, err := os.Stat(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	if info.IsDir() {
		out.Err = "path is a directory"
		return out
	}
	if !info.Mode().IsRegular() {
		out.Err = "path is not a regular file"
		return out
	}
	out.Size = info.Size()
	f, err := os.Open(path)
	if err != nil {
		out.Err = err.Error()
		return out
	}
	defer f.Close()

	buf := make([]byte, filePreviewLimit+1)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		out.Err = err.Error()
		return out
	}
	data := buf[:n]
	if len(data) > filePreviewLimit {
		data = data[:filePreviewLimit]
		out.Truncated = true
	}

	// Check for BOM first (just the first 2-3 bytes — always complete
	// even at a truncation boundary). BOM-prefixed files skip the NUL
	// check since UTF-16 normally contains 0x00 for ASCII characters.
	bomKind := fileenc.DetectQuick(data)
	if bomKind != fileenc.UTF8 {
		enc, _ := fileenc.Detect(data)
		if enc == fileenc.LossyUTF8 {
			out.Binary = true
			return out
		}
		decoded := fileenc.Decode(data, enc)
		out.Body = string(decoded)
		return out
	}

	// No BOM — NUL in raw bytes is a binary signal.
	if bytes.Contains(data, []byte{0}) {
		out.Binary = true
		return out
	}

	// Trim any partial multi-byte rune at the truncation boundary BEFORE
	// encoding detection. Without this, a large UTF-8 file truncated
	// mid-character would fail utf8.Valid and be misdetected as GB18030
	// or LossyUTF8, producing mojibake or a false binary classification.
	if out.Truncated {
		data = trimUTF8PartialSuffix(data)
	}
	enc, _ := fileenc.Detect(data)
	if enc == fileenc.LossyUTF8 {
		out.Binary = true
		return out
	}
	out.Body = string(fileenc.Decode(data, enc))
	return out
}

// OpenWorkspacePath opens a file or folder from the workspace in the OS default app.
func (fs *fileService) OpenWorkspacePath(rel string) error {
	path, ok, err := workspacePath(fs.root(), rel)
	if err != nil || !ok {
		return os.ErrInvalid
	}
	return openWorkspacePath(path)
}

// OpenFolder 在系统文件管理器里打开一个绝对路径的文件夹(供侧栏「在文件夹中打开」用)。
// 与 OpenWorkspacePath 不同:不限制在当前 workspace 内,可打开任意已存在的目录。
func (fs *fileService) OpenFolder(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return os.ErrInvalid
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return os.ErrInvalid
	}
	return openWorkspacePath(path)
}

// RevealWorkspacePath shows a workspace file in the native file manager.
func (fs *fileService) RevealWorkspacePath(rel string) error {
	path, ok, err := workspacePath(fs.root(), rel)
	if err != nil || !ok {
		return os.ErrInvalid
	}
	switch goruntime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", path).Start()
	case "windows":
		return exec.Command("explorer", "/select,", path).Start()
	default:
		dir := path
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			dir = filepath.Dir(path)
		}
		return exec.Command("xdg-open", dir).Start()
	}
}

// SavePastedImage stores a browser clipboard image data URL under
// .reasonix/attachments and returns the relative @-reference path.
func (fs *fileService) SavePastedImage(dataURL string) (string, error) {
	return control.SaveImageDataURL(dataURL)
}

// SavePastedFile stores a dropped non-image file (the browser exposes its bytes
// as a data URL but not a real path) under .reasonix/attachments and returns the
// relative @-reference path.
func (fs *fileService) SavePastedFile(name, dataURL string) (string, error) {
	return control.SaveAttachmentDataURL(name, dataURL)
}

// AttachmentDataURL returns a safe data URL for a stored image attachment.
func (fs *fileService) AttachmentDataURL(path string) (string, error) {
	return control.ImageDataURL(path)
}
