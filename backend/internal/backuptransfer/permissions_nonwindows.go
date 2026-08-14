//go:build !windows

package backuptransfer

import "os"

func stateFilePermissionsTooBroad(mode os.FileMode) bool {
	return mode.Perm()&0o077 != 0
}
