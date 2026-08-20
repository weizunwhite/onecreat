package main

// sessions.go 是历史会话面板与 internal/session 注册表之间的薄适配层。
//
// Plan 11 之前,这里有四份并列的侧车文件(.titles/.display/.cwds/.kinds),每份一套
// load/save 样板、都按 transcript 文件名索引、都只有桌面端会写。一个会话的「身份」
// 就是它的文件名,删一个会话要做四次读-改-写,而 CLI / ACP 起的会话根本没有任何元数据。
//
// 现在身份与元数据只有一个所有者:internal/session.Registry。transcript 本身仍归引擎
// (agent 包)所有 —— 注册表存的是一个它从不解析的 store 引用。

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"

	"reasonix/internal/session"
)

// errActiveSession is returned when a delete targets the session in use.
var errActiveSession = errors.New("can't delete the session you're in — start a new one first")

// 下面这些函数都显式收一个 *session.Registry,而不是自己按目录现开一个。
//
// 这不是风格问题:注册表把「读全量 → 改 → 原子写回」串行化在自己的锁里,而原子 rename
// 只保证文件不损坏、不防丢更新。两个标签几乎同时记 display 映射时,如果各拿一个实例,
// 就各有一把锁 —— 后写覆盖先写,丢一条映射(这正是从前 sidecarMu 挡住的事)。
// 桌面端的元数据写入全部经 sessionService 持有的那**一个**实例。

// rememberSessionCwd 记录会话所属项目(写入一次即定:会话归属哪个文件夹应当稳定,
// 即便用户中途切换 workspace)。
func rememberSessionCwd(r *session.Registry, sessionPath, cwd string) error {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(cwd) == "" {
		return nil
	}
	if _, err := r.Ensure(sessionPath, cwd, session.EngineNative); err != nil {
		return err
	}
	return r.SetWorkspace(sessionPath, cwd)
}

// rememberSessionKind 给某会话打类型标(写入一次即定)。会话默认是普通对话,只有真正用过
// 某垂直定制表面(如硬件工作台跑了编译/烧录/生成代码)才打标,供历史侧栏区分「硬件项目 /
// 普通对话」。打开硬件视图是切 mainView(同一 tab),tab.kind 仍是 chat,所以必须由前端在
// 真正干活时显式 MarkSessionKind,不能靠 tab 类型推断。
func rememberSessionKind(r *session.Registry, sessionPath, kind string) error {
	if strings.TrimSpace(sessionPath) == "" || strings.TrimSpace(kind) == "" {
		return nil
	}
	if _, err := r.Ensure(sessionPath, "", session.EngineNative); err != nil {
		return err
	}
	return r.SetKind(sessionPath, kind)
}

// setSessionTitle sets (or, with an empty title, clears) a session's custom name.
func setSessionTitle(r *session.Registry, sessionPath, title string) error {
	if _, err := r.Ensure(sessionPath, "", session.EngineNative); err != nil {
		return err
	}
	return r.SetTitle(sessionPath, title)
}

// deleteSessionFile 删掉引擎的 transcript,再让注册表忘掉这条记录。顺序是有意的:
// 注册表从不碰它不拥有的 store,所以文件由这里删、记录由它删。
func deleteSessionFile(r *session.Registry, sessionPath string) error {
	if err := os.Remove(sessionPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.Forget(sessionPath)
}

// messageDisplayKey 是一条消息内容的稳定指纹,用作 display 映射的键。
func messageDisplayKey(content string) string {
	sum := sha256.Sum256([]byte(content))
	return fmt.Sprintf("%x", sum[:])
}

// recordSessionDisplay 记住某条消息该显示成什么(粘贴附件的原始载荷 → 短标签)。
func recordSessionDisplay(r *session.Registry, sessionPath, content, display string) error {
	if strings.TrimSpace(sessionPath) == "" || content == display || strings.TrimSpace(display) == "" {
		return nil
	}
	if _, err := r.Ensure(sessionPath, "", session.EngineNative); err != nil {
		return err
	}
	return r.RecordDisplay(sessionPath, messageDisplayKey(content), display)
}

// sessionDisplayResolver 取一次记录并返回一个逐条解析器,免得 N 条消息读 N 次盘。
func sessionDisplayResolver(r *session.Registry, sessionPath string) func(content string) string {
	var byHash map[string]string
	if rec, ok := r.ByStore(sessionPath); ok {
		byHash = rec.Display
	}
	return func(content string) string {
		if byHash != nil {
			if display := byHash[messageDisplayKey(content)]; strings.TrimSpace(display) != "" {
				return display
			}
		}
		return content
	}
}

func resolveSessionDisplay(r *session.Registry, sessionPath, content string) string {
	return sessionDisplayResolver(r, sessionPath)(content)
}
