// Package atomicfile contains the platform-specific durability steps used by
// same-directory atomic file replacements.
package atomicfile

// SyncDirectory makes a completed rename durable on platforms that support
// syncing directory handles. Platforms without that operation may implement
// this as a no-op after the renamed file itself has been synced.
func SyncDirectory(directory string) error {
	return syncDirectory(directory)
}
