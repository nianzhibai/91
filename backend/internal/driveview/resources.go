// Package driveview owns the read-side synchronization of drive details.
package driveview

import "github.com/nianzhibai/91/backend/internal/driveevents"

type Resource string

const (
	Config  Resource = "config"
	Runtime Resource = "runtime"
	Stats   Resource = "stats"
	Storage Resource = "storage"
)

var Resources = []Resource{Config, Runtime, Stats, Storage}

var resourceEvents = map[Resource][]driveevents.Kind{
	Config:  {driveevents.DriveChanged, driveevents.DriveMetadataChanged, driveevents.DriveRuntimeChanged},
	Runtime: {driveevents.DriveChanged, driveevents.ScanResultChanged, driveevents.ActivityChanged},
	Stats:   {driveevents.DriveChanged, driveevents.MediaChanged, driveevents.GenerationChanged},
	Storage: {driveevents.DriveChanged, driveevents.MediaChanged, driveevents.AssetPathsChanged},
}
