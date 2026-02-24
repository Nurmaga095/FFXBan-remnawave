//go:build linux

package api

import "syscall"

type diskStats struct {
	Total   uint64
	Used    uint64
	Free    uint64
	Avail   uint64
	UsedPct float64
}

func getDiskStats(path string) (diskStats, bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return diskStats{}, false
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	avail := stat.Bavail * uint64(stat.Bsize)
	used := total - free
	usedPct := 0.0
	if total > 0 {
		usedPct = float64(used) * 100 / float64(total)
	}
	return diskStats{Total: total, Used: used, Free: free, Avail: avail, UsedPct: usedPct}, true
}
