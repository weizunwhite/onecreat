package boot

import (
	"testing"

	"reasonix/internal/plugin"
)

// C1 的完整性缺口,我自己漏的一处。
//
// `PluginSpecsIn` 给配置里的插件装上了本工作区的环境,但有两类 spec 是在那之后才被
// **追加**进来的:
//
//   - codegraph —— 装配时解析到二进制才注入,是「又一个 stdio 插件」;
//   - `opts.ExtraPlugins` —— ACP 客户端在 `session/new` 里指名的服务器。
//
// 它们的 `BaseEnv` 是 nil,于是退回进程环境。C1 之前那没问题(`.env` 就在进程环境里),
// C1 之后就成了静默失效:同一个会话里,配置里的 MCP 服务器拿得到项目 `.env`,这两类
// 拿不到。而这正是我在 boot.go 那条注释里写下的失败形态 ——「少给一处,那一处的 `.env`
// 就静默失效」。
func TestLateAddedPluginSpecsAlsoGetTheWorkspaceEnv(t *testing.T) {
	childEnv := []string{"C1_LATE=ws-value"}

	fromConfig := []plugin.Spec{{Name: "configured", BaseEnv: childEnv}}
	// 装配后期追加的两类:BaseEnv 空。
	lateAdded := []plugin.Spec{{Name: "codegraph"}, {Name: "acp-supplied"}}
	// 调用方显式给过环境的,不许被覆盖。
	explicit := []plugin.Spec{{Name: "explicit", BaseEnv: []string{"OTHER=1"}}}

	applyChildEnv(childEnv, fromConfig, lateAdded, explicit)

	for _, s := range lateAdded {
		got, ok := lookupIn(s.BaseEnv, "C1_LATE")
		if !ok || got != "ws-value" {
			t.Errorf("插件 %q 的子进程环境里没有工作区的值(拿到 %q,存在=%v)—— 它会静默退回进程环境", s.Name, got, ok)
		}
	}
	if got, _ := lookupIn(explicit[0].BaseEnv, "OTHER"); got != "1" {
		t.Error("调用方显式给的 BaseEnv 被覆盖了")
	}
	if _, ok := lookupIn(explicit[0].BaseEnv, "C1_LATE"); ok {
		t.Error("显式 BaseEnv 不该被追加工作区的键 —— 那是调用方的决定")
	}
	if got, _ := lookupIn(fromConfig[0].BaseEnv, "C1_LATE"); got != "ws-value" {
		t.Error("配置来源的 spec 被改坏了")
	}
}

// 没有叠加层的工作区(绝大多数情况)不该凭空造一份 BaseEnv —— nil 就是「继承进程环境」,
// 那是改动前的行为,也是 CLI / ACP 这类进程级前端该有的行为。
func TestNoOverlayLeavesBaseEnvNil(t *testing.T) {
	specs := []plugin.Spec{{Name: "p"}}
	applyChildEnv(nil, specs)
	if specs[0].BaseEnv != nil {
		t.Fatalf("没有工作区叠加层时 BaseEnv 应保持 nil,拿到 %v", specs[0].BaseEnv)
	}
}
