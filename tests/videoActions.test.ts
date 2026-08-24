import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const actionsSource = readFileSync(
  new URL("../src/components/VideoActions.tsx", import.meta.url),
  "utf8"
);
const detailCss = readFileSync(
  new URL("../src/styles/video-detail.css", import.meta.url),
  "utf8"
);
const detailPageSource = readFileSync(
  new URL("../src/pages/VideoDetailPage.tsx", import.meta.url),
  "utf8"
);
const infoPanelSource = readFileSync(
  new URL("../src/components/VideoInfoPanel.tsx", import.meta.url),
  "utf8"
);

test("detail reactions use one visit-scoped mutually exclusive ballot", () => {
  assert.match(
    actionsSource,
    /const visitId = useMemo\(createVideoReactionVisitId, \[video\.id\]\)/
  );
  assert.match(actionsSource, /nextVideoReaction\(previousReaction, selected\)/);
  assert.match(
    actionsSource,
    /setVideoVisitReaction\(\s*video\.id,\s*visitId,\s*nextReaction\s*\)/
  );
  assert.match(actionsSource, /onClick=\{\(\) => handleReaction\("like"\)\}/);
  assert.match(actionsSource, /onClick=\{\(\) => handleReaction\("dislike"\)\}/);
  assert.match(actionsSource, /disabled=\{reactionPending\}/);
  assert.doesNotMatch(actionsSource, /\/api\/video\/.*\/like/);
});

test("detail like and dislike buttons are visually separated", () => {
  assert.doesNotMatch(actionsSource, /vd-actions__divider/);
  assert.match(
    detailCss,
    /\.vd-actions__group\s*\{[^}]*gap:\s*var\(--space-2\)/s
  );
  assert.match(
    detailCss,
    /\.vd-actions__pill\s*\{[^}]*border:\s*1px solid var\(--border-subtle\)[^}]*border-radius:\s*var\(--radius-sm\)/s
  );
});

test("desktop share button matches the like button without changing narrow screens", () => {
  assert.match(
    detailCss,
    /@media \(min-width:\s*769px\)\s*\{[\s\S]*?\.vd-actions__share:not\(\.is-success\)\s*\{[^}]*background:\s*rgba\(255, 255, 255, 0\.04\)[^}]*color:\s*var\(--text-default\)/s
  );
  assert.match(
    detailCss,
    /\.vd-actions__share:not\(\.is-success\):hover:not\(:disabled\)\s*\{[^}]*background:\s*rgba\(255, 255, 255, 0\.06\)[^}]*border-color:\s*var\(--border-strong\)[^}]*color:\s*var\(--text-strong\)/s
  );
});

test("touch devices do not retain the share button hover highlight", () => {
  assert.match(
    detailCss,
    /@media \(hover:\s*none\) and \(pointer:\s*coarse\)\s*\{[\s\S]*?\.vd-actions__share:not\(\.is-success\):hover:not\(:disabled\)\s*\{[^}]*background:\s*transparent[^}]*border-color:\s*var\(--border-subtle\)[^}]*color:\s*var\(--text-muted\)/s
  );
});

test("detail playback actions only expose delete as the management action", () => {
  assert.doesNotMatch(actionsSource, /不再显示/);
  assert.doesNotMatch(actionsSource, /EyeOff/);
  assert.doesNotMatch(actionsSource, /onHideVideo/);
  assert.doesNotMatch(actionsSource, /hideSaving/);
  assert.doesNotMatch(actionsSource, /vd-actions__hide/);
  assert.match(actionsSource, /aria-label="删除这个视频"/);
  assert.doesNotMatch(detailPageSource, /hideVideo/);
  assert.doesNotMatch(detailPageSource, /handleHideVideo/);
  assert.doesNotMatch(detailPageSource, /onHideVideo/);
});

test("detail recommendations stay stable when returning from another video", () => {
  assert.match(detailPageSource, /const cachedRelatedVideosByID = new Map<string, VideoDetail\["relatedVideos"\]>\(\)/);
  assert.match(detailPageSource, /function withStableRelatedVideos\(detail: VideoDetail \| null\): VideoDetail \| null/);
  assert.match(detailPageSource, /let stableDetail = withStableRelatedVideos\(d\)/);
  assert.match(detailPageSource, /setDetail\(stableDetail\)/);
  assert.doesNotMatch(detailPageSource, /setDetail\(d\)/);
});

test("detail background refresh preserves confirmed local reaction counts", () => {
  assert.match(
    detailPageSource,
    /const reactionCountsRef = useRef<[\s\S]*VideoReactionCounts/
  );
  assert.match(
    detailPageSource,
    /localReactionCounts\?\.videoId === stableDetail\.id/
  );
  assert.match(
    detailPageSource,
    /likes:\s*localReactionCounts\.likes,[\s\S]*dislikes:\s*localReactionCounts\.dislikes/
  );
  assert.match(
    detailPageSource,
    /reactionCountsRef\.current = \{ videoId: id, \.\.\.counts \}/
  );
});

test("detail history navigation renders cached content before background refresh", () => {
  assert.match(detailPageSource, /const DETAIL_CACHE_LIMIT = 20;/);
  assert.match(
    detailPageSource,
    /const cachedVideoDetailsByID = new Map<string, VideoDetailSnapshot>\(\)/
  );
  assert.match(
    detailPageSource,
    /<VideoDetailContent key=\{id \?\? "missing"\} id=\{id\} \/>/
  );
  assert.match(
    detailPageSource,
    /const \[initialSnapshot\] = useState<VideoDetailSnapshot \| null>\(\(\) =>\s*id \? readCachedVideoDetail\(id\) : null/
  );
  assert.match(
    detailPageSource,
    /const \[loading, setLoading\] = useState\(initialSnapshot === null\)/
  );
  assert.match(
    detailPageSource,
    /const detailRequest = prefetchedDetail[\s\S]*?Promise\.all\(\[detailRequest, fetchTags\(\)\]\)/
  );
  assert.match(detailPageSource, /if \(!stableDetail && initialSnapshot\)/);
  assert.match(detailPageSource, /if \(navigationType !== "POP"\)/);
  assert.doesNotMatch(detailPageSource, /setLoading\(true\)/);
});

test("detail page defers subtitles to the player menu", () => {
  assert.doesNotMatch(detailPageSource, /haveSameSubtitles/);
  assert.doesNotMatch(detailPageSource, /subtitles:\s*stableSubtitles/);
  assert.match(detailPageSource, /const loadSubtitles = useCallback/);
  assert.match(detailPageSource, /loadSubtitles=\{loadSubtitles\}/);
});

test("detail delete dialog stays centered on mobile", () => {
  assert.match(
    detailCss,
    /@media \(max-width:\s*480px\)\s*\{[\s\S]*\.vd-delete-modal\s*\{[^}]*place-items:\s*center/s
  );
  assert.doesNotMatch(
    detailCss,
    /@media \(max-width:\s*480px\)\s*\{[\s\S]*\.vd-delete-modal\s*\{[^}]*align-items:\s*end/s
  );
});

test("detail delete source option stays visually flat", () => {
  assert.match(
    detailCss,
    /\.vd-delete-option\s*\{[^}]*padding:\s*0[^}]*border:\s*0[^}]*background:\s*transparent/s
  );
});

test("detail tag editor opens as a page modal", () => {
  assert.match(infoPanelSource, /createPortal\(/);
  assert.match(infoPanelSource, /className="vd-tag-editor-modal"/);
  assert.match(infoPanelSource, /aria-modal="true"/);
  assert.match(
    detailCss,
    /\.vd-tag-editor-modal\s*\{[^}]*position:\s*fixed[^}]*place-items:\s*center/s
  );
  assert.doesNotMatch(detailCss, /\.vd-tag-editor\s*\{[^}]*margin:/s);
});

test("detail tag editor chips hide counts and avoid divider lines", () => {
  assert.doesNotMatch(infoPanelSource, /typeof tag\.count/);
  assert.doesNotMatch(infoPanelSource, /<em>\{tag\.count\}<\/em>/);
  assert.doesNotMatch(detailCss, /vd-tag-editor__chip em/);
  assert.match(detailCss, /\.vd-tag-editor\s*\{[^}]*border:\s*0/s);
  assert.match(detailCss, /\.vd-tag-editor__head\s*\{[^}]*border-bottom:\s*0/s);
  assert.match(detailCss, /\.vd-tag-editor__chip\s*\{[^}]*border:\s*0/s);
  assert.match(detailCss, /\.vd-tag-editor__actions\s*\{[^}]*border-top:\s*0/s);
});

test("detail tag labels omit the hash prefix", () => {
  assert.match(infoPanelSource, /className="vd-tag"[\s\S]*?\{t\}/);
  assert.doesNotMatch(infoPanelSource, /#\{t\}/);
});
