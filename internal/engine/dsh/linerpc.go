package dsh

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// jsonrpc 版本常量。
const jsonrpcVersion = "2.0"

// rpcResponse 是一帧出站响应。目前只用来回错误 —— 我们不实现任何入站业务方法
// (见 readLoop 的第三种入站帧)。
type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Error   *rpcError `json:"error"`
}

// rpcRequest 是一帧出站请求(有 id)或通知(无 id)。
type rpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      *int64      `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
}

// rpcFrame 是一帧入站消息:响应(有 id)或通知(有 method、无 id)。
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// rpcError 是 JSON-RPC 错误对象。
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e *rpcError) Error() string {
	return fmt.Sprintf("dsh jsonrpc error %d: %s", e.Code, e.Message)
}

// NotificationHandler 处理一帧 server→client 通知。method 是通知名,params 是原始载荷。
type NotificationHandler func(method string, params json.RawMessage)

// LineClient 是 newline-delimited JSON-RPC 2.0 客户端,一帧一行,over 任意 io 流。
// 一个读循环 goroutine 把响应投递给等待中的请求、把通知投递给 handler。并发安全。
type LineClient struct {
	w   io.Writer
	enc *json.Encoder

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcFrame
	closed  bool
	writeMu sync.Mutex

	onNotify NotificationHandler
	readErr  chan error
}

// NewLineClient 在给定读写流上建客户端。start() 后台读;调用方负责关闭底层流。
func NewLineClient(r io.Reader, w io.Writer, onNotify NotificationHandler) *LineClient {
	c := &LineClient{
		w:        w,
		enc:      json.NewEncoder(w),
		pending:  make(map[int64]chan rpcFrame),
		onNotify: onNotify,
		readErr:  make(chan error, 1),
	}
	go c.readLoop(r)
	return c
}

// readLoop 逐行读入站帧,分派响应/通知。畸形 JSON 行忽略(协议要求)。
func (c *LineClient) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 单帧上限 16MB(工具输出可能很大)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f rpcFrame
		if err := json.Unmarshal(line, &f); err != nil {
			continue // 畸形行忽略
		}
		switch {
		case f.ID != nil && (f.Result != nil || f.Error != nil):
			c.deliver(*f.ID, f)
		case f.ID != nil && f.Method != "":
			// 入站**请求**(既有 id 又有 method)。今天 sidecar 从不发请求 —— 需要
			// Go 回话的地方一律用"带 id 的通知对"(见 protocol.go)。但若上游哪天
			// 开始发,静默按通知处理 = 它永远等不到回复 = 挂死。回 -32601 让它快速
			// 失败。这里**不**实现任何业务方法:要加方法得先想清楚它属不属于这条 wire。
			c.respondError(*f.ID, -32601, "method not found: "+f.Method)
		case f.Method != "":
			if c.onNotify != nil {
				c.onNotify(f.Method, f.Params)
			}
		}
	}
	err := sc.Err()
	if err == nil {
		err = io.EOF
	}
	c.failAll(err)
	select {
	case c.readErr <- err:
	default:
	}
}

// deliver 把一帧响应投给等待的请求 channel。
func (c *LineClient) deliver(id int64, f rpcFrame) {
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- f
	}
}

// failAll 在读流断开时拒绝所有等待中的请求。
func (c *LineClient) failAll(err error) {
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

// Notify 发一帧通知(无 id,不等回复)。
func (c *LineClient) Notify(method string, params interface{}) error {
	return c.write(rpcRequest{JSONRPC: jsonrpcVersion, Method: method, Params: params})
}

// Call 发一帧请求并等待响应;result 非 nil 时把结果 JSON 解到它。ctx 可取消等待。
func (c *LineClient) Call(ctx context.Context, method string, params, result interface{}) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errors.New("dsh jsonrpc: client closed")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcFrame, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(rpcRequest{JSONRPC: jsonrpcVersion, ID: &id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case f, ok := <-ch:
		if !ok {
			return errors.New("dsh jsonrpc: connection lost before response")
		}
		if f.Error != nil {
			return f.Error
		}
		if result != nil && f.Result != nil {
			return json.Unmarshal(f.Result, result)
		}
		return nil
	}
}

// respondError 回一帧错误响应。best-effort:写失败说明流已经断了,那边的等待
// 反正也会因为 EOF 结束,没有可做的补救。
func (c *LineClient) respondError(id int64, code int, msg string) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.enc.Encode(rpcResponse{
		JSONRPC: jsonrpcVersion,
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg},
	})
}

// write 串行化一帧到底层流(一帧一行,json.Encoder 自带换行)。
func (c *LineClient) write(v rpcRequest) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.enc.Encode(v)
}

// Wait 返回读循环终止的原因(EOF 或读错误)。
func (c *LineClient) Wait() <-chan error { return c.readErr }
