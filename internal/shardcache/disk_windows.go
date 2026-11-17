//go:build windows

package shardcache

import "golang.org/x/sys/windows"

// realDisk asks the volume holding dir how much room the caller may still
// spend and how big it is.
func realDisk(dir string) (free, total int64, err error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, size, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &size, &totalFree); err != nil {
		return 0, 0, err
	}
	return int64(avail), int64(size), nil
}
