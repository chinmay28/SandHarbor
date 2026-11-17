//go:build !unix && !windows

package shardcache

// realDisk cannot measure the disk here, so the cache leans on its own limit.
func realDisk(string) (free, total int64, err error) { return 0, 0, errNoDisk }
