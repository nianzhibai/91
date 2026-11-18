package videoid

import (
	"encoding/base64"
	"strconv"
	"strings"
)

// ForDrive returns the stable catalog ID used by the scanner for a file.
// Restore code uses the same function so a later scan updates the restored
// row instead of creating a second row for the same file.
func ForDrive(kind, driveID, fileID string) string {
	return kind + "-" + driveID + "-" + FilePart(fileID)
}

func FilePart(fileID string) string {
	if !strings.ContainsAny(fileID, `/\`+"\x00") {
		return fileID
	}
	return "b64_" + base64.RawURLEncoding.EncodeToString([]byte(fileID))
}

// Generation assigns another public ID when an earlier generation of a
// provider file still owns its original link. Drive kinds never start with ~.
func Generation(base string, n int) string {
	return "~" + strconv.Itoa(n) + "~" + base
}
