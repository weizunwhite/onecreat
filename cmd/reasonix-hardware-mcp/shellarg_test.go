package main

import (
	"strings"
	"testing"
)

// F1 回归:进入 `bash -lc` 的参数必须用 POSIX 单引号转义,使 $()/反引号/;/& 等都被当作
// 字面量,不被 shell 解释执行。
func TestShellArgNeutralizesShellMetacharacters(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"command substitution", "esp32$(touch /tmp/PWNED)"},
		{"backticks", "esp32`touch /tmp/PWNED`"},
		{"semicolon", "esp32; touch /tmp/PWNED"},
		{"ampersand", "esp32 & touch /tmp/PWNED"},
		{"pipe", "esp32 | tee /tmp/PWNED"},
		{"variable", "esp32$HOME"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shellArg(tc.in)
			// 含元字符的值必须被单引号包裹。
			if !strings.HasPrefix(got, "'") || !strings.HasSuffix(got, "'") {
				t.Fatalf("shellArg(%q) = %q, want single-quoted", tc.in, got)
			}
			// 单引号内只有 '\'' 这一种转义;不能出现「裸露的」$(/反引号/;/& 之外的解释。
			// 关键不变量:原值里的单引号都被 '\'' 替换,其余字节原样保留在引号内。
			inner := strings.TrimSuffix(strings.TrimPrefix(got, "'"), "'")
			want := strings.ReplaceAll(tc.in, "'", `'\''`)
			if inner != want {
				t.Fatalf("shellArg(%q) inner = %q, want %q", tc.in, inner, want)
			}
		})
	}
}

// F1:整条 ESP-IDF 脚本里,恶意 target 必须以单引号字面量出现,绝不会出现可被 bash
// 求值的 "$(touch ...)"(双引号内仍会展开)。
func TestESPIDFShellScriptQuotesMaliciousArg(t *testing.T) {
	env := espIDFEnv{Python: "python3", IDFPath: "/opt/esp-idf"}
	script := env.ESPIDFShellScript([]string{"-p", "esp32$(touch /tmp/PWNED)", "monitor"}, "")
	if strings.Contains(script, `"esp32$(touch /tmp/PWNED)"`) {
		t.Fatalf("malicious arg rendered in double quotes (bash would expand $()): %s", script)
	}
	if !strings.Contains(script, `'esp32$(touch /tmp/PWNED)'`) {
		t.Fatalf("malicious arg not single-quoted as a literal: %s", script)
	}
}

// F1:单引号本身也要被正确转义,不能逃逸出引号。
func TestShellArgEscapesEmbeddedSingleQuote(t *testing.T) {
	got := shellArg("a'b")
	if got != `'a'\''b'` {
		t.Fatalf("shellArg(\"a'b\") = %q, want %q", got, `'a'\''b'`)
	}
}
