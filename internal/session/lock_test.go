package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// AR-R09:两个**独立的 Registry owner**(desktop 的 sessionService 一个、每个标签的
// sessionStore 又各有一个;Desktop + CLI 更是两个进程)并发写同一份索引时,不得丢记录。
//
// 原来的测试只覆盖同一个 Registry 实例上的多个 goroutine —— 那由 r.mu 挡住了,测不到
// 真正的问题。这里必须造两个实例。
func TestTwoOwnersDoNotLoseEachOthersRecords(t *testing.T) {
	dir := t.TempDir()
	a, b := Open(dir), Open(dir)

	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, err := a.Ensure(filepath.Join(dir, fmt.Sprintf("a%02d.jsonl", i)), dir, "native"); err != nil {
				t.Error(err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := b.Ensure(filepath.Join(dir, fmt.Sprintf("b%02d.jsonl", i)), dir, "native"); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	// 用第三个 owner 从磁盘读:两边写的记录必须一条不少。
	got := Open(dir).List()
	if len(got) != 2*n {
		t.Fatalf("应有 %d 条记录,拿到 %d —— 两个 owner 互相覆盖了写回", 2*n, len(got))
	}
}

// 同一条记录被两个 owner 交替改名,最后必须是某一次真实的写入,而不是把记录改没了。
func TestTwoOwnersUpdatingOneRecordKeepItIntact(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "s.jsonl")
	if _, err := Open(dir).Ensure(store, dir, "native"); err != nil {
		t.Fatal(err)
	}
	a, b := Open(dir), Open(dir)

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(2)
		go func(i int) { defer wg.Done(); _ = a.SetTitle(store, fmt.Sprintf("A-%d", i)) }(i)
		go func(i int) { defer wg.Done(); _ = b.RecordDisplay(store, "k", fmt.Sprintf("B-%d", i)) }(i)
	}
	wg.Wait()

	rec, ok := Open(dir).ByStore(store)
	if !ok {
		t.Fatal("记录被并发更新弄丢了")
	}
	if rec.Title == "" {
		t.Error("标题被另一个 owner 的写回抹掉了")
	}
	if rec.Display["k"] == "" {
		t.Error("display 被另一个 owner 的写回抹掉了")
	}
}

// 持锁进程被 kill -9 之后锁文件会留下。不接管它,这台机器上的会话元数据就永久只读了。
func TestStaleLockIsTakenOver(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	lock := r.lockPath()
	if err := os.WriteFile(lock, []byte("99999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleAfter)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.Ensure(filepath.Join(dir, "s.jsonl"), dir, "native")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("陈旧锁应被接管:%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("陈旧锁没有被接管 —— 一次崩溃会让元数据永久只读")
	}
}

// 新鲜的锁必须被尊重:拿不到就等,等不到才放行(并记 warn),而不是直接无视。
func TestFreshLockIsRespectedThenReleased(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	release, err := r.acquireLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(r.lockPath()); err != nil {
		t.Fatalf("持锁期间锁文件应存在:%v", err)
	}
	// 第二次获取必须等待(而不是立刻成功)。
	start := time.Now()
	go func() { time.Sleep(50 * time.Millisecond); release() }()
	release2, err := r.acquireLock()
	if err != nil {
		t.Fatalf("锁释放后应能取得:%v", err)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Error("第二次获取没有等待前一个持有者")
	}
	release2()
	if _, err := os.Stat(r.lockPath()); !os.IsNotExist(err) {
		t.Error("释放后锁文件应被删除")
	}
}

// 锁文件不能被当成一条会话记录。
func TestLockFileIsNotMistakenForASession(t *testing.T) {
	dir := t.TempDir()
	r := Open(dir)
	if _, err := r.Ensure(filepath.Join(dir, "s.jsonl"), dir, "native"); err != nil {
		t.Fatal(err)
	}
	for _, rec := range Open(dir).List() {
		if filepath.Base(rec.Store) == lockFile {
			t.Fatal("锁文件被登记成了会话")
		}
	}
}
