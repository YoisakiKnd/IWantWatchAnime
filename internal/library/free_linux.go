//go:build linux

package library

import "syscall"

// freeBytes 读取挂载点剩余空间（Linux 专用，目标平台就是 linux/arm）。
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	// Bavail 是给非特权用户的可用块数，Bsize 是块大小。
	return int64(st.Bavail) * int64(st.Bsize), nil
}
