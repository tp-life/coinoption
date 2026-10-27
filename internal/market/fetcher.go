package market

import "github.com/tp/coinoption/internal/okx"

// Fetcher main → ui 的适配器。
type Fetcher struct{ C *okx.Client }

// FetchSnapshot 实现 ui.Fetcher。
func (f Fetcher) FetchSnapshot(uly string) (*Snapshot, error) { return Fetch(f.C, uly) }
