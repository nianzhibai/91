package catalog

import (
	"context"
	"testing"

	"github.com/video-site/backend/internal/tagging"
)

// Matching tests explicitly create user-owned rules; production starts empty.
func seedCustomTagRules(t *testing.T, cat *Catalog) {
	t.Helper()
	for _, definition := range []struct {
		label string
		rule  tagging.Rule
	}{
		{"AV", avTagRule},
		{"奶子", tagging.Rule{Keywords: []string{"奶子", "大奶", "巨乳", "美乳", "大胸", "揉胸", "揉奶"}}},
		{"女大", tagging.Rule{Keywords: []string{"女大", "大一", "大二", "大三", "大四", "学妹", "学姐", "研究生"}}},
		{"人妻", tagging.Rule{Keywords: []string{"人妻", "少妇", "已婚"}}},
		{"后入", tagging.Rule{Keywords: []string{"后入"}}},
		{"制服", tagging.Rule{Keywords: []string{"制服", "水手服", "空姐", "护士", "JK制服"}}},
		{"美臀", tagging.Rule{Keywords: []string{"屁股", "翘臀", "美臀", "蜜桃臀", "大屁股"}}},
		{"口交", tagging.Rule{Keywords: []string{"口交", "口爆", "口活", "深喉", "吞精"}}},
	} {
		if _, err := cat.ensureTagWithRules(context.Background(), definition.label, definition.rule, "user"); err != nil {
			t.Fatal(err)
		}
	}
}
