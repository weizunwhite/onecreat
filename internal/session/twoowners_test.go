package session

import (
	"testing"
)

// 一个进程里会同时存在两个 Registry:desktop 的 sessionService 一个,每个标签的
// control.sessionStore 又各有一个(`session.Open` 在 desktop 包内只有一处调用,
// 但 internal/control 里还有一处 —— 两处各自合法,合起来就不是了)。
//
// 之前 load() 上有个 `loaded` latch:读一次之后永不重读,而 save() 每次都把**整份**
// 内存快照写回去。于是这不是竞态,是**必然**丢更新:
//
//  1. UI 侧 registry 读到 N 条;
//  2. 新开一个标签 → 它自己的 registry 读到同样 N 条,Ensure 后写回 N+1 条;
//  3. 用户在 UI 上改个标题 → UI 侧内存里还是那 N 条,写回去 —— 第 2 步那条没了。
//
// 后果正是 Plan 11 要消除的东西:会话丢掉身份与 workspace,下次再 Ensure 会拿到一个
// 新 ID(而 workspace 是 write-once 的)。
func TestSecondOwnerDoesNotClobberTheFirst(t *testing.T) {
	dir := t.TempDir()

	ui := Open(dir)   // 相当于 desktop 的 sessionService
	tabA := Open(dir) // 相当于某个标签的 control.sessionStore
	if _, err := ui.Ensure("old.jsonl", "/proj", EngineNative); err != nil {
		t.Fatal(err)
	}
	// UI 侧先把索引读进内存(列表刷新就会做这件事)。
	if got := len(ui.List()); got != 1 {
		t.Fatalf("准备阶段应有 1 条,拿到 %d", got)
	}

	// 新标签注册了自己的会话。
	newRec, err := tabA.Ensure("new.jsonl", "/proj", EngineNative)
	if err != nil {
		t.Fatal(err)
	}

	// 之后 UI 侧改个标题 —— 它必须**不能**把新标签那条抹掉。
	// (SetTitle 的第一个参数是 store 引用,不是 ID。)
	if err := ui.SetTitle("old.jsonl", "改个名字"); err != nil {
		t.Fatal(err)
	}

	// 用第三个 registry 直接看盘上的真相,绕开任何内存缓存。
	disk := Open(dir)
	if _, ok := disk.ByStore("new.jsonl"); !ok {
		t.Fatalf("新标签的会话记录 %s 被另一个 owner 的写回抹掉了", newRec.ID)
	}
	if rec, ok := disk.ByStore("old.jsonl"); !ok || rec.Title != "改个名字" {
		t.Fatalf("标题没落盘:%+v ok=%v", rec, ok)
	}
	if n := len(disk.List()); n != 2 {
		t.Fatalf("盘上应有 2 条会话,拿到 %d", n)
	}
}
