import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const componentSource = readFileSync(
  new URL("../src/components/MobileVideoCollection.tsx", import.meta.url),
  "utf8"
);
const detailSource = readFileSync(
  new URL("../src/pages/VideoDetailPage.tsx", import.meta.url),
  "utf8"
);
const railSource = readFileSync(
  new URL("../src/components/RecommendedRail.tsx", import.meta.url),
  "utf8"
);
const railSkeletonSource = readFileSync(
  new URL("../src/components/VideoRailSkeleton.tsx", import.meta.url),
  "utf8"
);
const collectionHookSource = readFileSync(
  new URL("../src/lib/useLazyVideoCollection.ts", import.meta.url),
  "utf8"
);
const dataSource = readFileSync(
  new URL("../src/data/videos.ts", import.meta.url),
  "utf8"
);
const stylesSource = readFileSync(
  new URL("../src/styles/video-detail.css", import.meta.url),
  "utf8"
);

test("video detail renders a directory collection only when it has siblings", () => {
  assert.match(
    detailSource,
    /detail\.collection && detail\.collection\.total > 1[\s\S]*?<MobileVideoCollection[\s\S]*?videoId=\{detail\.id\}/
  );
  assert.match(componentSource, />合集</);
  assert.match(componentSource, /collection\.currentIndex\}\/\{collection\.total/);
  assert.doesNotMatch(componentSource, /\bListVideo\b/);
  assert.match(
    componentSource,
    /collection\.currentIndex\}\/\{collection\.total[\s\S]*?<ChevronRight/
  );
  assert.doesNotMatch(
    stylesSource,
    /\.vd-collection-entry__position\s*>\s*svg:first-child/
  );
});

test("collection items load lazily through one shared resource", () => {
  assert.match(
    dataSource,
    /`\/api\/video\/\$\{encodeURIComponent\(id\)\}\/collection\$\{previewQuery\}`/
  );
  assert.match(
    componentSource,
    /useLazyVideoCollection\(videoId, open\)/
  );
  assert.match(
    collectionHookSource,
    /if \(!enabled \|\| dataHasRequiredFields\) return/
  );
  assert.match(
    collectionHookSource,
    /fetchVideoCollection\(videoId, \{[\s\S]*?signal:\s*controller\.signal,[\s\S]*?includePreview,/
  );
  assert.match(collectionHookSource, /cachedCollectionsByVideoID/);
  assert.match(
    collectionHookSource,
    /requirePreview && !cached\.includesPreview/
  );
  assert.match(dataSource, /collection\.total !== collection\.items\.length/);
});

test("desktop recommendation rail offers recommendation and collection tabs", () => {
  assert.match(
    detailSource,
    /<RecommendedRail[\s\S]*?videos=\{detail\.relatedVideos\}[\s\S]*?videoId=\{detail\.id\}[\s\S]*?collection=\{detail\.collection\}/
  );
  assert.match(
    railSource,
    /className="vd-rail__tabs" role="tablist"[\s\S]*?>\s*推荐视频\s*<[\s\S]*?>\s*相关合集\s*</
  );
  assert.match(
    railSource,
    /desktop && hasCollection && activeView === "collection"/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__tab\s*\{[\s\S]*?min-height:\s*34px;[\s\S]*?padding:\s*0 12px;[\s\S]*?font-size:\s*var\(--font-sm\);/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__tab\[aria-selected="true"\]\s*\{[\s\S]*?background:\s*var\(--text-strong\);/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__head\.vd-rail__head--mobile-only\s*\{\s*display:\s*none;/
  );
  assert.match(
    stylesSource,
    /@media \(max-width:\s*768px\)[\s\S]*?\.vd-rail__head\.vd-rail__head--mobile-only\s*\{\s*display:\s*flex;/
  );
  assert.match(
    stylesSource,
    /@media \(max-width:\s*768px\)[\s\S]*?\.vd-rail--collection-only,\s*\.vd-rail__tabs,\s*\.vd-rail__tabpanel--collection\s*\{\s*display:\s*none;/
  );
});

test("desktop tab switches preserve both list instances", () => {
  assert.match(
    railSource,
    /className="vd-rail__tabpanel vd-rail__tabpanel--recommended"[\s\S]*?hidden=\{showCollection\}/
  );
  assert.match(
    railSource,
    /className="vd-rail__tabpanel vd-rail__tabpanel--collection"[\s\S]*?hidden=\{!showCollection\}/
  );
  assert.match(railSource, /const recommendedItems = useMemo\(/);
  assert.match(railSource, /const collectionItems = useMemo\(/);
  assert.match(
    railSource,
    /const RecommendedItem = memo\(\s*forwardRef<HTMLLIElement, RailItemProps>/
  );
  assert.doesNotMatch(railSource, /\{showCollection \? \(/);
  assert.match(
    railSource,
    /previewController\.setActiveId\(null\);\s*setActiveView\(nextView\)/
  );
  assert.match(
    railSource,
    /collectionViewActive \|\|[\s\S]*?collectionLoadStartedFor === videoId/
  );
  assert.match(
    railSource,
    /nextView === "collection"[\s\S]*?setCollectionLoadStartedFor\(videoId\)/
  );
});

test("desktop collection creates thumbnail resources only near the viewport", () => {
  assert.match(
    railSource,
    /const \[thumbnailActivated, setThumbnailActivated\] = useState\([\s\S]*?variant !== "collection"[\s\S]*?if \(inView\) setThumbnailActivated\(true\)/
  );
  assert.match(
    railSource,
    /<VideoThumbnail[\s\S]*?src=\{video\.thumbnail\}[\s\S]*?enabled=\{thumbnailActivated\}/
  );
  assert.match(
    railSource,
    /function useIsActivePreview\(videoID: string\): boolean[\s\S]*?previewController\.getActiveId\(\) === videoID/
  );
  assert.doesNotMatch(railSource, /function useActivePreviewId/);
  assert.doesNotMatch(railSource, /media\.addEventListener\("change", update\)/);
});

test("desktop collection requests previews and reuses recommendation preview behavior", () => {
  assert.match(dataSource, /options\.includePreview \? "\?preview=1" : ""/);
  assert.match(
    railSource,
    /useLazyVideoCollection\([\s\S]*?\{ includePreview: true \}/
  );
  assert.match(
    railSource,
    /variant="collection"[\s\S]*?shouldRenderPreview && video\.previewSrc[\s\S]*?<PreviewVideo/
  );
  assert.match(
    railSource,
    /previewController\.setActiveId\(video\.id\)/
  );
  assert.match(
    componentSource,
    /useLazyVideoCollection\(videoId, open\)/
  );
});

test("recommendation rail omits retired quality metadata", () => {
  assert.match(
    stylesSource,
    /\.vd-rail__duration,\s*\.vd-rail__current\s*\{\s*z-index:\s*2;/
  );
  assert.doesNotMatch(railSource, /vd-rail__hd|quality === "HD"/);
  assert.doesNotMatch(stylesSource, /\.vd-rail__hd/);
});

test("desktop collection loading state renders six skeleton cards", () => {
  assert.match(railSource, /<VideoRailRowsSkeleton \/>/);
  assert.match(
    railSkeletonSource,
    /className="vd-rail__collection-loading"[\s\S]*?Array\.from\(\{ length: 6 \}\)[\s\S]*?className="vd-rail__loading-row"/
  );
  assert.doesNotMatch(railSkeletonSource, />\s*正在加载相关合集…\s*</);
  assert.match(
    stylesSource,
    /\.vd-rail__loading-row\s*\{[\s\S]*?grid-template-columns:\s*148px minmax\(0, 1fr\);/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__loading-thumb,[\s\S]*?\.vd-rail__loading-body\s*>\s*span\s*\{[\s\S]*?animation:\s*vd-shimmer/
  );
});

test("recommendation loading cards use two top-aligned text bars", () => {
  assert.match(
    railSkeletonSource,
    /className="vd-rail__loading-body">\s*<span \/>\s*<span \/>\s*<\/span>/
  );
  assert.doesNotMatch(
    railSkeletonSource,
    /className="vd-rail__loading-body">\s*(?:<span \/>\s*){3}/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__loading-row\s*\{[^}]*align-items:\s*start;/s
  );
  assert.match(
    stylesSource,
    /\.vd-rail__loading-body\s*>\s*span\s*\{[^}]*width:\s*100%;[^}]*height:\s*13px;/s
  );
  assert.match(
    stylesSource,
    /\.vd-rail__loading-body\s*>\s*span:nth-child\(2\)\s*\{[^}]*width:\s*75%;/s
  );
});

test("mobile collection loading cards match the two-bar skeleton", () => {
  assert.match(
    componentSource,
    /className="vd-collection-sheet__skeleton-body">\s*<span \/>\s*<span \/>\s*<\/span>/
  );
  assert.doesNotMatch(
    componentSource,
    /className="vd-collection-sheet__skeleton-body">\s*(?:<span \/>\s*){3}/
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__skeleton-row\s*\{[^}]*align-items:\s*start;/s
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__skeleton-body span\s*\{[^}]*width:\s*100%;[^}]*height:\s*13px;/s
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__skeleton-body span:nth-child\(2\)\s*\{[^}]*width:\s*75%;/s
  );
  assert.doesNotMatch(
    stylesSource,
    /\.vd-collection-sheet__skeleton-body span:nth-child\(3\)/
  );
});

test("desktop collection stays bounded and positions the current video", () => {
  assert.match(
    stylesSource,
    /\.vd-rail__collection-list,\s*\.vd-rail__collection-loading\s*\{[\s\S]*?max-height:/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__collection-list\s*\{[\s\S]*?overflow-y:\s*auto;[\s\S]*?scrollbar-width:\s*none;/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__collection-list::\-webkit-scrollbar\s*\{[\s\S]*?display:\s*none;/
  );
  assert.match(
    stylesSource,
    /\.vd-rail__collection-item\s*\{[\s\S]*?content-visibility:\s*auto;[\s\S]*?contain-intrinsic-block-size:\s*var\(--vd-rail-collection-row-height\);/
  );
  assert.match(
    stylesSource,
    /\.vd-rail\s*\{[\s\S]*?--vd-rail-collection-row-height:\s*108\.25px;/
  );
  assert.match(
    stylesSource,
    /@media \(max-width:\s*1024px\)[\s\S]*?\.vd-rail\s*\{[\s\S]*?--vd-rail-collection-row-height:\s*137\.5px;/
  );
  assert.match(
    railSource,
    /function alignCollectionItem\([\s\S]*?current\.getBoundingClientRect\(\)[\s\S]*?list\.scrollHeight - list\.clientHeight[\s\S]*?list\.scrollTop = nextScrollTop/
  );
  assert.match(
    railSource,
    /COLLECTION_POSITION_MAX_FRAMES\s*=\s*8[\s\S]*?COLLECTION_POSITION_STABLE_FRAMES\s*=\s*2/
  );
  assert.match(
    railSource,
    /stableFrames =\s*list && current && alignCollectionItem\(list, current\)[\s\S]*?stableFrames < COLLECTION_POSITION_STABLE_FRAMES[\s\S]*?requestAnimationFrame\(positionCurrentItem\)/
  );
  assert.match(
    railSource,
    /return \(\) => window\.cancelAnimationFrame\(frame\)/
  );
  assert.doesNotMatch(
    railSource,
    /collectionScrollPositionRef|handleCollectionScroll/
  );
  assert.match(railSource, /aria-current=\{current \? "page" : undefined\}/);
  assert.match(
    stylesSource,
    /\.vd-rail__link\[aria-current="page"\]\s+\.vd-rail__title\s*\{[\s\S]*?color:\s*var\(--accent-strong\);/
  );
});

test("mobile collection uses an accessible scroll-locked bottom sheet", () => {
  assert.match(componentSource, /createPortal\(/);
  assert.match(componentSource, /role="dialog"/);
  assert.match(componentSource, /aria-modal="true"/);
  assert.match(componentSource, /useDocumentScrollLock\(open\)/);
  assert.match(componentSource, /event\.key !== "Escape"/);
  assert.match(componentSource, /currentItemRef/);
  assert.match(componentSource, /setAscending\(\(value\) => !value\)/);
  assert.match(componentSource, /aria-current=\{current \? "page" : undefined\}/);
});

test("collection view counts use the shared eye icon", () => {
  assert.match(
    componentSource,
    /<Eye size=\{12\} aria-hidden="true" \/>\s*\{formatCount\(video\.views\)\} 次观看/
  );
  assert.doesNotMatch(componentSource, /<Play/);
});

test("collection sheet uses a compact header without shrinking the close hit target", () => {
  assert.match(componentSource, /<X size=\{18\} strokeWidth=\{2\} \/>/);
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__head\s*\{[\s\S]*?min-height:\s*56px;/
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__close\s*\{[\s\S]*?width:\s*44px;[\s\S]*?height:\s*44px;/
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__close::before\s*\{[\s\S]*?width:\s*34px;[\s\S]*?height:\s*34px;/
  );
});

test("current collection item uses only a concise thumbnail badge", () => {
  assert.match(componentSource, />\s*当前视频\s*</);
  const currentBadge = componentSource.match(
    /className="vd-collection-item__current-thumb"([\s\S]*?)<\/span>/
  )?.[1];
  assert.ok(currentBadge);
  assert.doesNotMatch(currentBadge, /<Play/);
  assert.match(
    stylesSource,
    /\.vd-collection-item__current-thumb\s*\{[\s\S]*?border-radius:\s*0;[\s\S]*?background:\s*rgba\(255, 255, 255, 0\.78\);[\s\S]*?color:\s*var\(--accent-strong\);/
  );
  assert.doesNotMatch(componentSource, /正在播放|vd-collection-item__playing|is-current/);
  assert.doesNotMatch(
    stylesSource,
    /\.vd-collection-item\.is-current|\.vd-collection-item__playing/
  );
  assert.match(
    stylesSource,
    /\.vd-collection-item__link\[aria-current="page"\]\s+\.vd-collection-item__title\s*\{[\s\S]*?color:\s*var\(--accent-strong\);/
  );
});

test("collection sheet follows a downward drag and dismisses past a threshold", () => {
  assert.match(componentSource, /onPointerDown=\{beginSheetDrag\}/);
  assert.match(componentSource, /setPointerCapture\(pointerId\)/);
  assert.match(componentSource, /offset >= distanceThreshold/);
  assert.match(componentSource, /velocityY >= SHEET_DISMISS_VELOCITY/);
  assert.match(
    componentSource,
    /SHEET_DISMISS_FLICK_MIN_DISTANCE\s*=\s*64[\s\S]*?offset >= SHEET_DISMISS_FLICK_MIN_DISTANCE/
  );
  assert.match(componentSource, /sheetHeight \* 0\.22/);
  assert.match(componentSource, /finishSheetDrag\(event, true\)/);
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__drag-zone\s*\{[\s\S]*?touch-action:\s*none;/
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet\.is-dragging\s*\{[\s\S]*?transition:\s*none;/
  );
});

test("an incomplete pull snaps back without replaying the entrance animation", () => {
  const sheetBlock = stylesSource.match(
    /\.vd-collection-sheet\s*\{([\s\S]*?)\}/
  )?.[1];
  assert.ok(sheetBlock);
  assert.doesNotMatch(sheetBlock, /animation\s*:/);
  assert.match(
    stylesSource,
    /\.vd-collection-sheet\.is-entering\s*\{[\s\S]*?animation:\s*vd-collection-sheet-in/
  );
  assert.match(
    componentSource,
    /className="vd-collection-sheet is-entering"[\s\S]*?classList\.remove\("is-entering"\)/
  );
  assert.match(
    componentSource,
    /classList\.remove\("is-entering", "is-dismissing"\)/
  );
});

test("pulling an already top-aligned collection list hands off to the sheet", () => {
  assert.match(componentSource, /list\.scrollTop > 1/);
  assert.match(
    componentSource,
    /deltaY <= 0 \|\| Math\.abs\(deltaX\) > Math\.abs\(deltaY\)/
  );
  assert.match(
    componentSource,
    /addEventListener\("touchmove", handleTouchMove, \{ passive: false \}\)/
  );
  assert.match(
    componentSource,
    /event\.preventDefault\(\);[\s\S]*?beginSheetDragAt\([\s\S]*?pull\.touchId/
  );
  assert.match(componentSource, /finishSheetDragAt\([\s\S]*?cancelled/);
});

test("detail, collection, and info cards use a compact mobile stack", () => {
  assert.match(
    detailSource,
    /className="vd-detail-panels"[\s\S]*?className="vd-summary"[\s\S]*?<MobileVideoCollection[\s\S]*?<VideoInfoPanel/
  );
  assert.match(
    stylesSource,
    /@media \(max-width:\s*768px\)[\s\S]*?\.vd-detail-panels\s*\{\s*gap:\s*10px;/
  );
});

test("mobile collection trigger stays hidden on desktop and becomes a bottom sheet on mobile", () => {
  assert.match(
    stylesSource,
    /\.vd-mobile-collection,\s*\.vd-collection-sheet-modal\s*\{\s*display:\s*none;/s
  );
  assert.match(
    stylesSource,
    /@media \(max-width:\s*768px\)\s*\{[\s\S]*?\.vd-mobile-collection\s*\{\s*display:\s*block;/s
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet-modal\s*\{[\s\S]*?position:\s*fixed;[\s\S]*?align-items:\s*flex-end;/s
  );
  assert.match(stylesSource, /env\(safe-area-inset-bottom, 0px\)/);
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__list,[\s\S]*?scrollbar-width:\s*none;/
  );
  assert.match(
    stylesSource,
    /\.vd-collection-sheet__list::\-webkit-scrollbar,[\s\S]*?display:\s*none;/
  );
});
