import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const tagsPageSource = readFileSync(
  new URL("../src/admin/TagsPage.tsx", import.meta.url),
  "utf8"
);
const adminPaginationSource = readFileSync(
  new URL("../src/admin/AdminPagination.tsx", import.meta.url),
  "utf8"
);
const adminCss = readFileSync(new URL("../src/styles/admin.css", import.meta.url), "utf8");

test("admin tags page limits visible tags by viewport", () => {
  assert.match(tagsPageSource, /const DESKTOP_TAGS_PAGE_SIZE = 24;/);
  assert.match(tagsPageSource, /const MOBILE_TAGS_PAGE_SIZE = 8;/);
  assert.match(tagsPageSource, /const TAGS_MOBILE_QUERY = "\(max-width: 640px\)";/);
  assert.match(tagsPageSource, /const pageSize = useTagsPageSize\(\);/);
  assert.match(tagsPageSource, /window\.matchMedia\(TAGS_MOBILE_QUERY\)/);
});

test("admin tags page renders only the current page", () => {
  assert.match(tagsPageSource, /filteredTags\.slice\(pageStartIndex, pageEndIndex\)/);
  assert.match(tagsPageSource, /pagedTags\.map\(\(tag\) =>/);
  assert.doesNotMatch(tagsPageSource, /filteredTags\.map\(\(tag\) =>/);
  assert.match(tagsPageSource, /全选本页/);
});

test("admin tag pagination matches the compact video pagination and hides for a single page", () => {
  assert.match(tagsPageSource, /const showPagination = filteredTags\.length > pageSize;/);
  assert.match(tagsPageSource, /\{showPagination && \(/);
  assert.match(
    tagsPageSource,
    /<AdminPagination\s+page=\{currentPage\}[\s\S]*?totalPages=\{totalPages\}[\s\S]*?total=\{filteredTags\.length\}[\s\S]*?itemLabel="标签"[\s\S]*?onPage=\{setPage\}\s*\/>/
  );
  assert.match(adminPaginationSource, /第 \{page\} \/ \{totalPages\} 页 · 共 \{total\} 个\{itemLabel\}/);
  assert.equal(Array.from(adminPaginationSource.matchAll(/上一页|下一页/g)).length, 2);
  assert.doesNotMatch(adminPaginationSource, /首页|末页/);
  assert.doesNotMatch(tagsPageSource, /显示 \{pageStart\}-\{pageEnd\}/);
  assert.doesNotMatch(tagsPageSource, /每页 \{pageSize\} 个/);
});

test("admin tag pagination does not create invisible rows on a short final page", () => {
  assert.doesNotMatch(tagsPageSource, /placeholderTags|admin-tag-card--placeholder/);
  assert.doesNotMatch(adminCss, /\.admin-tag-card--placeholder/);
});

test("admin tags loading keeps the fixed controls and leaves the card area blank", () => {
  assert.doesNotMatch(tagsPageSource, /AdminLoading|lds-ellipsis|admin-card-skeleton-surface/);
  assert.match(
    tagsPageSource,
    /<div className="admin-tags-toolbar">[\s\S]*?\{tagsEmpty \? \(/
  );
  assert.match(
    tagsPageSource,
    /<div className="admin-tags-board" aria-busy=\{loading \|\| tagsResource\.refreshing \|\| undefined\}>\s*<div className="admin-tags-cards">\s*\{loading \? null : loadError \? \(/
  );
});

test("admin tag empty states distinguish an empty catalog from no results", () => {
  assert.match(tagsPageSource, /const tagsEmpty = !loading && !loadError && stats\.total === 0;/);
  assert.match(tagsPageSource, /const resultsEmpty = !tagsEmpty && !loading && !loadError && filteredTags\.length === 0;/);
  assert.match(
    tagsPageSource,
    /tagsEmpty \? \(\s*<AdminEmptyVisual[\s\S]*?variant="empty"[\s\S]*?text="当前没有标签"[\s\S]*?admin-tags-empty-state[\s\S]*?\) : resultsEmpty \? \(\s*<AdminEmptyVisual[\s\S]*?variant="no-results"[\s\S]*?text="未查询到"[\s\S]*?admin-tags-empty-state[\s\S]*?\) : \(\s*<div className="admin-tags-board" aria-busy=\{loading \|\| tagsResource\.refreshing \|\| undefined\}>/
  );
  assert.doesNotMatch(tagsPageSource, /没有找到匹配的标签。|className="admin-card admin-empty"/);
  assert.match(
    tagsPageSource,
    /className="admin-page admin-page--with-floating-actions admin-tags-page"/
  );
  assert.match(
    adminCss,
    /\.admin-tags-layout\s*\{[^}]*display\s*:\s*grid;[^}]*flex\s*:\s*1 1 auto;[^}]*align-items\s*:\s*stretch;[^}]*min-height\s*:\s*0/s
  );
  assert.match(
    adminCss,
    /\.admin-tags-main\s*\{[^}]*display\s*:\s*flex;[^}]*flex-direction\s*:\s*column;[^}]*min-height\s*:\s*0/s
  );
  assert.match(
    adminCss,
    /\.admin-tags-empty-state\s*\{[^}]*box-sizing\s*:\s*border-box;[^}]*flex\s*:\s*1 1 auto;[^}]*min-height\s*:\s*0;[^}]*padding\s*:\s*0 16px 96px/s
  );
  assert.doesNotMatch(adminCss, /\.admin-tags-page\.is-search-empty\s*\{[^}]*100(?:d)?vh/s);
  assert.doesNotMatch(adminCss, /\.admin-tags-page\.is-search-empty \.admin-tags-board[\s\S]*?display\s*:\s*flex/);
});

test("admin tags hide bulk delete when the catalog is empty", () => {
  assert.match(
    tagsPageSource,
    /\{stats\.total > 0 && \(\s*<button[\s\S]*?admin-tags-toolbar-actions__toggle[\s\S]*?>\s*批量删除\s*<\/button>\s*\)\}/
  );
});
