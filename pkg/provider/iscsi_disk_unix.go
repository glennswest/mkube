//go:build !windows

package provider

import "syscall"

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
