//go:build unix

package production

import (
	"fmt"
	"os"
	"syscall"
)

func externalIdentity(info os.FileInfo) string {
	if s, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d-%d", s.Dev, s.Ino)
	}
	return ""
}
