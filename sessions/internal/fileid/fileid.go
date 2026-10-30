// Package fileid names a file by its device and inode: what tells a file replaced at the same path from the file
// that was there before.
package fileid

import (
	"os"
	"syscall"
)

// Of returns a file's device and inode; zero where the platform has none.
func Of(fi os.FileInfo) (dev, ino uint64) {
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Dev), sys.Ino
	}
	return 0, 0
}

// OfPath returns the device and inode of the file at path; zero when it cannot be read.
func OfPath(path string) (dev, ino uint64) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	return Of(fi)
}
