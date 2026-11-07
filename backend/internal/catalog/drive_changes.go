package catalog

import "github.com/nianzhibai/91/backend/internal/driveevents"

func (c *Catalog) DriveEvents() *driveevents.Hub { return &c.driveEvents }

// notifyDriveWrite runs after the owning statement/transaction has returned.
// Cross-drive canonical projections can change when a single video changes, so
// video mutations publish a change for all drives.
func (c *Catalog) notifyDriveWrite(err *error, driveID string, kinds ...driveevents.Kind) {
	if *err == nil {
		c.driveEvents.Notify(driveID, false, kinds...)
	}
}
