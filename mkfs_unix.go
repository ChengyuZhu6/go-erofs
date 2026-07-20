//go:build linux

package erofs

import (
	"fmt"
	"io/fs"
	"syscall"

	"github.com/erofs/go-erofs/internal/builder"
)

// entryFromSys extracts metadata from info.Sys(). Returns nil if the
// type is not recognized, allowing the caller to use a default.
func entryFromSys(info fs.FileInfo) *builder.Entry {
	switch sys := info.Sys().(type) {
	case *builder.Entry:
		return sys
	case *syscall.Stat_t:
		hardlinkKey := ""
		if info.Mode().IsRegular() && sys.Nlink > 1 {
			hardlinkKey = fmt.Sprintf("linux:%d:%d", sys.Dev, sys.Ino)
		}
		return &builder.Entry{
			UID:         sys.Uid,
			GID:         sys.Gid,
			Mtime:       uint64(sys.Mtim.Sec),
			MtimeNs:     uint32(sys.Mtim.Nsec),
			Nlink:       uint32(sys.Nlink),
			Rdev:        uint32(sys.Rdev),
			HardlinkKey: hardlinkKey,
		}
	default:
		return nil
	}
}

func erofsHardlinkKey(info fs.FileInfo, stat *Stat) string {
	if info.Mode().IsRegular() && stat.Nlink > 1 {
		return fmt.Sprintf("erofs:%d", stat.Ino)
	}
	return ""
}
