package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
)

// Web 模式的通用 RPC 端点。
//
// Wails 会把 App 的 143 个导出方法生成成 JS 绑定;浏览器模式下没有绑定生成器,
// 于是走一条反射分发的通用路由:POST /rpc/<方法名>,body 是 JSON 数组(位置参数,
// 与 Wails 的调用语义一致),按方法的形参类型逐个 Unmarshal。
//
// 返回值约定(覆盖 App 现有的全部签名形态):
//
//	()            → {"result": null}
//	(T)           → {"result": T}
//	(error)       → 错误则 500 + {"error": …},否则 {"result": null}
//	(T, error)    → 错误则 500 + {"error": …},否则 {"result": T}
//
// 这样加一个 App 方法不需要加一行路由代码 —— 与 Wails 绑定的可维护性一致。

// rpcExcluded 是不暴露给前端的方法名。App 的生命周期回调(startup/domReady/
// shutdown)本就是非导出方法、不会进反射方法集;这里额外挡一道,防止将来有人把它们
// 改成导出后被 HTTP 直接调用。
var rpcExcluded = map[string]bool{
	"Startup":     true,
	"DomReady":    true,
	"Shutdown":    true,
	"BeforeClose": true,
}

// rpcMaxBody 是单次调用的请求体上限。前端最大的一次调用是「粘贴的参考资料正文」
// (ImportReferenceFile 上限 60k 字符)与 SaveDoc,16MB 留足余量。
const rpcMaxBody = 16 << 20

// rpcServer 在构造时把 *App 的导出方法索引成表,之后每次请求只做一次 map 查找。
type rpcServer struct {
	methods map[string]reflect.Value
}

// newRPCServer 索引 target 的导出方法。target 实际上总是 *App;参数放宽成 any
// 只是为了让分发/解码逻辑能用一个小的假对象单测,不必构造真 controller。
func newRPCServer(target any) *rpcServer {
	v := reflect.ValueOf(target)
	t := v.Type()
	methods := make(map[string]reflect.Value, t.NumMethod())
	for i := 0; i < t.NumMethod(); i++ {
		name := t.Method(i).Name
		if rpcExcluded[name] {
			continue
		}
		methods[name] = v.Method(i)
	}
	return &rpcServer{methods: methods}
}

// methodNames 供测试与启动日志使用。
func (s *rpcServer) methodNames() []string {
	out := make([]string, 0, len(s.methods))
	for name := range s.methods {
		out = append(out, name)
	}
	return out
}

var errType = reflect.TypeOf((*error)(nil)).Elem()

// ServeHTTP 处理 POST /rpc/<方法名>。
func (s *rpcServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeRPCError(w, http.StatusMethodNotAllowed, "只接受 POST")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/rpc/")
	if name == "" || strings.Contains(name, "/") {
		writeRPCError(w, http.StatusNotFound, "未知方法")
		return
	}
	fn, ok := s.methods[name]
	if !ok {
		writeRPCError(w, http.StatusNotFound, "未知方法: "+name)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rpcMaxBody))
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, "读取请求体失败: "+err.Error())
		return
	}
	args, err := decodeRPCArgs(fn.Type(), body)
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, err.Error())
		return
	}

	out, err := callRPC(name, fn, args)
	if err != nil {
		writeRPCError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeRPCResult(w, out)
}

// decodeRPCArgs 把 JSON 数组按形参类型解成 reflect.Value 列表。
func decodeRPCArgs(ft reflect.Type, body []byte) ([]reflect.Value, error) {
	if ft.IsVariadic() {
		return nil, fmt.Errorf("不支持可变参数方法")
	}
	var raw []json.RawMessage
	trimmed := strings.TrimSpace(string(body))
	if trimmed != "" {
		if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
			return nil, fmt.Errorf("参数必须是 JSON 数组: %v", err)
		}
	}
	if len(raw) != ft.NumIn() {
		return nil, fmt.Errorf("参数个数不对: 期望 %d,收到 %d", ft.NumIn(), len(raw))
	}
	args := make([]reflect.Value, ft.NumIn())
	for i := 0; i < ft.NumIn(); i++ {
		p := reflect.New(ft.In(i))
		if err := json.Unmarshal(raw[i], p.Interface()); err != nil {
			return nil, fmt.Errorf("第 %d 个参数解码失败: %v", i+1, err)
		}
		args[i] = p.Elem()
	}
	return args, nil
}

// callRPC 调用方法并把返回值折叠成 (结果, 错误)。panic 被兜住转成错误,
// 免得一个方法的 bug 打掉整个本地服务进程。
func callRPC(name string, fn reflect.Value, args []reflect.Value) (result any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("web rpc: 方法 panic", "method", name, "panic", rec)
			err = fmt.Errorf("%s 执行出错: %v", name, rec)
		}
	}()

	outs := fn.Call(args)
	ft := fn.Type()
	switch ft.NumOut() {
	case 0:
		return nil, nil
	case 1:
		if ft.Out(0) == errType {
			if e, _ := outs[0].Interface().(error); e != nil {
				return nil, e
			}
			return nil, nil
		}
		return outs[0].Interface(), nil
	case 2:
		if ft.Out(1) != errType {
			return nil, fmt.Errorf("%s 的返回签名不受支持", name)
		}
		if e, _ := outs[1].Interface().(error); e != nil {
			return nil, e
		}
		return outs[0].Interface(), nil
	default:
		return nil, fmt.Errorf("%s 的返回签名不受支持", name)
	}
}

func writeRPCResult(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	body, err := json.Marshal(map[string]any{"result": result})
	if err != nil {
		writeRPCError(w, http.StatusInternalServerError, "结果序列化失败: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func writeRPCError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
