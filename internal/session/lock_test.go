package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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

// —— 跨进程:锁的**真正**目标 ——
//
// 上面几条用的是同一个进程里的两个 Registry 实例。复核明确要求「至少一个测试使用两个
// 进程或可控 barrier 复现交错」—— 这不是吹毛求疵:进程内的两个实例即便没有文件锁,也
// 常常因为调度而侥幸不撞;而 Desktop + CLI 同时开着才是这个 bug 最常发生的场景,那条
// 路径上唯一的保护就是 lockfile。没有跨进程用例,等于这把锁的主要用途从未被验证过。
//
// 做法:把测试二进制自己当成第二个进程(经环境变量分派),让它对同一个索引目录写入。

const (
	childEnvDir   = "REASONIX_TEST_REGISTRY_DIR"
	childEnvCount = "REASONIX_TEST_REGISTRY_N"
	childEnvTag   = "REASONIX_TEST_REGISTRY_TAG"
)

// TestMain 让这个包的测试二进制可以被当作「写索引的子进程」复用。
func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnvDir); dir != "" {
		n, _ := strconv.Atoi(os.Getenv(childEnvCount))
		tag := os.Getenv(childEnvTag)
		r := Open(dir)
		for i := 0; i < n; i++ {
			if _, err := r.Ensure(filepath.Join(dir, fmt.Sprintf("%s%02d.jsonl", tag, i)), dir, "native"); err != nil {
				fmt.Fprintln(os.Stderr, "child:", err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestTwoProcessesDoNotLoseEachOthersRecords(t *testing.T) {
	dir := t.TempDir()
	const n = 30

	start := func(tag string) *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(),
			childEnvDir+"="+dir,
			childEnvCount+"="+strconv.Itoa(n),
			childEnvTag+"="+tag,
		)
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("拉起子进程 %s: %v", tag, err)
		}
		return cmd
	}

	a, b := start("p"), start("q")
	for _, c := range []*exec.Cmd{a, b} {
		if err := c.Wait(); err != nil {
			t.Fatalf("子进程失败: %v", err)
		}
	}

	got := Open(dir).List()
	if len(got) != 2*n {
		t.Fatalf("两个**进程**各写 %d 条,磁盘上应有 %d 条,拿到 %d —— 跨进程写回互相覆盖了",
			n, 2*n, len(got))
	}
}
