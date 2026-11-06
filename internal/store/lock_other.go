//go:build !unix

package store

import "os"

// 非 Unix 平台没有 flock：退化为「不阻塞」。
// 多进程保护在这些平台上不生效，文档里必须说明。
func flockExclusive(*os.File) error { return nil }

func funlock(*os.File) error { return nil }
