package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"reasonix/desktop/internal/update"
)

// updater_app.go is the auto-updater's bound command surface — the App methods the
// frontend calls — mirroring settings_app.go's "one file per concern" split. The
// transport-free logic lives in updater.go; this file is the Wails glue: it streams
// download progress as "updater:progress" events and routes macOS to the manual
// download path (an unsigned .app can't be swapped in place under Gatekeeper).

// Version returns the build version injected via -ldflags (see main.go). The
// frontend displays it; CheckUpdate compares against it.
func (a *App) Version() string { return version }

// CheckUpdate is intentionally disabled in this fork (onecreat). It is distributed
// manually and has no own update channel yet, so it must NOT phone home to the upstream
// reasonix endpoints in updater.go — doing so leaks usage and could steer users into
// installing upstream over this fork. Returns "no update" with zero network calls, so
// the frontend's UpdateBanner stays hidden. To re-enable later: restore the
// fetchManifest/evaluate path below and repoint the endpoints to onecreat's channel.
func (a *App) CheckUpdate() (*UpdateInfo, error) {
	// Web 模式(主分发形态)有自己的更新渠道(阿里云 nginx 上的 latest.json),由
	// webUpdateOverride 接管:只提示不自更新、失败静默。桌面版 override 返回 false,
	// 于是仍走下面这段禁用逻辑,行为不变(不 phone home 到上游 reasonix)。
	if info, ok := a.webUpdateOverride(); ok {
		return info, nil
	}
	return &UpdateInfo{
		Current:       version,
		Latest:        version,
		Available:     false,
		CanSelfUpdate: false,
	}, nil
}

// OpenDownloadPage opens the releases page in the browser — the macOS manual-update
// path and a fallback link elsewhere.
func (a *App) OpenDownloadPage() {
	page := defaultDownloadPage
	// Web 模式有自己的下载页(webDownloadPage);桌面版 webDownloadURL() 返回空串 → 用默认页。
	if w := webDownloadURL(); w != "" {
		a.sh().BrowserOpenURL(w)
		return
	}
	// 自更新在本 fork 已禁用(见 CheckUpdate),manifest 端点为空,fetchManifest 必然失败。
	// 仅当 manifest 端点真的配置了(将来重新接入 onecreat 自己的更新通道时)才去取它的
	// DownloadPage 覆盖,否则直接开默认发布页 —— 省掉每次点击两次必败的请求(L7)。
	if manifestPrimary != "" || manifestFallback != "" {
		if c, err := httpClient(); err == nil {
			if m, err := fetchManifest(a.reqCtx(), c); err == nil && m.DownloadPage != "" {
				page = m.DownloadPage
			}
		}
	}
	a.sh().BrowserOpenURL(page)
}

// ApplyUpdate downloads, verifies, installs the latest build, then relaunches. On
// macOS it can't self-update (unsigned bundle), so it defers to the download page.
// Progress is streamed on the "updater:progress" event; on success the process exits.
func (a *App) ApplyUpdate() error {
	if !canSelfUpdate() {
		a.OpenDownloadPage()
		return nil
	}
	c, err := httpClient()
	if err != nil {
		return a.failUpdate(err)
	}
	m, err := fetchManifest(a.reqCtx(), c)
	if err != nil {
		return a.failUpdate(err)
	}
	asset, ok := m.Asset()
	if !ok {
		return a.failUpdate(fmt.Errorf("no update artifact for %s", update.CurrentPlatform()))
	}

	data, err := a.downloadVerify(asset)
	if err != nil {
		return a.failUpdate(err)
	}

	a.emitProgress("applying", asset.Size, asset.Size, "")
	switch runtime.GOOS {
	case "windows":
		err = applyWindows(data)
	case "linux":
		err = applyLinux(data)
	default:
		err = fmt.Errorf("self-update unsupported on %s", runtime.GOOS)
	}
	if err != nil {
		return a.failUpdate(err)
	}

	a.emitProgress("done", asset.Size, asset.Size, "")

	// Persist the conversation and stop subprocesses before handing off (same as
	// shutdown). On Linux the binary is now replaced, so relaunch it; on Windows the
	// installer we launched takes over once we exit.
	a.shutdown(a.ctx)
	if runtime.GOOS == "linux" {
		_ = relaunch()
	}
	os.Exit(0)
	return nil
}

// downloadVerify downloads the asset (streaming progress), verifies its minisign
// signature against the embedded public key, then its sha256. It returns the
// verified bytes and never touches disk on a bad signature.
func (a *App) downloadVerify(asset update.Asset) ([]byte, error) {
	c, err := httpClient()
	if err != nil {
		return nil, err
	}
	data, err := download(a.reqCtx(), c, asset.URL, asset.Size, func(rcv, total int64) {
		a.emitProgress("downloading", rcv, total, "")
	})
	if err != nil {
		return nil, err
	}
	a.emitProgress("verifying", asset.Size, asset.Size, "")
	sig, err := fetchBytes(a.reqCtx(), c, asset.Sig)
	if err != nil {
		return nil, err
	}
	if err := update.Verify(data, sig); err != nil {
		return nil, err
	}
	if err := checkSHA256(data, asset.SHA256); err != nil {
		return nil, err
	}
	return data, nil
}

// reqCtx is the context for updater HTTP calls — the Wails context once startup has
// run, else Background (CheckUpdate may, in theory, be reached before startup).
func (a *App) reqCtx() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *App) emitProgress(phase string, received, total int64, errMsg string) {
	if a.ctx == nil {
		return
	}
	a.sh().Emit("updater:progress", updateProgress{
		Phase: phase, Received: received, Total: total, Err: errMsg,
	})
}

// failUpdate emits an error progress event and returns the error to the caller.
func (a *App) failUpdate(err error) error {
	a.emitProgress("error", 0, 0, err.Error())
	return err
}
