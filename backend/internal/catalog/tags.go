package catalog

import (
	"errors"

	"github.com/video-site/backend/internal/tagging"
)

var ErrUnknownTag = errors.New("unknown tag")
var ErrInvalidTagSource = errors.New("tag source must be user")

const avTagLabel = "AV"

var avTagRule = tagging.Rule{MatchAVCode: true, AVCodePrefixes: tagging.DefaultAVCodePrefixes()}

func avRuleFromPrefixes(prefixes []string) tagging.Rule {
	prefixes = tagging.CleanAVCodePrefixes(prefixes)
	if len(prefixes) == 0 {
		return tagging.Rule{}
	}
	return tagging.Rule{MatchAVCode: true, AVCodePrefixes: prefixes}
}

// settingTagRulesVersion 是标签规则版本号（settings 表）。任何标签的创建、
// 规则修改、删除都会 +1；Matcher 缓存据此失效重建。
const (
	settingTagRulesVersion = "tags.rules_version"
)

type Tag struct {
	ID           int64        `json:"id"`
	Label        string       `json:"label"`
	MatchRules   tagging.Rule `json:"matchRules"`
	Source       string       `json:"source"`
	Count        int          `json:"count"`
	CrawlerOwned bool         `json:"crawlerOwned,omitempty"`
}

// TagAssignment 是一次"给视频挂标签"的完整描述：标签名、来源、命中证据。
type TagAssignment struct {
	Label    string
	Source   string
	Evidence string
}

type VideoTagMetadata struct {
	Source   string `json:"source"`
	Evidence string `json:"evidence"`
}
