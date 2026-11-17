package backup

import (
	"context"
	"path/filepath"
	"strings"
)

// assetSource is an application directory archived as payload/<name>.
type assetSource struct {
	name     string
	selected func(BackupSelection) bool
	// snapshotSelected copies only the files owned by the selected catalog
	// rows into a partial backup. Nil means the whole directory belongs to
	// every selection that includes it.
	snapshotSelected func(*Manager, context.Context, string, snapshotSelectionState) error
}

// assetSources lists every application directory a backup can contain, in
// archive order. Local storage is archived separately because its files live
// in user-configured roots.
var assetSources = []assetSource{
	{name: "previews", selected: BackupSelection.anyResource, snapshotSelected: (*Manager).snapshotSelectedPreviews},
	{name: "uploads", selected: selectsUploadStorage, snapshotSelected: (*Manager).snapshotSelectedUploads},
	{name: "crawler-scripts", selected: selectsCrawlerScripts},
	{name: "scriptcrawlers", selected: selectsCrawlerScripts, snapshotSelected: (*Manager).snapshotSelectedCrawlerVideos},
	{name: "spider91", selected: selectsCrawlerScripts},
}

const (
	databaseArchivePath = "payload/database.sqlite"
	localStorageSection = "localstorage"
)

func selectsUploadStorage(s BackupSelection) bool  { return s.UploadStorage }
func selectsCrawlerScripts(s BackupSelection) bool { return s.CrawlerScripts }

// assetPath returns the live directory of an asset source.
func (m *Manager) assetPath(name string) string {
	if name == "previews" {
		return m.previewPath
	}
	return filepath.Join(m.assetRoot, name)
}

// selectedAssetSources returns the asset sources included by selection.
func selectedAssetSources(selection BackupSelection) []assetSource {
	var selected []assetSource
	for _, source := range assetSources {
		if source.selected(selection) {
			selected = append(selected, source)
		}
	}
	return selected
}

// includedForSelection returns the manifest's included sections: the
// database followed by every payload section the selection archives.
func includedForSelection(selection BackupSelection, hasLocalStorageRoots bool) []string {
	included := []string{"database"}
	for _, source := range selectedAssetSources(selection) {
		included = append(included, source.name)
	}
	if selection.LocalStorage && hasLocalStorageRoots {
		included = append(included, localStorageSection)
	}
	return included
}

// payloadSectionSelected reports whether a payload/<section> directory
// belongs to selection.
func payloadSectionSelected(section string, selection BackupSelection) bool {
	if section == localStorageSection {
		return selection.LocalStorage
	}
	for _, source := range assetSources {
		if source.name == section {
			return source.selected(selection)
		}
	}
	return false
}

// payloadSection splits an archive path below payload/ into its section and
// the remainder. It reports false for paths outside a known section.
func payloadSection(name string) (section, rest string, ok bool) {
	relative, found := strings.CutPrefix(name, "payload/")
	if !found {
		return "", "", false
	}
	section, rest, _ = strings.Cut(relative, "/")
	if section == localStorageSection {
		return section, rest, true
	}
	for _, source := range assetSources {
		if source.name == section {
			return section, rest, true
		}
	}
	return "", "", false
}
