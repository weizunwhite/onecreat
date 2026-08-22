package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 把内置 UI 的能力门逻辑**真跑一遍**,而不是只匹配字符串。
//
// 字符串守卫能确认"这几根线还在",但确认不了逻辑对不对 —— 布尔取反、属性名写错、
// 漏掉某个入口,它一个都发现不了。这里抽出 `CAP_GATED` / `applyCapabilities` /
// `gatedIsBlocked` 三段,配一个最小的 DOM 桩,在 node 里跑真实断言。
//
// 如实说明:**没有 node 就跳过**,所以它不在 Go 那条 CI 腿的覆盖里。它的价值在开发机上
// —— 改这段逻辑时立刻会红,而不是等到有人在浏览器里发现按钮点不动或该禁的没禁。
func TestEmbeddedUICapabilityGateActuallyWorks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node —— 这条是开发机上的行为验证,不是 CI 覆盖")
	}

	ui := string(indexHTML)
	start := strings.Index(ui, "const CAP_GATED=[")
	end := strings.Index(ui, "function resync(reason){")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("抽不出能力门那一段 —— 内置 UI 的结构变了,这条守卫需要跟着改")
	}
	logic := ui[start:end]

	script := `
// 最小 DOM 桩:只实现被测那段真正用到的几个方法。
const nodes = {};
function makeNode(){
  const attrs = {}, classes = new Set();
  return {
    attrs, classes,
    classList: { toggle: (c, on) => { on ? classes.add(c) : classes.delete(c); } },
    setAttribute: (k, v) => { attrs[k] = v; },
    removeAttribute: (k) => { delete attrs[k]; },
    getAttribute: (k) => (k in attrs ? attrs[k] : null),
  };
}
for (const id of ['btn-new','btn-compact','btn-rewind','btn-tree']) nodes[id] = makeNode();
const $ = (sel) => nodes[sel.replace('#','')] || null;

` + logic + `

const fail = (m) => { console.error('FAIL: ' + m); process.exitCode = 1; };

// 1. 一个什么都不支持的引擎:四个入口全部禁用,且都带原因。
applyCapabilities({streaming:true, resume:false, fork:false}, 'dsh');
for (const id of Object.keys(nodes)) {
  if (nodes[id].getAttribute('aria-disabled') !== 'true') fail(id + ' 没有被禁用');
  if (!nodes[id].classes.has('is-unsupported')) fail(id + ' 少了视觉状态');
  const title = nodes[id].getAttribute('title');
  if (!title || !title.includes('dsh')) fail(id + ' 没说明原因(title=' + title + ')');
  if (!gatedIsBlocked(id)) fail(id + ' 的点击拦截没生效');
}

// 2. 只支持 resume:new/compact 放行,rewind/tree 仍禁用。分组必须真的按能力走。
applyCapabilities({resume:true, fork:false}, 'half');
if (gatedIsBlocked('btn-new')) fail('resume 可用时 btn-new 仍被挡');
if (gatedIsBlocked('btn-compact')) fail('resume 可用时 btn-compact 仍被挡');
if (!gatedIsBlocked('btn-rewind')) fail('fork 不可用时 btn-rewind 应被挡');
if (!gatedIsBlocked('btn-tree')) fail('fork 不可用时 btn-tree 应被挡');

// 3. 全支持:恢复原状,title 必须清掉 —— 否则换会话后留着一句过时的解释。
applyCapabilities({resume:true, fork:true}, 'native');
for (const id of Object.keys(nodes)) {
  if (gatedIsBlocked(id)) fail(id + ' 在全支持的引擎上仍被挡');
  if (nodes[id].getAttribute('title') !== null) fail(id + ' 留下了过时的 title');
  if (nodes[id].classes.has('is-unsupported')) fail(id + ' 留下了过时的视觉状态');
}

// 4. 没有能力表(老服务端 / 快照取失败):不动它 —— 宁可可点,也不要凭空全禁掉。
applyCapabilities(undefined, '');
for (const id of Object.keys(nodes)) {
  if (gatedIsBlocked(id)) fail(id + ' 在没有能力表时被禁掉了');
}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("内置 UI 的能力门行为不对:\n%s(err=%v)", out, err)
	}
}
