package store

import (
	"fmt"
	"testing"
	"time"
)

func autoFailAt(ref string, at time.Time) AutoFailure {
	return AutoFailure{Ts: at, Kind: "option", Ref: ref, Reason: "风控拒绝：" + ref}
}

// 环形表只留最近 AutoFailureLimit 条，最旧的被淘汰。
func TestPushAutoFailureKeepsLastN(t *testing.T) {
	now := time.Now().UTC()
	var rows []AutoFailure
	for i := 0; i < AutoFailureLimit+2; i++ {
		rows = PushAutoFailure(rows, autoFailAt(fmt.Sprintf("inst-%02d", i), now.Add(time.Duration(i)*time.Second)), now)
	}
	if len(rows) != AutoFailureLimit {
		t.Fatalf("保留 %d 条，期望 %d", len(rows), AutoFailureLimit)
	}
	if rows[0].Ref != "inst-02" {
		t.Fatalf("最旧一条应为 inst-02，实际 %q", rows[0].Ref)
	}
	if rows[len(rows)-1].Ref != fmt.Sprintf("inst-%02d", AutoFailureLimit+1) {
		t.Fatalf("最新一条不符: %q", rows[len(rows)-1].Ref)
	}
}

// 10 分钟内完全相同的失败合并成一条并累加 Count；超过窗口再记一条。
func TestPushAutoFailureMergesRepeats(t *testing.T) {
	t0 := time.Now().UTC()
	rows := PushAutoFailure(nil, autoFailAt("X", t0), t0)
	rows = PushAutoFailure(rows, autoFailAt("X", t0.Add(time.Minute)), t0.Add(time.Minute))
	if len(rows) != 1 || rows[0].Count != 2 {
		t.Fatalf("一分钟内重复应合并为 1 条 ×2，实际 %+v", rows)
	}
	if !rows[0].Ts.Equal(t0.Add(time.Minute)) {
		t.Fatalf("合并后时间应更新到最新一次，实际 %v", rows[0].Ts)
	}
	rows = PushAutoFailure(rows, autoFailAt("X", t0.Add(time.Minute+autoFailureMergeWindow+time.Second)), t0)
	if len(rows) != 2 || rows[1].Count != 1 {
		t.Fatalf("超出合并窗口应新增一条，实际 %+v", rows)
	}
}

// RecordAutoFailure / LoadAutoFailures 落盘往返：旧 → 新，且截到 10 条。
func TestRecordAutoFailureRoundTrip(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	for i := 0; i < AutoFailureLimit+3; i++ {
		if err := st.RecordAutoFailure(AutoFailure{
			Ts: base.Add(time.Duration(i) * time.Second), Kind: "dcd",
			Ref: fmt.Sprintf("BTC-USDT-%02d-C", i), Reason: "低于产品最小申购",
		}); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	rows, err := st.LoadAutoFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != AutoFailureLimit {
		t.Fatalf("LoadAutoFailures = %d 条，期望 %d", len(rows), AutoFailureLimit)
	}
	if rows[0].Ref != "BTC-USDT-03-C" || rows[len(rows)-1].Ref != "BTC-USDT-12-C" {
		t.Fatalf("顺序应为旧 → 新且保留最近 10 条: first=%q last=%q", rows[0].Ref, rows[len(rows)-1].Ref)
	}
}

// SaveAutoFailures 覆盖写（Web 端内存表整体落盘）同样只保留 10 条。
func TestSaveAutoFailuresCaps(t *testing.T) {
	st, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC()
	rows := make([]AutoFailure, 0, AutoFailureLimit+5)
	for i := 0; i < AutoFailureLimit+5; i++ {
		rows = append(rows, AutoFailure{
			Ts: base.Add(time.Duration(i) * time.Second), Kind: "market",
			Reason: fmt.Sprintf("行情失败 %d", i),
		})
	}
	if err := st.SaveAutoFailures(rows); err != nil {
		t.Fatal(err)
	}
	got, err := st.LoadAutoFailures()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != AutoFailureLimit || got[len(got)-1].Reason != "行情失败 14" {
		t.Fatalf("应只保留最后 10 条，实际 %d 条 / 最后 %q", len(got), got[len(got)-1].Reason)
	}
}
