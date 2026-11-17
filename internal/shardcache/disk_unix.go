//go:build unix

package shardcache

import "golang.org/x/sys/unix"

// realDisk asks the filesystem holding dir how much room an ordinary user may
// still spend (Bavail, not Bfree) and how big it is.
func realDisk(dir string) (free, total int64, err error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return 0, 0, err
	}
	block := int64(fs.Bsize)
	return int64(fs.Bavail) * block, int64(fs.Blocks) * block, nil
}
