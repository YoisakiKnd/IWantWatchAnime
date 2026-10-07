//go:build !linux

package library

// freeBytes 在非 Linux 平台（开发机上的 Windows/macOS）只做占位，
// 保证 go test 能跑；正式构建目标是 linux/arm。
func freeBytes(dir string) (int64, error) { return 1 << 40, nil }
