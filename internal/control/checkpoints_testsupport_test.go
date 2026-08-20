package control

import "reasonix/internal/checkpoint"

// 测试用的直接存取:几个 rewind / fork / branch 的回归测试需要把 checkpoint 状态摆到
// 某个特定形状(比如「压缩后边界已失效」「turn 号越界」),而不是真跑几轮 turn 去凑。
//
// 放在 _test.go 里而不是生产文件里:它们只服务测试,生产代码一律走 Begin / Bound /
// TruncateFrom 这些带不变量的入口。

func (s *checkpointService) seedBound(turn, msgIndex int) {
	s.mu.Lock()
	s.bound[turn] = msgIndex
	s.mu.Unlock()
}

func (s *checkpointService) setTurn(n int) {
	s.mu.Lock()
	s.turn = n
	s.mu.Unlock()
}

// storeForTest 取底层 store,用于直接摆 checkpoint(Begin 指定 turn 号)或读回 List。
func (s *checkpointService) storeForTest() *checkpoint.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store
}
