//go:build !windows

package provider

import (
	"os"
	"syscall"
)

func getDiskUsagePlatform(path string) (*diskUsage, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return nil, err
	}
	total := stat.Blocks * uint64(stat.Bsize)
	avail := stat.Bavail * uint64(stat.Bsize)
	used := total - (stat.Bfree * uint64(stat.Bsize))
	return &diskUsage{total: total, used: used, avail: avail}, nil
}

// getActualDiskBytes returns the real on-disk allocation for a file using
// syscall.Stat_t.Blocks (512-byte blocks), which correctly reports sparse
// file usage instead of the logical size.
func getActualDiskBytes(fi os.FileInfo) int64 {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return sys.Blocks * 512
	}
	return fi.Size() // fallback to logical size
}
