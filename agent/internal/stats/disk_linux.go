//go:build linux

package stats

import "golang.org/x/sys/unix"

// diskUsage reports used/total bytes of the filesystem containing path.
func diskUsage(path string) (used, total uint64) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, 0
	}
	bs := uint64(fs.Bsize)
	total = fs.Blocks * bs
	free := fs.Bavail * bs
	if total >= free {
		used = total - free
	}
	return used, total
}
