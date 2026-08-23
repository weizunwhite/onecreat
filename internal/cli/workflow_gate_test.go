package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gateFixture 造一个沙箱项目根,返回门禁和根路径。
func gateFixture(t *testing.T) (*materialGate, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "P26C-020_飞鸟志")
	if err := os.MkdirAll(filepath.Join(root, "02_平台材料"), 0o755); err != nil {
		t.Fatal(err)
	}
	return newMaterialGate(root), root
}

func args(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestMaterialGateMatrix 钉死材料作业门禁的 allow/deny 矩阵(M2 拍板 1)。
//
// 这张表就是口径本身:改门禁必须先改这张表,否则"从紧"会在某次重构里悄悄变松。
func TestMaterialGateMatrix(t *testing.T) {
	g, root := gateFixture(t)
	outside := filepath.Join(filepath.Dir(root), "别的地方.md")

	cases := []struct {
		name      string
		tool      string
		args      map[string]any
		readOnly  bool
		wantAllow bool
	}{
		// —— 工作区内的文件读写:allow ——
		{"写项目内文件", "write_file", map[string]any{"path": "02_平台材料/技术方案.md", "content": "x"}, false, true},
		{"写项目内文件(绝对路径)", "write_file", map[string]any{"path": filepath.Join(root, "02_平台材料/a.md")}, false, true},
		{"改项目内文件", "edit_file", map[string]any{"path": "02_平台材料/技术方案.md"}, false, true},
		{"批量改项目内文件", "multi_edit", map[string]any{"file_path": "02_平台材料/技术方案.md"}, false, true},
		{"读项目内文件", "read_file", map[string]any{"path": "01_立项定题/立项确认单.md"}, true, true},
		{"列项目内目录", "ls", map[string]any{"path": "02_平台材料"}, true, true},
		{"无路径参数的检索", "grep", map[string]any{"pattern": "鸟"}, true, true},

		// —— 工作区外的路径:deny(不是 ask) ——
		{"写工作区外", "write_file", map[string]any{"path": outside}, false, false},
		{"用 .. 逃逸", "write_file", map[string]any{"path": "../别的地方.md"}, false, false},
		{"读系统文件", "read_file", map[string]any{"path": "/etc/hosts"}, true, false},
		{"读家目录 SSH 私钥", "read_file", map[string]any{"path": "~/.ssh/id_rsa"}, true, false},

		// —— dsh 引擎那套同名工具:同一口径 ——
		{"dsh 写项目内文件", "write", map[string]any{"file_path": "02_平台材料/技术方案.md", "content": "x"}, false, true},
		{"dsh 写工作区外", "write", map[string]any{"file_path": outside}, false, false},
		{"dsh 读项目内文件", "read", map[string]any{"filePath": "02_平台材料/技术方案.md"}, true, true},
		{"dsh 读系统文件", "read", map[string]any{"filePath": "/etc/hosts"}, true, false},

		// —— 记账工具:allow ——
		{"记 todo", "todo_write", map[string]any{"todos": []any{}}, true, true},
		{"签收步骤", "complete_step", map[string]any{"step": "写技术方案"}, true, true},

		// —— bash:只有 mkdir/ls,且路径在内 ——
		{"建项目内目录", "bash", map[string]any{"command": "mkdir -p 02_平台材料/图表"}, false, true},
		{"看项目内目录", "bash", map[string]any{"command": "ls -la 03_研发工作区"}, true, true},
		{"白名单外命令", "bash", map[string]any{"command": "curl https://example.com"}, false, false},
		{"cat 也不在白名单", "bash", map[string]any{"command": "cat 02_平台材料/a.md"}, true, false},
		{"用 && 串命令绕白名单", "bash", map[string]any{"command": "mkdir x && curl https://example.com"}, false, false},
		{"用管道", "bash", map[string]any{"command": "ls | wc -l"}, true, false},
		{"用命令替换", "bash", map[string]any{"command": "mkdir $(whoami)"}, false, false},
		{"mkdir 到工作区外", "bash", map[string]any{"command": "mkdir /tmp/onecreat-越界"}, false, false},

		// —— 其余一律 deny(哪怕它是只读工具)——
		{"联网抓取", "web_fetch", map[string]any{"url": "https://example.com"}, true, false},
		{"派子代理", "task", map[string]any{"prompt": "去查点资料"}, false, false},
		{"向用户提问", "ask", map[string]any{"questions": []any{}}, true, false},
		{"跑别的 skill", "run_skill", map[string]any{"name": "x"}, false, false},
		{"删代码段", "delete_range", map[string]any{"path": "02_平台材料/a.md"}, false, false},
		{"任意 MCP 工具", "mcp__hardware__arduino_upload", map[string]any{}, false, false},
		{"没听说过的工具", "自造工具", map[string]any{}, true, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allow, reason, err := g.Check(context.Background(), c.tool, args(t, c.args), c.readOnly)
			if err != nil {
				t.Fatalf("门禁不该返回 error:%v", err)
			}
			if allow != c.wantAllow {
				t.Fatalf("%s(%v) allow=%v want %v(reason=%s)", c.tool, c.args, allow, c.wantAllow, reason)
			}
			if !allow && strings.TrimSpace(reason) == "" {
				t.Fatal("拒绝必须给模型一句人话原因,否则它只会原样重试")
			}
		})
	}
}

// TestMaterialGateNeverAsks 是拍板 1 的直接守卫:门禁只有两种答案。
//
// permission.Gate 在 headless 下会把 ask 退化成 allow(preserve autonomy),那正是
// 材料作业不能接受的口径。这里从接口形状上确认:Check 只返回 (bool, reason, err),
// 没有第三态,也不会因为"没人应答"就放行——上面矩阵里的越界项全部是 false。
func TestMaterialGateNeverAsks(t *testing.T) {
	g, _ := gateFixture(t)
	// 一个既非白名单、又"只读"的工具:permission.Policy 的兜底会 Allow 它。
	allow, _, err := g.Check(context.Background(), "web_fetch", args(t, map[string]any{"url": "https://example.com"}), true)
	if err != nil {
		t.Fatal(err)
	}
	if allow {
		t.Fatal("只读的网络工具也必须被拒:材料作业不许联网")
	}
}

// TestMaterialGateBlocksSymlinkEscape:项目目录里放一个指向外面的软链,
// 顺着它写文件同样算越界。
func TestMaterialGateBlocksSymlinkEscape(t *testing.T) {
	g, root := gateFixture(t)
	outsideDir := t.TempDir()
	link := filepath.Join(root, "外链")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("本平台建不了软链:%v", err)
	}
	allow, _, err := g.Check(context.Background(), "write_file",
		args(t, map[string]any{"path": "外链/偷渡.md", "content": "x"}), false)
	if err != nil {
		t.Fatal(err)
	}
	if allow {
		t.Fatal("经软链写到项目目录外必须被拒")
	}
}
