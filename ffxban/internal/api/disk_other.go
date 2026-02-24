//go:build !linux

package api

type diskStats struct {
	Total   uint64
	Used    uint64
	Free    uint64
	Avail   uint64
	UsedPct float64
}

func getDiskStats(_ string) (diskStats, bool) {
	return diskStats{}, false
}
