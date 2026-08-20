// Package tsgen generates the frontend's Go↔TS method contract from the Go
// source that actually serves it.
//
// The desktop exposes one flat method surface (Wails bindings and the Web mode's
// POST /rpc/<Method> both dispatch by name), and the browser side needs a
// TypeScript interface for it. That interface used to be typed out by hand —
// 120-odd methods mirrored from app.go, kept in step by a test that could only
// compare *names*, never signatures. A parameter reordered or a return type
// changed on the Go side drifted silently until it broke at runtime.
//
// So the interface is generated from two things that are already the truth:
//
//   - rpcPublicMethods (desktop/rpc_surface.go) — the explicit allowlist that
//     decides what is public at all. Generating from it, rather than from "every
//     exported method", keeps the deliberate opt-in: adding a method to *App
//     still does not publish it.
//   - the methods' Go signatures and doc comments.
//
// Everything is read from source with go/ast rather than reflection, so the
// generator is a plain library: `desktop/cmd/gen-bindings` writes the file and
// the package's own test regenerates it in memory to fail on staleness.
package tsgen

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// OutputFile is the generated file's path, relative to the desktop package dir.
const OutputFile = "frontend/src/lib/bindings.generated.ts"

// Param is one method argument.
type Param struct {
	Name string
	Type string // TypeScript
}

// Method is one entry of the browser-facing API.
type Method struct {
	Name   string
	Doc    []string // doc comment lines, "//" stripped
	Params []Param
	Result string // TypeScript type, "void" when the method returns nothing useful
}

// Generate renders the bindings file for the desktop package rooted at dir.
func Generate(dir string) ([]byte, error) {
	methods, err := Collect(dir)
	if err != nil {
		return nil, err
	}
	return Render(methods), nil
}

// Collect reads the package's source and returns the public API surface: the
// methods on *App whose names appear in rpcPublicMethods, in sorted order.
func Collect(dir string) ([]Method, error) {
	files, err := parseDir(dir)
	if err != nil {
		return nil, err
	}
	allowed, err := allowlist(files)
	if err != nil {
		return nil, err
	}
	byName := map[string]Method{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
				continue
			}
			star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			if id, ok := star.X.(*ast.Ident); !ok || id.Name != "App" {
				continue
			}
			if !allowed[fn.Name.Name] {
				continue
			}
			m, err := method(fn)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fn.Name.Name, err)
			}
			byName[m.Name] = m
		}
	}
	var missing []string
	for name := range allowed {
		if _, ok := byName[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("rpc_surface.go 允许了 %v,但 *App 上没有这些方法", missing)
	}
	out := make([]Method, 0, len(byName))
	for _, m := range byName {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func parseDir(dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		out = append(out, f)
	}
	return out, nil
}

// allowlist reads the keys of rpcPublicMethods.
func allowlist(files []*ast.File) (map[string]bool, error) {
	out := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "rpcPublicMethods" {
					continue
				}
				lit, ok := vs.Values[0].(*ast.CompositeLit)
				if !ok {
					return nil, fmt.Errorf("rpcPublicMethods 不是复合字面量")
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.BasicLit)
					if !ok || key.Kind != token.STRING {
						continue
					}
					name, err := strconv.Unquote(key.Value)
					if err != nil {
						return nil, err
					}
					out[name] = true
				}
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("找不到 rpcPublicMethods")
	}
	return out, nil
}

func method(fn *ast.FuncDecl) (Method, error) {
	m := Method{Name: fn.Name.Name, Result: "void"}
	if fn.Doc != nil {
		for _, c := range fn.Doc.List {
			line := strings.TrimPrefix(c.Text, "//")
			m.Doc = append(m.Doc, strings.TrimPrefix(line, " "))
		}
	}
	for _, field := range fn.Type.Params.List {
		ts, err := tsType(field.Type)
		if err != nil {
			return m, err
		}
		if len(field.Names) == 0 {
			m.Params = append(m.Params, Param{Name: fmt.Sprintf("arg%d", len(m.Params)), Type: ts})
			continue
		}
		for _, n := range field.Names {
			m.Params = append(m.Params, Param{Name: lowerFirst(n.Name), Type: ts})
		}
	}
	results := flatten(fn.Type.Results)
	switch len(results) {
	case 0:
	case 1:
		// A lone error carries no payload: the RPC layer turns it into a
		// rejected promise, so TypeScript sees Promise<void>.
		if !isError(results[0]) {
			ts, err := tsType(results[0])
			if err != nil {
				return m, err
			}
			m.Result = ts
		}
	case 2:
		if !isError(results[1]) {
			return m, fmt.Errorf("两个返回值时第二个必须是 error")
		}
		ts, err := tsType(results[0])
		if err != nil {
			return m, err
		}
		m.Result = ts
	default:
		return m, fmt.Errorf("返回值多于两个,RPC 层不支持")
	}
	return m, nil
}

func flatten(fl *ast.FieldList) []ast.Expr {
	if fl == nil {
		return nil
	}
	var out []ast.Expr
	for _, f := range fl.List {
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, f.Type)
		}
	}
	return out
}

func isError(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "error"
}

// tsType maps a Go type to its TypeScript spelling. Named struct types keep
// their name: they are mirrored in frontend/src/lib/types.ts, and the import
// list below is derived from whichever ones a signature actually mentions.
func tsType(e ast.Expr) (string, error) {
	switch v := e.(type) {
	case *ast.Ident:
		switch v.Name {
		case "string":
			return "string", nil
		case "bool":
			return "boolean", nil
		case "int", "int8", "int16", "int32", "int64",
			"uint", "uint8", "uint16", "uint32", "uint64",
			"float32", "float64":
			return "number", nil
		case "any":
			return "unknown", nil
		}
		if v.IsExported() {
			return v.Name, nil
		}
		return "", fmt.Errorf("不认识的类型 %q —— 浏览器侧看不到未导出类型", v.Name)
	case *ast.ArrayType:
		elem, err := tsType(v.Elt)
		if err != nil {
			return "", err
		}
		return elem + "[]", nil
	case *ast.MapType:
		key, err := tsType(v.Key)
		if err != nil {
			return "", err
		}
		if key != "string" {
			return "", fmt.Errorf("JSON 对象的键必须是 string,得到 %s", key)
		}
		val, err := tsType(v.Value)
		if err != nil {
			return "", err
		}
		return "Record<string, " + val + ">", nil
	case *ast.StarExpr:
		inner, err := tsType(v.X)
		if err != nil {
			return "", err
		}
		return inner + " | null", nil
	default:
		return "", fmt.Errorf("无法映射的类型 %T", e)
	}
}

// jsReserved are JavaScript keywords that cannot be identifiers. Go happily
// names a parameter `in`; TypeScript would not parse it.
var jsReserved = map[string]bool{
	"in": true, "new": true, "class": true, "function": true, "default": true,
	"delete": true, "for": true, "if": true, "var": true, "let": true,
	"const": true, "return": true, "typeof": true, "void": true, "with": true,
	"this": true, "case": true, "catch": true, "do": true, "else": true,
	"enum": true, "export": true, "extends": true, "import": true, "super": true,
	"switch": true, "throw": true, "try": true, "while": true, "instanceof": true,
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	// Go 里习惯写 tabID,TS 侧惯例是 tabId。参数名不影响结构化类型,只影响可读性。
	if strings.HasSuffix(s, "ID") {
		s = s[:len(s)-2] + "Id"
	}
	s = strings.ToLower(s[:1]) + s[1:]
	if jsReserved[s] {
		s += "_"
	}
	return s
}

// Render emits the TypeScript file.
func Render(methods []Method) []byte {
	imports := map[string]bool{}
	note := func(ts string) {
		ts = strings.TrimSuffix(ts, "[]")
		ts = strings.TrimSuffix(ts, " | null")
		if inner, ok := strings.CutPrefix(ts, "Record<string, "); ok {
			ts = strings.TrimSuffix(inner, ">")
		}
		ts = strings.TrimSuffix(ts, "[]")
		if ts == "" {
			return
		}
		if c := ts[0]; c >= 'A' && c <= 'Z' {
			imports[ts] = true
		}
	}
	for _, m := range methods {
		for _, p := range m.Params {
			note(p.Type)
		}
		note(m.Result)
	}
	var names []string
	for n := range imports {
		names = append(names, n)
	}
	sort.Strings(names)

	var b bytes.Buffer
	b.WriteString("// Code generated by desktop/cmd/gen-bindings; DO NOT EDIT.\n")
	b.WriteString("//\n")
	b.WriteString("// 这是 Go 侧 *App 的浏览器可见方法集(desktop/rpc_surface.go 的 allowlist)\n")
	b.WriteString("// 逐条翻译出来的 TypeScript 契约。改 Go 签名后运行:\n")
	b.WriteString("//\n")
	b.WriteString("//   cd desktop && go generate ./...\n")
	b.WriteString("//\n")
	b.WriteString("// desktop 的 TestFrontendBindingsAreUpToDate 会在这份文件过期时报错。\n")
	b.WriteString("\n")
	if len(names) > 0 {
		b.WriteString("import type {\n")
		for _, n := range names {
			fmt.Fprintf(&b, "  %s,\n", n)
		}
		b.WriteString("} from \"./types\";\n\n")
	}
	b.WriteString("// AppBindings 是三种壳(Wails 绑定 / Web 模式的 POST /rpc/<方法名> / 裸浏览器 mock)\n")
	b.WriteString("// 共同实现的接口,组件代码因此完全不知道自己跑在哪种壳里。\n")
	b.WriteString("export interface AppBindings {\n")
	for i, m := range methods {
		if i > 0 {
			b.WriteString("\n")
		}
		for _, line := range m.Doc {
			if line == "" {
				b.WriteString("  //\n")
				continue
			}
			fmt.Fprintf(&b, "  // %s\n", line)
		}
		params := make([]string, 0, len(m.Params))
		for _, p := range m.Params {
			params = append(params, p.Name+": "+p.Type)
		}
		fmt.Fprintf(&b, "  %s(%s): Promise<%s>;\n", m.Name, strings.Join(params, ", "), m.Result)
	}
	b.WriteString("}\n")
	return b.Bytes()
}
