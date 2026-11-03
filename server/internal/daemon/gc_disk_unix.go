//go:build !windows

package daemon

import "golang.org/x/sys/unix"

// Statfs reports filesystem availability, including shared APFS free space,
// rather than summing logical file sizes (clones/snapshots can retain blocks).
func gcFilesystemFreeBytes(path string) (uint64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}
