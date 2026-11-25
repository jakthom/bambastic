//go:build !unix

package production

import (
	"fmt"
	"os"
)

func externalIdentity(info os.FileInfo) string {
	return fmt.Sprintf("%d-%d", info.Size(), info.ModTime().UnixNano())
}
