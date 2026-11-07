package scanner

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
	"github.com/nianzhibai/91/backend/internal/drives"
)

type namedScanSource struct {
	*discoveryTestSource
	name      string
	err       error
	nameCalls []string
}

func (s *namedScanSource) DirectoryName(_ context.Context, dirID string) (string, error) {
	s.nameCalls = append(s.nameCalls, dirID)
	return s.name, s.err
}

func TestScanMatchesAncestorDirectoriesAndRefreshesMoves(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cat.Close() })
	for _, label := range []string{"旅行", "工作", "视频库"} {
		if _, err := cat.EnsureTag(ctx, label, "user"); err != nil {
			t.Fatal(err)
		}
	}
	source := &namedScanSource{
		name: "视频库",
		discoveryTestSource: &discoveryTestSource{entries: map[string][]drives.Entry{
			"root": {
				{ID: "category", Name: "旅行", IsDir: true},
				{ID: "sibling", Name: "其他", IsDir: true},
				{ID: "root-video", Name: "root.mp4", Size: 100},
			},
			"category": {{ID: "year", Name: "2026年", IsDir: true}},
			"year": {
				{ID: "clip", Name: "clip.mp4", Size: 200},
				{ID: "manual", Name: "manual.mp4", Size: 300},
			},
			"sibling": {{ID: "other", Name: "other.mp4", Size: 400}},
		}},
	}
	scanner := New(cat, source, []string{".mp4"}, nil, nil)
	scan := func() {
		t.Helper()
		result, err := scanner.Scan(ctx, "")
		if err != nil || len(result.Issues) != 0 {
			t.Fatalf("scan: %v, issues=%v", err, result.Issues)
		}
	}
	assertVideo := func(fileID, dirName string, names, tags []string) {
		t.Helper()
		video, err := cat.GetVideo(ctx, "fake-drive-"+fileID)
		if err != nil {
			t.Fatal(err)
		}
		if video.DirName != dirName || !reflect.DeepEqual(video.AncestorDirNames, names) {
			t.Fatalf("%s directory = %q / %#v, want %q / %#v", fileID, video.DirName, video.AncestorDirNames, dirName, names)
		}
		slices.Sort(video.Tags)
		slices.Sort(tags)
		if !sameStrings(video.Tags, tags) {
			t.Fatalf("%s tags = %#v, want %#v", fileID, video.Tags, tags)
		}
	}

	scan()
	assertVideo("clip", "2026年", []string{"视频库", "旅行", "2026年"}, []string{"视频库", "旅行"})
	assertVideo("root-video", "视频库", []string{"视频库"}, []string{"视频库"})
	assertVideo("other", "其他", []string{"视频库", "其他"}, []string{"视频库"})
	metadata, err := cat.ListVideoTagMetadata(ctx, []string{"fake-drive-clip"})
	if err != nil {
		t.Fatal(err)
	}
	if got := metadata["fake-drive-clip"]["旅行"].Evidence; got != "上级目录:旅行" {
		t.Fatalf("ancestor evidence = %q", got)
	}
	if !reflect.DeepEqual(source.nameCalls, []string{"root"}) {
		t.Fatalf("directory metadata calls = %#v, want only the scan root", source.nameCalls)
	}
	if err := cat.SetManualVideoTags(ctx, "fake-drive-manual", []string{"旅行"}); err != nil {
		t.Fatal(err)
	}

	// Renaming an ancestor changes tags even when the direct parent is unchanged.
	source.entries["root"][0].Name = "工作"
	scan()
	assertVideo("clip", "2026年", []string{"视频库", "工作", "2026年"}, []string{"视频库", "工作"})
	assertVideo("manual", "2026年", []string{"视频库", "工作", "2026年"}, []string{"旅行"})

	// Moving a file out of that subtree removes stale ancestor-derived tags.
	source.entries["root"] = append(source.entries["root"], source.entries["year"][0])
	source.entries["year"] = source.entries["year"][1:]
	scan()
	assertVideo("clip", "视频库", []string{"视频库"}, []string{"视频库"})
}

func TestDiscoverDirectoryNamesWithUnavailableRootMetadata(t *testing.T) {
	for _, metadataErr := range []error{drives.ErrNotSupported, errors.New("metadata unavailable")} {
		t.Run(metadataErr.Error(), func(t *testing.T) {
			source := &namedScanSource{
				err: metadataErr,
				discoveryTestSource: &discoveryTestSource{entries: map[string][]drives.Entry{
					"root":   {{ID: "parent", Name: "旅行", IsDir: true}},
					"parent": {{ID: "clip", Name: "clip.mp4", Size: 100}},
				}},
			}
			snapshot, _, err := New(nil, source, []string{".mp4"}, nil, nil).Discover(context.Background(), "")
			if err != nil || !snapshot.Complete() || len(snapshot.Files) != 1 {
				t.Fatalf("discover: snapshot=%#v, err=%v", snapshot, err)
			}
			if got := snapshot.Files[0].AncestorDirNames; !reflect.DeepEqual(got, []string{"", "旅行"}) {
				t.Fatalf("names = %#v", got)
			}
		})
	}
}

func TestDiscoverDirectoryNamesStartAtRequestedScanRoot(t *testing.T) {
	source := &namedScanSource{
		name: "旅行",
		discoveryTestSource: &discoveryTestSource{entries: map[string][]drives.Entry{
			"selected": {{ID: "year", Name: "2026年", IsDir: true}},
			"year":     {{ID: "clip", Name: "clip.mp4", Size: 100}},
		}},
	}
	snapshot, _, err := New(nil, source, []string{".mp4"}, nil, nil).Discover(context.Background(), "selected")
	if err != nil || len(snapshot.Files) != 1 {
		t.Fatalf("discover: snapshot=%#v, err=%v", snapshot, err)
	}
	if !reflect.DeepEqual(source.nameCalls, []string{"selected"}) {
		t.Fatalf("root name lookup = %#v", source.nameCalls)
	}
	if got := snapshot.Files[0].AncestorDirNames; !reflect.DeepEqual(got, []string{"旅行", "2026年"}) {
		t.Fatalf("directory names = %#v", got)
	}
}
