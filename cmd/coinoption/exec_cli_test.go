package main

import (
	"testing"

	"github.com/tp/coinoption/internal/store"
)

// live 只认真实平台仓位：历史 sim-local 纸面单不能污染实盘风控。
func TestBootLiveIgnoresPaperOrders(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SaveSim(store.SimOrder{ID: "paper", State: "open", AmtUSDT: 2000}); err != nil {
		t.Fatalf("SaveSim: %v", err)
	}

	c := cliExec{mode: "live", storeDir: dir}
	_, ex, err := c.boot()
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	s := ex.State()
	if s.ExposureUSDT != 0 || s.OpenCount != 0 {
		t.Fatalf("live 不应恢复纸面单: %+v", s)
	}
}

// sim-local 仍要恢复纸面单，保持纸面风控连续。
func TestBootSimRestoresPaperOrders(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if err := st.SaveSim(store.SimOrder{ID: "paper", State: "open", AmtUSDT: 2000}); err != nil {
		t.Fatalf("SaveSim: %v", err)
	}

	c := cliExec{mode: "sim-local", storeDir: dir}
	_, ex, err := c.boot()
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	s := ex.State()
	if s.ExposureUSDT != 2000 || s.OpenCount != 1 {
		t.Fatalf("sim-local 应恢复纸面单: %+v", s)
	}
}
