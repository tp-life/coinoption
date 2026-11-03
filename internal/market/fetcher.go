package market

import (
	"context"
	"sync"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

// Fetcher main → ui 的适配器。
//
// 参数（阈值/期限/资金）可运行时调整：Settings 页改完立即生效，
// 下一次刷新即用新参数重算决策。锁保护并发刷新与参数写入。
//
// UseWS=true 时使用 market.Live（OKX WebSocket 实时行情）；
// false 时保持旧行为：REST 拉取 + 15s 本地缓存。
type Fetcher struct {
	C     *okx.Client
	UseWS bool

	mu           sync.Mutex
	p            strategy.Params
	live         *Live
	liveCancel   context.CancelFunc
	liveRetry    time.Time
	liveStarting chan struct{} // 非 nil = WS bootstrap 在途（避免持锁做网络 I/O）
	rest         map[string]restEntry

	restMu sync.Mutex
}

type restEntry struct {
	snap *Snapshot
	at   time.Time
}

func (f *Fetcher) params() strategy.Params {
	f.mu.Lock()
	p := f.p
	f.mu.Unlock()
	if p.MinOptionEdge == 0 && p.DaysMax == 0 {
		return strategy.DefaultParams()
	}
	return p
}

// FetchSnapshot 实现 ui.Fetcher / web.SnapshotFetcher。
func (f *Fetcher) FetchSnapshot(uly string) (*Snapshot, error) {
	p := f.params()
	if f.UseWS {
		return f.liveSnapshot(uly, p)
	}
	return f.restSnapshot(uly, p)
}

func (f *Fetcher) liveSnapshot(uly string, p strategy.Params) (*Snapshot, error) {
	f.mu.Lock()
	if f.live != nil && f.live.uly == uly {
		l := f.live
		f.mu.Unlock()
		return l.Snapshot(), nil
	}
	// 已有 bootstrap 在途：等它结束再重新判断，避免并发建两条 WS。
	// 关键点是**不持锁做网络 I/O**——否则 SetParams/Close 会被 WS 握手卡住，
	// 设置页保存、退出 TUI 都会跟着挂起。
	if wait := f.liveStarting; wait != nil {
		f.mu.Unlock()
		<-wait
		return f.liveSnapshot(uly, p)
	}
	if f.live != nil {
		f.live.Close()
		f.live = nil
	}
	if f.liveCancel != nil {
		f.liveCancel()
		f.liveCancel = nil
	}
	// 刚失败过就先用 REST 兜底一小段时间，避免每次刷新都卡在 WS 初始化。
	if time.Now().Before(f.liveRetry) {
		f.mu.Unlock()
		return f.restSnapshot(uly, p)
	}
	wait := make(chan struct{})
	f.liveStarting = wait
	f.mu.Unlock()

	l := NewLive(f.C, uly, p)
	ctx, cancel := context.WithCancel(context.Background())
	err := l.Start(ctx)

	f.mu.Lock()
	f.liveStarting = nil
	close(wait)
	if err != nil {
		f.liveRetry = time.Now().Add(30 * time.Second)
		f.mu.Unlock()
		cancel()
		return f.restSnapshot(uly, p)
	}
	f.live = l
	f.liveCancel = cancel
	f.mu.Unlock()

	// bootstrap 期间设置页可能改过阈值：以最新参数为准。
	l.SetParams(f.params())
	return l.Snapshot(), nil
}

func (f *Fetcher) restSnapshot(uly string, p strategy.Params) (*Snapshot, error) {
	f.restMu.Lock()
	defer f.restMu.Unlock()

	f.mu.Lock()
	if f.rest == nil {
		f.rest = make(map[string]restEntry, 4)
	}
	if e, ok := f.rest[uly]; ok && time.Since(e.at) < 15*time.Second {
		f.mu.Unlock()
		return e.snap, nil
	}
	f.mu.Unlock()

	snap, err := FetchWith(f.C, uly, p)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.rest[uly] = restEntry{snap: snap, at: time.Now()}
	f.mu.Unlock()
	return snap, nil
}

// SetParams 实现 ui.ParamSetter（Settings 页调用）。
func (f *Fetcher) SetParams(p strategy.Params) {
	f.mu.Lock()
	f.p = p
	l := f.live
	f.mu.Unlock()
	if l != nil {
		l.SetParams(p)
	}
}

// Params 当前参数（零值视为默认）。
func (f *Fetcher) Params() strategy.Params {
	return f.params()
}

// Close 释放 WS 连接（测试/退出时调用）。
func (f *Fetcher) Close() {
	f.mu.Lock()
	if f.live != nil {
		f.live.Close()
		f.live = nil
	}
	if f.liveCancel != nil {
		f.liveCancel()
		f.liveCancel = nil
	}
	f.mu.Unlock()
}
