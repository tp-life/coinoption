package strategy

import (
	"testing"
	"time"
)

func TestBestDCYPicksHighestScore(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	p := DefaultParams()
	ok := DecideDCY(dcyRow(now), now, p)
	if ok.Action != ActionSubscribeDual || ok.ReasonCode != ReasonDCYOK {
		t.Fatalf("fixture 应为 dcy_ok: %s/%s", ok.Action, ok.ReasonCode)
	}

	// 同分时取先出现者；高分者必须胜出。
	low := ok
	low.Score = 0.01
	low.Row.ProductID = "low"
	high := ok
	high.Score = 0.02
	high.Row.ProductID = "high"
	best, found := BestDCY([]Decision{
		{Action: ActionNone, ReasonCode: ReasonDeltaTooHigh},
		low,
		high,
	})
	if !found || best.Row.ProductID != "high" {
		t.Fatalf("best = %+v, found=%v; want high", best, found)
	}
}

func TestBestDCYNone(t *testing.T) {
	if best, found := BestDCY(nil); found {
		t.Fatalf("空列表不应有 best: %+v", best)
	}
	rejected := Decision{Action: ActionNone, ReasonCode: ReasonIVRVTooLow}
	if _, found := BestDCY([]Decision{rejected}); found {
		t.Fatal("拒绝的候选不能算 best")
	}
}
