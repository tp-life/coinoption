package executor

import (
	"math"
	"testing"
)

func TestDbg2(t *testing.T) {
	loss, ccy, _ := SettleOptionShortLoss('C', 80000, 83000, 2, 1, "BTC")
	t.Logf("ccy=%q bits=%x loss=%v nearly=%v", ccy, math.Float64bits(loss), loss, nearly(loss, 6000/83000))
}
