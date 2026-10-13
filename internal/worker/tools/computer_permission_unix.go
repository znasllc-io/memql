//go:build darwin || linux

package tools

import (
	"os"
	"strings"

	permission "github.com/znasllc-io/memql/core/computerpermission"
	"golang.org/x/sys/unix"
)

func openPermissionPath(path string, directory bool) (*os.File, error) {
	if !permission.CleanAbsolute(path) {
		return nil, os.ErrPermission
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for index, part := range parts {
		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
		if index < len(parts)-1 || directory {
			flags |= unix.O_DIRECTORY
		}
		next, err := unix.Openat(fd, part, flags, 0)
		unix.Close(fd)
		if err != nil {
			return nil, err
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

func permissionEntryMetadata(directory *os.File, name string) (int64, os.FileMode, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, 0, err
	}
	mode := os.FileMode(st.Mode & 0777)
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		mode |= os.ModeDir
	case unix.S_IFLNK:
		mode |= os.ModeSymlink
	case unix.S_IFBLK:
		mode |= os.ModeDevice
	case unix.S_IFCHR:
		mode |= os.ModeDevice | os.ModeCharDevice
	case unix.S_IFIFO:
		mode |= os.ModeNamedPipe
	case unix.S_IFSOCK:
		mode |= os.ModeSocket
	}
	if st.Mode&unix.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if st.Mode&unix.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if st.Mode&unix.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return st.Size, mode, nil
}
