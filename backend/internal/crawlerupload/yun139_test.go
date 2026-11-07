package crawlerupload

import (
	"testing"

	"github.com/nianzhibai/91/backend/internal/drives/yun139"
)

func TestMobilePersonalCloudIsACrawlerUploadTarget(t *testing.T) {
	d := yun139.New(yun139.Config{ID: "mobile", RootID: "target-folder"})
	target, err := adaptUploadTarget(d)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID() != "mobile" || target.Kind() != yun139.Kind || target.RootID() != "target-folder" {
		t.Fatalf("incorrect upload target: %+v", target)
	}
	if _, ok := target.(existingUploadFinder); !ok {
		t.Fatal("missing existing-upload reconciliation")
	}
}
