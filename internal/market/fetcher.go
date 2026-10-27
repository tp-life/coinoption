package market

import (
	"sync"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

// Fetcher main → ui 的适配器。
//
// 参数（阈值/期限/资金）可运行时调整：Settings 页改完立即生效，
// 下一次刷新即用新参数重算决策。锁保护并发刷新与参数写入。
type Fetcher struct {
	C *okx.Client

	mu sync.RWMutex
	p  strategy.Params
}

// FetchSnapshot 实现 ui.Fetcher。
func (f *Fetcher) FetchSnapshot(uly string) (*Snapshot, error) {
	f.mu.RLock()
	p := f.p
	f.mu.RUnlock()
	return FetchWith(f.C, uly, p)
}

// SetParams 实现 ui.ParamSetter（Settings 页调用）。
func (f *Fetcher) SetParams(p strategy.Params) {
	f.mu.Lock()
	f.p = p
	f.mu.Unlock()
}

// Params 当前参数（零值视为默认）。
func (f *Fetcher) Params() strategy.Params {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.p.MinOptionEdge == 0 && f.p.DaysMax == 0 {
		return strategy.DefaultParams()
	}
	return f.p
}
