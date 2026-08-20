package main

// 工作区文件的 transport facade:实现在 fileService 里(见 files_service.go)。

// BrowseDir 列出一个目录,供「选择文件夹」这类浏览界面用。
func (a *App) BrowseDir(path string) FolderListing { return a.files.BrowseDir(path) }

// ListDir 列出项目内某个相对目录,供 @ 引用菜单用。
func (a *App) ListDir(rel string) []DirEntry { return a.files.ListDir(rel) }

// ReadFile 读一个项目内文件的有界预览。
func (a *App) ReadFile(rel string) FilePreview { return a.files.ReadFile(rel) }

// OpenWorkspacePath 用系统默认程序打开项目内的一个路径。
func (a *App) OpenWorkspacePath(rel string) error { return a.files.OpenWorkspacePath(rel) }

// OpenFolder 用文件管理器打开一个绝对路径。
func (a *App) OpenFolder(path string) error { return a.files.OpenFolder(path) }

// RevealWorkspacePath 在文件管理器里选中项目内的一个路径。
func (a *App) RevealWorkspacePath(rel string) error { return a.files.RevealWorkspacePath(rel) }

// SavePastedImage 把粘贴进来的图片落盘,返回可引用的路径。
func (a *App) SavePastedImage(dataURL string) (string, error) {
	return a.files.SavePastedImage(dataURL)
}

// SavePastedFile 把粘贴/拖入的附件落盘,返回可引用的路径。
func (a *App) SavePastedFile(name, dataURL string) (string, error) {
	return a.files.SavePastedFile(name, dataURL)
}

// AttachmentDataURL 把一个附件读成 data URL,供前端内联预览。
func (a *App) AttachmentDataURL(path string) (string, error) {
	return a.files.AttachmentDataURL(path)
}
