package strategy

// BestDCY 返回本轮可申购候选中 Score 最高的一笔（ActionSubscribeDual +
// ReasonDCYOK）。没有合格候选时 ok=false，调用方应显示「空仓」而不是
// 硬塞一个被拒绝的档位。
//
// 与自动执行的选档口径一致：只在真正可申购的候选里比较 score；同分取
// 先出现者，保持 BuildDCY 的确定性顺序。
func BestDCY(ds []Decision) (Decision, bool) {
	var best Decision
	found := false
	for _, d := range ds {
		if d.Action != ActionSubscribeDual || d.ReasonCode != ReasonDCYOK {
			continue
		}
		if !found || d.Score > best.Score {
			best = d
			found = true
		}
	}
	return best, found
}
