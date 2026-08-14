//go:build windows

package backuptransfer

import (
	"os"
	"testing"
)

func TestStateFilePermissionsIgnoreSyntheticWindowsBits(t *testing.T) {
	if stateFilePermissionsTooBroad(os.FileMode(0o666)) {
		t.Fatal("synthetic Windows permission bits were rejected")
	}
}
