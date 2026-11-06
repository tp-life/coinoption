package store

import (
	"strings"
	"testing"
)

// 同一目录第二个锁必须失败，并带上持有者 PID；关闭后可重新获取。
func TestAcquireLockExclusive(t *testing.T) {
	dir := t.TempDir()
	l1, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("首个锁失败: %v", err)
	}
	if l1.Path() == "" {
		t.Fatal("锁路径为空")
	}
	if _, err := AcquireLock(dir); err == nil {
		t.Fatal("第二个锁应失败")
	} else if !strings.Contains(err.Error(), "已被进程") {
		t.Fatalf("错误信息应含持有者: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("释放: %v", err)
	}
	l2, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("释放后应可重新获取: %v", err)
	}
	_ = l2.Close()
	// nil 安全。
	var nilLock *Lock
	if err := nilLock.Close(); err != nil {
		t.Fatalf("nil Close: %v", err)
	}
}

// 空目录 → $HOME/.coinoption（只验证解析，不写家目录）。
func TestResolveDir(t *testing.T) {
	if got, err := ResolveDir("/tmp/x"); err != nil || got != "/tmp/x" {
		t.Fatalf("ResolveDir = %q/%v", got, err)
	}
	got, err := ResolveDir("")
	if err != nil || !strings.HasSuffix(got, ".coinoption") {
		t.Fatalf("默认目录 = %q/%v", got, err)
	}
}
