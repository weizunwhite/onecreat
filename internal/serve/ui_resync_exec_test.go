package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 把内置 UI 的**重新对齐**逻辑真跑一遍(AR-R07 / B2)。
//
// PR #26 修掉了一个真的洞:`resync()` 是异步的,取快照期间到达的帧会被渲染、随后被
// `renderHistory` 抹掉,而快照里没有它们。当时给这个修复配的只是字符串守卫 —— 我在
// 那条守卫里也如实写了「这是接线检查,不是行为测试」。
//
// 字符串匹配确认不了的东西恰恰是这个修复的全部要害:缓冲里的帧有没有按序号重放、
// `<= 快照序号` 的有没有被丢掉、缓冲满了会不会静默丢状态。这里抽出 `resync` 与
// `onmessage` 两段,配一个记录型的桩,在 node 里真跑。
//
// 与能力门那条一样:**没有 node 就跳过**,不在 Go 那条 CI 腿的覆盖里。
func TestEmbeddedUIResyncBufferingActuallyWorks(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node —— 这条是开发机上的行为验证,不是 CI 覆盖")
	}

	ui := string(indexHTML)
	resyncSrc := slice(t, ui, "function resync(reason){", "const es=new EventSource(")
	onMsgSrc := slice(t, ui, "es.onmessage=ev=>{", "es.onerror=")
	// 被测的是那个赋值右边的函数,不是 EventSource 本身。
	onMsgSrc = strings.Replace(onMsgSrc, "es.onmessage=", "const onmessage=", 1)

	script := `
// ── 被测逻辑的协作者,全部换成记录型的桩 ──
let lastSeq=0, streamId=null, resyncing=false, resyncBuf=null, resyncBufFull=false, planMode=false, resyncGen=0;
const RESYNC_BUF_MAX=4;   // 压到 4:缓冲溢出因此是确定的,不必造几千条

let applied=[];           // applyEvent 收到的帧,按顺序
let renderedHistory=0;    // renderHistory 被调用了几次 = 对齐了几次
let pendingResolve=null;  // 手动控制 /snapshot 什么时候返回 —— 那个窗口正是 bug 所在

function applyEvent(e){ if(typeof e.sequence==='number'&&e.sequence>lastSeq)lastSeq=e.sequence; applied.push(e); }
function renderHistory(){ renderedHistory++; applied=[]; }  // 真实实现会清空界面
function setRunning(){} function updateModeButtons(){} function showApproval(){} function showAsk(){}
function fetchStatus(){} function applyCapabilities(){} function scrollDown(){}
function el(){ return {}; }
const log={appendChild(){}};
function fetch(){ return new Promise(res=>{ pendingResolve=res; }); }

` + resyncSrc + onMsgSrc + `

const fail=(m)=>{ console.error('FAIL: '+m); process.exitCode=1; };
const snap=(seq)=>({json:()=>Promise.resolve({streamId:'s1',sequence:seq,history:[],running:false,plan:false,pendingApprovals:[],pendingAsks:[],capabilities:{}})});
const frame=(seq)=>({data:JSON.stringify({sequence:seq,durable:true,kind:'notice',text:'e'+seq})});

(async () => {
  // ── 1. 取快照期间到达的帧:必须缓冲,而且不能在快照应用前就被渲染 ──
  lastSeq=10;
  const p=resync('test');
  if(!resyncing) fail('resync 没有进入对齐状态');
  onmessage(frame(11)); onmessage(frame(12));
  if(applied.length!==0) fail('取快照期间的帧被直接渲染了 —— renderHistory 随后就会抹掉它们');

  // 快照截止到 10:11 与 12 是它之后的,必须被重放回来。
  pendingResolve(snap(10)); await p;
  if(renderedHistory!==1) fail('快照没有被应用');
  const seqs=applied.map(e=>e.sequence);
  if(seqs.join(',')!=='11,12') fail('重放结果是 ['+seqs+'],want [11,12] —— 缓冲里的帧丢了或乱序了');
  if(lastSeq!==12) fail('lastSeq='+lastSeq+',重放之后应当推进到 12');

  // ── 2. 快照已经包含的帧必须丢弃,否则就是重复 ──
  applied=[]; lastSeq=10; resyncing=false; resyncBuf=null;
  const p2=resync('test');
  onmessage(frame(9)); onmessage(frame(10)); onmessage(frame(11));
  pendingResolve(snap(10)); await p2;
  const seqs2=applied.map(e=>e.sequence);
  if(seqs2.join(',')!=='11') fail('重放结果是 ['+seqs2+'],want [11] —— 快照已含的帧被重复应用了');

  // ── 3. 缓冲满了不许静默丢:必须再对齐一次,拿一份更新的快照 ──
  applied=[]; renderedHistory=0; lastSeq=0; resyncing=false; resyncBuf=null; resyncBufFull=false;
  const p3=resync('test');
  for(let i=1;i<=RESYNC_BUF_MAX+3;i++) onmessage(frame(i));
  if(!resyncBufFull) fail('缓冲已经超过上限,却没有被标记为满');
  const first=pendingResolve; first(snap(0)); await p3;
  // 第一次对齐之后应当立刻又发起一次(而不是重放半截缓冲)。
  if(applied.length!==0) fail('缓冲溢出后仍然重放了半截缓冲 —— 那正是「有洞」的形态');
  if(!resyncing) fail('缓冲溢出后没有再对齐一次 —— 中间那些状态帧就此消失');
  pendingResolve(snap(99));
  await new Promise(r=>setTimeout(r,0));
  if(renderedHistory!==2) fail('第二次对齐没有真的发生(renderHistory 调用了 '+renderedHistory+' 次)');
})();
`
	dir := t.TempDir()
	path := filepath.Join(dir, "resync.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("内置 UI 的重新对齐行为不对:\n%s(err=%v)", out, err)
	}
}

// slice 取 [from, to) 之间那段源码;取不到就是 UI 结构变了,守卫必须跟着改而不是静默失效。
func slice(t *testing.T, s, from, to string) string {
	t.Helper()
	a := strings.Index(s, from)
	b := strings.Index(s, to)
	if a < 0 || b < 0 || b <= a {
		t.Fatalf("抽不出 %q…%q 之间那段 —— 内置 UI 的结构变了,这条守卫需要跟着改", from, to)
	}
	return s[a:b]
}
