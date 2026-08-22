package control

// markRunning 模拟「一轮 turn 正在进行」。测试用它去撞独占 op 的互斥守卫,不必真跑一轮。
func (t *turnState) markRunning() {
	t.mu.Lock()
	t.running = true
	t.mu.Unlock()
}
