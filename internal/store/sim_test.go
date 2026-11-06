package store

import "testing"

func TestClearOpenSimKeepsSettledHistory(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, o := range []SimOrder{
		{ID: "open-1", State: "open", AmtUSDT: 2000},
		{ID: "open-2", State: "open", AmtUSDT: 2000},
		{ID: "settled-1", State: "settled", AmtUSDT: 100},
	} {
		if err := s.SaveSim(o); err != nil {
			t.Fatalf("SaveSim %s: %v", o.ID, err)
		}
	}
	n, err := s.ClearOpenSim()
	if err != nil {
		t.Fatalf("ClearOpenSim: %v", err)
	}
	if n != 2 {
		t.Fatalf("应清除 2 笔 open，实际 %d", n)
	}
	rows, err := s.LoadSim()
	if err != nil {
		t.Fatalf("LoadSim: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "settled-1" {
		t.Fatalf("应保留已结算历史: %+v", rows)
	}
	if n, err := s.ClearOpenSim(); err != nil || n != 0 {
		t.Fatalf("重复清理应无变化: n=%d err=%v", n, err)
	}
}
