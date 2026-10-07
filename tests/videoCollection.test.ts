import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import postcss from "postcss";
import { createMemoryRouter } from "react-router";

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
const railMobileHeadingSource = readFileSync(
  new URL("../src/components/VideoRailMobileHeading.tsx", import.meta.url),
  "utf8"
);
const collectionHookSource = readFileSync(
  new URL("../src/lib/useLazyVideoCollection.ts", import.meta.url),
  "utf8"
);
const cardPreviewHookSource = readFileSync(
  new URL("../src/lib/useCardPreview.ts", import.meta.url),
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
    /collectionSummary && \([\s\S]*?<MobileVideoCollection[\s\S]*?videoId=\{detail\.id\}[\s\S]*?collection=\{collectionSummary\}/
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
    /`\/api\/video\/\$\{encodeURIComponent\(id\)\}\/collection\/summary`/
  );
  assert.match(
    detailSource,
    /if \(!id \|\| deletedVideoIDs\.has\(id\) \|\| !detail\?\.collectionCandidate\) \{[\s\S]*?return;[\s\S]*?fetchVideoCollectionSummary\(id, \{ signal: controller\.signal \}\)/
  );
  assert.match(detailSource, /cachedCollectionSummariesByID/);
  assert.match(
    dataSource,
    /`\/api\/video\/\$\{encodeURIComponent\(id\)\}\/collection\$\{previewQuery\}`/
  );
  assert.match(
    componentSource,
    /useLazyVideoCollection\(\s*videoId,\s*open,\s*\{ includePreview: true \}\s*\)/
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

test("desktop recommendation rail always uses tabs while mobile keeps its heading", () => {
  assert.match(
    detailSource,
    /<RecommendedRail[\s\S]*?videos=\{visibleRecommendations\}[\s\S]*?videoId=\{detail\.id\}[\s\S]*?collection=\{collectionSummary \?\? undefined\}[\s\S]*?recommendationsLoading=\{recommendationsLoading\}[\s\S]*?recommendationsError=\{recommendationsError\}/
  );
  assert.match(
    railSource,
    /className="content-tabs vd-rail__tabs"[\s\S]*?role="tablist"[\s\S]*?>\s*推荐视频\s*<[\s\S]*?>\s*相关合集\s*</
  );
  assert.doesNotMatch(
    railSource,
    /\{hasCollection && \(\s*<div[\s\S]*?className="content-tabs vd-rail__tabs"/
  );
  assert.match(
    railSource,
    /desktop && hasCollection && activeView === "collection"/
  );
  assert.match(
    railSource,
    /className="content-tabs__tab vd-rail__tab"[\s\S]*?aria-selected=\{activeView === "recommended"\}/
  );
  assert.match(
    railSource,
    /className="content-tabs__tab vd-rail__tab"[\s\S]*?aria-selected=\{showCollection\}[\s\S]*?disabled=\{!hasCollection\}/
  );
  assert.doesNotMatch(stylesSource, /\.vd-rail__tab\[aria-selected="true"\]/);
  assert.match(
    railMobileHeadingSource,
    /<header className="vd-rail__head vd-rail__head--mobile-only">/
  );
  assert.match(
    railMobileHeadingSource,
    /import \{ ListCollapse \} from "lucide-react";[\s\S]*?<ListCollapse className="vd-rail__head-icon" aria-hidden="true" \/>/
  );
  assert.match(railSource, /<VideoRailMobileHeading \/>/);
  assert.match(railSkeletonSource, /<VideoRailMobileHeading \/>/);
  assert.match(
    stylesSource,
    /\.vd-rail__head-icon\s*\{[^}]*flex:\s*0 0 24px;[^}]*width:\s*24px;[^}]*height:\s*24px;/s
  );
  assert.doesNotMatch(stylesSource, /\.vd-rail__head-icon span/);
  assert.match(
    stylesSource,
    /\.vd-rail__head\.vd-rail__head--mobile-only\s*\{\s*display:\s*none;/
  );
  const styles = postcss.parse(stylesSource);
  for (const [selector, display] of [
    [".vd-rail--collection-only", "none"],
    [".vd-rail__tabs", "none"],
    [".vd-rail__tabpanel--collection", "none"],
    [".vd-rail__head.vd-rail__head--mobile-only", "flex"],
  ]) {
    const queries: string[] = [];
    styles.walkRules((rule) => {
      if (!rule.selectors.includes(selector)) return;
      if (!rule.nodes.some((node) =>
        node.type === "decl" && node.prop === "display" && node.value === display
      )) return;
      const media = rule.parent;
      assert.ok(media?.type === "atrule" && media.name === "media");
      queries.push(media.params);
    });
    assert.deepEqual(queries, ["(max-width: 768px)"], selector);
  }
});

test("recommendation loading and failures stay inside the independent rail", () => {
  assert.match(
    railSource,
    /recommendationsLoading && !hasRecommendations \? \([\s\S]*?<VideoRailRowsSkeleton label="正在加载推荐视频" \/>/
  );
  assert.match(
    railSource,
    /recommendationsError && !hasRecommendations \? \([\s\S]*?role="alert"[\s\S]*?onClick=\{onRetryRecommendations\}/
  );
  assert.match(
    detailSource,
    /onRetryRecommendations=\{\(\) =>\s*setRecommendationsLoadVersion\(\(version\) => version \+ 1\)/
  );
  assert.doesNotMatch(
    detailSource,
    /Promise\.all\(\[[^\]]*detailRequest[^\]]*recommendationsRequest/
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
    /active=\{!showCollection\}/
  );
  assert.match(railSource, /active=\{showCollection && desktop\}/);
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
    cardPreviewHookSource,
    /if \(!previewEnabled \|\| !active \|\| !inView\) stopPreview\(\)/
  );
  assert.match(railSource, /import \{ useCardPreview \}/);
  assert.doesNotMatch(railSource, /function useActivePreviewId/);
  assert.doesNotMatch(railSource, /media\.addEventListener\("change", update\)/);
});

test("desktop and mobile collections request previews and share preview behavior", () => {
  assert.match(dataSource, /options\.includePreview \? "\?preview=1" : ""/);
  assert.match(
    railSource,
    /useLazyVideoCollection\([\s\S]*?\{ includePreview: true \}/
  );
  assert.match(
    railSource,
    /variant="collection"[\s\S]*?shouldRenderPreview && video\.previewSrc[\s\S]*?<PreviewVideo/
  );
  assert.match(railSource, /useCardPreview\(\{ id: video\.id, src: video\.previewSrc, inView, active \}\)/);
  assert.match(
    componentSource,
    /useLazyVideoCollection\(\s*videoId,\s*open,\s*\{ includePreview: true \}\s*\)/
  );
  assert.match(
    componentSource,
    /function CollectionItem\([\s\S]*?video\.previewSrc[\s\S]*?<PreviewVideo/
  );
  assert.match(
    componentSource,
    /className="vd-collection-item__thumb"[\s\S]*?onPointerEnter=\{handlePointerEnter\}[\s\S]*?onTouchStart=\{startPreview\}/
  );
  assert.match(
    componentSource,
    /onClick=\{\(event\) => \{\s*stopPreview\(\);\s*onSelect\(event\)/
  );
  assert.match(componentSource, /import \{ useCardPreview \}/);
  for (const source of [componentSource, railSource]) {
    assert.doesNotMatch(source, /setActiveId|getActiveId|shouldInterceptPreviewTap|TOUCH_PREVIEW_DELAY_MS/);
    assert.match(source, /onPlay=\{handlePreviewPlay\}[\s\S]*?onEnded=\{stopPreview\}/);
    assert.match(source, /<PreviewLoader onFinish=\{finishPreviewLoader\}/);
  }
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

test("mobile collection titles align with the top of their thumbnails", () => {
  assert.match(
    stylesSource,
    /\.vd-collection-item__link\s*\{[^}]*align-items:\s*start;/s
  );
  assert.doesNotMatch(
    stylesSource,
    /\.vd-collection-item__body\s*\{[^}]*align-self:\s*stretch;/s
  );
  assert.doesNotMatch(
    stylesSource,
    /\.vd-collection-item__body\s*\{[^}]*justify-content:\s*center;/s
  );
});

test("desktop collection stays bounded and positions the current video", () => {
  assert.match(
    stylesSource,
    /\.vd-rail__collection-list,\s*\.vd-rail__collection-loading\s*\{[\s\S]*?max-height:\s*calc\(var\(--vd-rail-collection-row-height\) \* 6\);/
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

test("mobile collection uses native close requests with a history fallback", () => {
  assert.match(componentSource, /useNavigate\(\)/);
  assert.match(
    componentSource,
    /const historyOpen = collectionSheetVideoId\(locationState\) === videoId/
  );
  assert.match(
    componentSource,
    /const open = historyOpen \|\| nativeOpenKey === location\.key/
  );
  assert.match(
    componentSource,
    /if \(supportsNativeBack\(\)\) \{\s*setNativeOpenKey\(location\.key\);\s*return;/
  );
  assert.match(
    componentSource,
    /useNativeBackHandler\(open, \(\) => closeSheet\(true, false\)\)/
  );
  assert.match(
    componentSource,
    /navigate\(routeToPath\(location\), \{[\s\S]*?\[COLLECTION_SHEET_HISTORY_STATE_KEY\]: \{ videoId \}/
  );
  assert.match(
    componentSource,
    /function closeSheet\([\s\S]*?if \(historyOpen\) navigate\(-1\);\s*else setNativeOpenKey\(null\)/
  );
  assert.match(
    componentSource,
    /const detailNavigationState = useMemo\([\s\S]*?continueVideoDetailNavigationState\(returnPath, location\.state\)/
  );
  assert.match(
    componentSource,
    /replaceHistory=\{historyOpen\}/
  );
  assert.match(
    componentSource,
    /<Link\s+to=\{video\.href\}\s+replace=\{replaceHistory\}\s+state=\{navigationState\}/
  );
});

test("history fallback closes the collection layer before leaving the video", async () => {
  const router = createMemoryRouter([{ path: "*", element: null }], {
    initialEntries: [
      "/list",
      { pathname: "/video/current", state: { from: "/list" } },
    ],
    initialIndex: 1,
  });

  try {
    await router.navigate("/video/current", {
      state: {
        from: "/list",
        mobileVideoCollection: { videoId: "current" },
      },
    });
    await router.navigate(-1);

    assert.equal(router.state.location.pathname, "/video/current");
    assert.deepEqual(router.state.location.state, { from: "/list" });

    await router.navigate(-1);
    assert.equal(router.state.location.pathname, "/list");
  } finally {
    router.dispose();
  }
});

test("selecting another collection item replaces the fallback history layer", async () => {
  const router = createMemoryRouter([{ path: "*", element: null }], {
    initialEntries: [
      "/list",
      { pathname: "/video/current", state: { from: "/list" } },
    ],
    initialIndex: 1,
  });

  try {
    await router.navigate("/video/current", {
      state: {
        from: "/list",
        mobileVideoCollection: { videoId: "current" },
      },
    });
    await router.navigate("/video/next", {
      replace: true,
      state: { from: "/list" },
    });
    await router.navigate(-1);

    assert.equal(router.state.location.pathname, "/video/current");
    assert.deepEqual(router.state.location.state, { from: "/list" });
  } finally {
    router.dispose();
  }
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
  assert.doesNotMatch(componentSource, /velocityY|SHEET_DISMISS_(?:FLICK|VELOCITY)/);
  assert.match(componentSource, /SHEET_DISMISS_HEIGHT_RATIO\s*=\s*0\.25/);
  assert.match(
    componentSource,
    /sheetHeight \* SHEET_DISMISS_HEIGHT_RATIO/
  );
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

test("collection toolbar is part of the drag surface without stealing controls", () => {
  assert.match(
    componentSource,
    /<header className="vd-collection-sheet__head">[\s\S]*?<\/header>\s*<div className="vd-collection-sheet__toolbar">[\s\S]*?<\/div>\s*<\/div>\s*\{loading && !data/
  );
  assert.match(
    componentSource,
    /if \(target\.closest\("button, a, input, select, textarea"\)\) return/
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

test("mobile recommendations use one layout gap without stacked top spacing", () => {
  const mobileStyles = stylesSource.slice(
    stylesSource.lastIndexOf("@media (max-width: 768px)")
  );
  assert.match(
    mobileStyles,
    /\.vd-layout\s*\{\s*gap:\s*var\(--space-4\);/
  );

  const railRules = Array.from(mobileStyles.matchAll(/\.vd-rail\s*\{([^}]*)\}/g));
  assert.ok(railRules.length > 0);
  for (const [, rule] of railRules) {
    assert.match(rule, /border-top:\s*0;/);
    assert.match(rule, /padding-top:\s*0;/);
  }

  assert.match(
    mobileStyles,
    /\.vd-rail__head\s*\{\s*padding:\s*0 0 var\(--space-3\);/
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
