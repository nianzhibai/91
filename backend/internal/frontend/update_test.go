package frontend

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestUpdatesReadOnlyMetadataAndCompareNewestInstalled(t *testing.T) {
	m := testManager(t)
	for _, version := range []string{"1.2.0", "1.10.0"} {
		if _, err := m.Install(bytes.NewReader(testNamedArchive(t, "web", version, "1.0.0", version, nil)), InstallOptions{Repository: "https://github.com/owner/web"}); err != nil {
			t.Fatal(err)
		}
	}
	// The active version is older; updates compare with the newest installed copy.
	if err := m.Activate("web@1.2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(bytes.NewReader(testNamedArchive(t, "other", "1.0.0", "1.0.0", "other", nil)), InstallOptions{Repository: "owner/other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(bytes.NewReader(testNamedArchive(t, "failing", "1.0.0", "1.0.0", "failing", nil)), InstallOptions{Repository: "owner/failing"}); err != nil {
		t.Fatal(err)
	}
	tags := map[string]string{"/repos/owner/web/releases/latest": "v1.10.0", "/repos/owner/other/releases/latest": "v1.1.0"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tag, ok := tags[r.URL.Path]
		if !ok {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []releaseAsset{{Name: ArchiveName, URL: "https://github.com/archive"}, {Name: ArchiveName + ".sha256", URL: "https://github.com/checksum"}}})
	}))
	defer server.Close()
	before, _ := m.Status()
	updates, err := (&ReleaseSource{Client: server.Client(), apiURL: server.URL}).Updates(context.Background(), m)
	if err != nil || !reflect.DeepEqual(updates, []Update{{Name: "other", Version: "1.1.0"}}) {
		t.Fatalf("updates: %+v %v", updates, err)
	}
	if after, _ := m.Status(); !reflect.DeepEqual(before, after) {
		t.Fatalf("update check changed state: %+v %+v", before, after)
	}
}
