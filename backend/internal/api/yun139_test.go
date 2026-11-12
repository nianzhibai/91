package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nianzhibai/91/backend/internal/catalog"
)

func TestMobilePersonalCloudConfiguration(t *testing.T) {
	cat, err := catalog.Open(t.TempDir() + "/catalog.db")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	admin := &AdminServer{Catalog: cat}
	for _, body := range []string{
		`{"id":"mobile","kind":"yun139","name":"移动个人云","credentials":{"authorization":"old-auth"}}`,
		`{"id":"mobile","kind":"yun139","name":"重命名个人云"}`,
		`{"id":"mobile","kind":"yun139","name":"重命名个人云","credentials":{"authorization":"new-auth"}}`,
	} {
		res := httptest.NewRecorder()
		admin.handleUpsertDrive(res, httptest.NewRequest(http.MethodPost, "/admin/api/drives", strings.NewReader(body)))
		if res.Code != http.StatusOK {
			t.Fatalf("save: status=%d body=%s", res.Code, res.Body.String())
		}
		if !strings.Contains(res.Body.String(), `"canUpload":true`) {
			t.Fatalf("upload capability missing: %s", res.Body.String())
		}
		if strings.Contains(res.Body.String(), "old-auth") || strings.Contains(res.Body.String(), "new-auth") {
			t.Fatal("save response leaked authorization")
		}
		got, err := cat.GetDrive(context.Background(), "mobile")
		if err != nil {
			t.Fatal(err)
		}
		if got.RootID != "/" || got.ScanRootID != "/" {
			t.Fatalf("roots = %q, %q", got.RootID, got.ScanRootID)
		}
		want := "old-auth"
		if strings.Contains(body, "new-auth") {
			want = "new-auth"
		}
		if got.Credentials["authorization"] != want {
			t.Fatal("credentials not preserved or updated")
		}
	}
	if driveKindLabel("yun139") != "移动网盘" {
		t.Fatal("missing public source label")
	}
}
