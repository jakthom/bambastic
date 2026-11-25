//go:build linux

package production

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func renameExternal(root *os.Root, from, to string) error {
	if !validFileName(from) || !validFileName(to) {
		return os.ErrInvalid
	}
	dir, e := root.Open(".")
	if e != nil {
		return e
	}
	defer dir.Close()
	fd := int(dir.Fd())
	e = unix.Renameat2(fd, from, fd, to, unix.RENAME_NOREPLACE)
	if errors.Is(e, unix.EOPNOTSUPP) || errors.Is(e, unix.ENOSYS) || errors.Is(e, unix.EINVAL) {
		return linkMove(root, from, to)
	}
	return e
}
