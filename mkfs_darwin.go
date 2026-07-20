package erofs

import (
	"fmt"
	"io/fs"
	"syscall"

	"github.com/erofs/go-erofs/internal/builder"
)

func entryFromSys(info fs.FileInfo) *builder.Entry {
	switch sys := info.Sys().(type) {
	case *builder.Entry:
		return sys
	case *syscall.Stat_t:
		hardlinkKey := ""
		if info.Mode().IsRegular() && sys.Nlink > 1 {
			hardlinkKey = fmt.Sprintf("darwin:%d:%d", sys.Dev, sys.Ino)
		}
		return &builder.Entry{
			UID:         sys.Uid,
			GID:         sys.Gid,
			Mtime:       uint64(sys.Mtimespec.Sec),
			MtimeNs:     uint32(sys.Mtimespec.Nsec),
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
