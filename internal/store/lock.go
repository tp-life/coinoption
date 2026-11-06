package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Lock 数据目录进程锁（flock，Unix）。
//
// 为什么需要：TUI 与 Web 同时跑会各持一份 GuardState，两边都以为额度够，
// 合计可能超过总敞口/并发/日笔数；纸面单结算与 settings 写入也会互相覆盖。
// 长驻进程（TUI/Web）与下单类命令在启动时抢占独占锁，第二个进程直接失败，
// 而不是「看起来正常、风控悄悄失效」。
type Lock struct {
	f    *os.File
	path string
}

// ResolveDir 数据目录（空 → $HOME/.coinoption）。
func ResolveDir(dir string) (string, error) {
	if strings.TrimSpace(dir) != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("user home: %w", err)
	}
	return filepath.Join(home, ".coinoption"), nil
}

// AcquireLock 对数据目录加独占锁；已被其它进程占用时返回错误（含持有者 PID）。
func AcquireLock(dir string) (*Lock, error) {
	dir, err := ResolveDir(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "LOCK")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开锁文件: %w", err)
	}
	if err := flockExclusive(f); err != nil {
		holder, _ := os.ReadFile(path)
		_ = f.Close()
		pid := strings.TrimSpace(string(holder))
		if pid == "" {
			pid = "未知"
		}
		return nil, fmt.Errorf("数据目录 %s 已被进程 %s 占用（flock）: %w", dir, pid, err)
	}
	// 写入 PID 便于排查；失败不影响锁本身。
	_ = f.Truncate(0)
	if _, err := f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0); err != nil {
		_ = f.Sync()
	}
	l := &Lock{f: f, path: path}
	return l, nil
}

// Path 锁文件路径（提示信息用）。
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close 释放锁（进程退出时内核也会自动释放）。
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = funlock(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
