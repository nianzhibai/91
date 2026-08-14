//go:build !windows

package backuptransfer

import (
	"os"
	"testing"
)

func TestStateFilePermissionsTooBroad(t *testing.T) {
	for _, test := range []struct {
		mode  uint32
		broad bool
	}{
		{mode: 0o600, broad: false},
		{mode: 0o640, broad: true},
		{mode: 0o606, broad: true},
	} {
		if broad := stateFilePermissionsTooBroad(os.FileMode(test.mode)); broad != test.broad {
			t.Fatalf("mode %#o broad = %t, want %t", test.mode, broad, test.broad)
		}
	}
}
