package okx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocket 公共行情（M4）：OKX WS tickers 频道。
//
// 设计目标：
//   - 只做「行情流」一件事：连接、订阅、心跳、断线重连；不做业务缓存。
//   - 每张期权一个 tickers instId 订阅（OKX 的 tickers 不支持按
//     instFamily 批量订阅期权；opt-summary 频道只有 IV/Greeks，没有
//     bidPx，无法直接复用现有 APR 公式）。
//   - 断线后指数退避重连，重连成功自动重放全部订阅。
//
// 调用方（market.Live）负责把回调写入自己的缓存，并用锁控制并发。

const (
	wsPublicLive = "wss://ws.okx.com/ws/v5/public"
	wsPublicDemo = "wss://wspap.okx.com/ws/v5/public?brokerId=9999"

	wsPingEvery    = 20 * time.Second
	wsReadTimeout  = 60 * time.Second
	wsWriteTimeout = 5 * time.Second
	wsBatchSize    = 300 // 单次 subscribe 参数条数（远低于 64KB 上限）
)

// WSStatus 连接状态回调（Connected=false 时 Err 为断线/握手错误）。
type WSStatus struct {
	Connected bool      `json:"connected"`
	At        time.Time `json:"at"`
	Err       string    `json:"err,omitempty"`
}

// publicWSURL 实盘/模拟盘公共行情地址；wsURL 非空时优先（测试）。
func (c *Client) publicWSURL() string {
	if c.wsURL != "" {
		return c.wsURL
	}
	if c.flag == FlagDemo {
		return wsPublicDemo
	}
	return wsPublicLive
}

func (c *Client) wsDialer() *websocket.Dialer {
	d := &websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   1 << 20,
		WriteBufferSize:  1 << 16,
		// 未显式配置 --proxy 时跟随 HTTPS_PROXY / HTTP_PROXY / ALL_PROXY。
		// REST 用 http.Client 的默认 Transport，本来就吃环境变量；WS 若不同步，
		// 就会出现「行情能拉到、WS 永远重连中」的割裂（gorilla 在拨号前把
		// wss 归一成 https，所以 ProxyFromEnvironment 能正确命中）。
		Proxy: http.ProxyFromEnvironment,
	}
	if c.proxyURL != nil {
		d.Proxy = http.ProxyURL(c.proxyURL)
	}
	return d
}

// StreamTickers 持续订阅一组 instId 的 tickers，阻塞到 ctx 取消。
// onTicker 在单个读 goroutine 中串行调用，必须快速返回；onStatus 可为 nil。
//
// 注意：Ticker 期权数据的 optType/strike/expTime 不在推送里，调用方应
// 用 instruments 的 instId 关联静态字段；本方法只更新 bid/ask/last 等动态字段。
func (c *Client) StreamTickers(ctx context.Context, instIDs []string,
	onTicker func(OptionTicker), onStatus func(WSStatus)) error {

	if c.proxyErr != nil {
		return fmt.Errorf("代理配置错误: %w", c.proxyErr)
	}
	if c.proxyURL != nil && c.proxyURL.Scheme != "http" && c.proxyURL.Scheme != "socks5" {
		return fmt.Errorf("WebSocket 不支持 %s 代理（请改用 http:// 或 socks5://；REST 仍可使用 https 代理）", c.proxyURL.Scheme)
	}
	ids := uniqueNonEmpty(instIDs)
	if len(ids) == 0 {
		return errors.New("WebSocket 订阅列表为空")
	}

	backoff := time.Second
	for {
		connected, err := c.streamTickersOnce(ctx, ids, onTicker, onStatus)
		if ctx.Err() != nil {
			return nil
		}
		if !connected {
			// 握手失败：退避后重试；连接成功过则重置退避。
			backoff = maxDuration(backoff, time.Second)
		} else {
			backoff = time.Second
		}
		if onStatus != nil {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			onStatus(WSStatus{Connected: false, At: time.Now(), Err: msg})
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

// streamTickersOnce 建立一次连接并读到断开；返回 connected 表示握手是否成功。
func (c *Client) streamTickersOnce(ctx context.Context, ids []string,
	onTicker func(OptionTicker), onStatus func(WSStatus)) (bool, error) {

	d := c.wsDialer()
	conn, resp, err := d.DialContext(ctx, c.publicWSURL(), nil)
	if err != nil {
		return false, err
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	defer conn.Close()
	conn.SetReadLimit(2 << 20)

	// ctx 取消时主动关闭连接，避免 ReadMessage 一直阻塞到 60s 读超时。
	stopClose := make(chan struct{})
	defer close(stopClose)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stopClose:
		}
	}()

	if onStatus != nil {
		onStatus(WSStatus{Connected: true, At: time.Now()})
	}

	var writeMu sync.Mutex
	writeJSON := func(v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		return conn.WriteJSON(v)
	}
	writeText := func(s string) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
		return conn.WriteMessage(websocket.TextMessage, []byte(s))
	}

	// 分批订阅：单条 JSON 不超过 64KB；批间稍作停顿，避免瞬时写爆。
	for start := 0; start < len(ids); start += wsBatchSize {
		end := start + wsBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		args := make([]map[string]string, 0, end-start)
		for _, id := range ids[start:end] {
			args = append(args, map[string]string{"channel": "tickers", "instId": id})
		}
		if err := writeJSON(map[string]any{
			"id":   fmt.Sprintf("%d", start),
			"op":   "subscribe",
			"args": args,
		}); err != nil {
			return true, err
		}
		if end < len(ids) {
			select {
			case <-ctx.Done():
				return true, nil
			case <-time.After(50 * time.Millisecond):
			}
		}
	}

	// 心跳：OKX 要求 30s 内有数据/心跳，否则服务端断链。
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go func() {
		t := time.NewTicker(wsPingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingCtx.Done():
				return
			case <-t.C:
				if err := writeText("ping"); err != nil {
					return
				}
			}
		}
	}()

	for {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}
		if string(raw) == "pong" {
			continue
		}
		var env struct {
			Event string          `json:"event"`
			Arg   json.RawMessage `json:"arg"`
			Data  json.RawMessage `json:"data"`
			Code  string          `json:"code"`
			Msg   string          `json:"msg"`
		}
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		switch env.Event {
		case "subscribe", "unsubscribe", "notice":
			continue
		case "error":
			if onStatus != nil {
				onStatus(WSStatus{
					Connected: true,
					At:        time.Now(),
					Err:       fmt.Sprintf("WS 订阅/服务错误 %s: %s", env.Code, env.Msg),
				})
			}
			continue
		}
		if len(env.Data) == 0 || onTicker == nil {
			continue
		}
		var ticks []OptionTicker
		if json.Unmarshal(env.Data, &ticks) != nil {
			continue
		}
		for _, t := range ticks {
			if t.InstID != "" {
				onTicker(t)
			}
		}
	}
}

func uniqueNonEmpty(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
