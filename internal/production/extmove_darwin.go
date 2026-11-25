//go:build darwin

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
	e = unix.RenameatxNp(fd, from, fd, to, unix.RENAME_EXCL)
	if errors.Is(e, unix.ENOTSUP) || errors.Is(e, unix.EINVAL) {
		return linkMove(root, from, to)
	}
	return e
}
