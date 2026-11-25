//go:build !darwin && !linux

package production

import "os"

func renameExternal(root *os.Root, from, to string) error { return linkMove(root, from, to) }
