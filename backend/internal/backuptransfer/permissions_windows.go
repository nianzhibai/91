//go:build windows

package backuptransfer

import "os"

func stateFilePermissionsTooBroad(os.FileMode) bool {
	// Windows FileMode permission bits are synthetic: writable files report
	// 0666, and Chmod only changes the read-only attribute. The state directory
	// ACL is the access-control boundary on this platform.
	return false
}
