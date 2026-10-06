import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { listTags } from "../src/admin/api.ts";

const apiSource = readFileSync(
  new URL("../src/admin/api.ts", import.meta.url),
  "utf8"
);
const tagsPageSource = readFileSync(
  new URL("../src/admin/TagsPage.tsx", import.meta.url),
  "utf8"
);
const adminCss = readFileSync(
  new URL("../src/styles/admin.css", import.meta.url),
  "utf8"
);
const tokensCss = readFileSync(
  new URL("../src/styles/tokens.css", import.meta.url),
  "utf8"
);
const videosPageSource = readFileSync(
  new URL("../src/admin/VideosPage.tsx", import.meta.url),
  "utf8"
);

function ruleBody(css: string, selector: string): string {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  const match = css.match(new RegExp(`${escapedSelector}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `Expected CSS rule for ${selector}`);
  return match[1];
}

test("admin tags API treats a legacy null collection as empty", async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = (async () =>
    new Response("null", {
      status: 200,
      headers: { "Content-Type": "application/json" },
    })) as typeof fetch;

  try {
    assert.deepEqual(await listTags(), []);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test("admin tags manage custom and generated tags", () => {
  assert.match(apiSource, /export type TagMatchRules/);
  assert.match(apiSource, /matchRules\?: \{/);
  assert.match(apiSource, /keywords\?: string\[\]/);
  assert.doesNotMatch(apiSource, /\n\s+words\?: string\[\];/);
  assert.doesNotMatch(apiSource, /\n\s+excludes\?: string\[\];/);
  assert.match(apiSource, /matchAvCode\?: boolean/);
  assert.match(apiSource, /avCodePrefixes\?: string\[\]/);
  assert.match(apiSource, /export function updateTag/);
  assert.match(apiSource, /updateTag\(id: number, matchRules: TagMatchRules\)/);
  assert.doesNotMatch(apiSource, /export function startTagRetag/);
  assert.doesNotMatch(apiSource, /export function getTagJobStatus/);
  assert.doesNotMatch(apiSource, /autoGenerateTagsEnabled: boolean/);
  assert.doesNotMatch(apiSource, /startTagLlmRun/);
  assert.doesNotMatch(apiSource, /llmEnabled|llmPending/);
  assert.doesNotMatch(tagsPageSource, /编辑标签：/);
  assert.doesNotMatch(tagsPageSource, /<h1 className="admin-page__title">标签管理<\/h1>/);
  assert.match(tagsPageSource, /<div className="admin-tags-board" aria-busy=\{loading \|\| tagsResource\.refreshing \|\| undefined\}>/);
  assert.match(tagsPageSource, /<aside className="admin-tags-filter-panel" aria-label="标签分类">/);
  assert.match(tagsPageSource, /<div className="admin-tags-main">/);
  assert.ok(
    tagsPageSource.indexOf('className="admin-tags-filter-panel"') <
      tagsPageSource.indexOf('className="admin-tags-toolbar-actions"'),
    "tag source filter should appear before the toolbar actions"
  );
  assert.doesNotMatch(tagsPageSource, /SearchPanel|admin-tags-search|searchQuery|setSearchQuery/);
  assert.doesNotMatch(tagsPageSource, /搜索标签名或包含词|搜索标签名或规则词/);
  assert.match(tagsPageSource, /admin-tags-filter-tab__text/);
  assert.doesNotMatch(tagsPageSource, /admin-tags-filter-tab__count/);
  assert.doesNotMatch(tagsPageSource, /aria-label=\{`\$\{label\} \(\$\{count\}\)`\}/);
  assert.doesNotMatch(tagsPageSource, /aria-label=\{`全部 \(\$\{stats\.total\}\)`\}/);
  assert.match(tagsPageSource, /添加标签/);
  assert.match(tagsPageSource, /onClick=\{openCreateModal\}/);
  assert.match(tagsPageSource, /className="admin-btn admin-tags-toolbar-actions__create"\s+onClick=\{openCreateModal\}/);
  assert.match(tagsPageSource, /<Plus size="1em" aria-hidden="true" \/>\s*新增标签/);
  assert.match(tagsPageSource, /\{!selectMode && \(\s*<div className="admin-tags-toolbar-actions" data-admin-floating-actions>[\s\S]*?<button[\s\S]*?admin-tags-toolbar-actions__create/);
  assert.match(tagsPageSource, /const createLabelExists = useMemo/);
  assert.match(tagsPageSource, /tag\.label\.trim\(\)\.toLowerCase\(\) === cleanLabel/);
  assert.match(tagsPageSource, /if \(createLabelExists\) return;/);
  assert.match(tagsPageSource, /disabled=\{saving \|\| !label\.trim\(\) \|\| createLabelExists\}/);
  assert.match(tagsPageSource, /aria-label="输入标签名"/);
  assert.match(tagsPageSource, /aria-describedby=\{createLabelExists \? "admin-tag-create-warning" : undefined\}/);
  assert.match(tagsPageSource, /placeholder="输入标签名"/);
  assert.match(tagsPageSource, /className=\{`admin-tag-create-warning\$\{createLabelExists \? " is-visible" : ""\}`\}/);
  assert.match(tagsPageSource, /aria-hidden=\{!createLabelExists\}/);
  assert.match(tagsPageSource, /当前标签已存在/);
  assert.match(tagsPageSource, /\{saving \? "添加中\.\.\." : "确认"\}/);
  assert.doesNotMatch(tagsPageSource, /<label htmlFor="admin-tag-label">标签名<\/label>/);
  assert.doesNotMatch(tagsPageSource, /placeholder="例如：清纯"/);
  assert.doesNotMatch(tagsPageSource, /<Plus size=\{13\} \/> 新增标签/);
  assert.doesNotMatch(tagsPageSource, /className="admin-btn is-primary"\s+onClick=\{openCreateModal\}/);
  assert.match(tagsPageSource, /form="admin-create-tag-form"/);
  assert.match(tagsPageSource, /confirmText="确认"/);
  assert.doesNotMatch(tagsPageSource, /confirmText="确认删除"/);
  assert.doesNotMatch(tagsPageSource, /admin-card__title[\s\S]*新增标签/);
  assert.doesNotMatch(tagsPageSource, /系统不会再从文件名或标题自动创建标签/);
  assert.doesNotMatch(tagsPageSource, /包含词（子串）/);
  assert.doesNotMatch(tagsPageSource, /识别文件名和标题中的番号/);
  assert.doesNotMatch(tagsPageSource, /重新整理所有标签/);
  assert.doesNotMatch(tagsPageSource, /<RefreshCw size=\{13\} \/> 刷新/);
  assert.doesNotMatch(tagsPageSource, /自动生成标签/);
  assert.doesNotMatch(tagsPageSource, /autoGenerateTagsEnabled/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-setting-toggle__switch/);
  assert.doesNotMatch(tagsPageSource, /role="switch"/);
  assert.doesNotMatch(tagsPageSource, /onClick=\{toggleAutoGenerateTags\}/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-setting-toggle__hint/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-setting-toggle__body/);
  assert.doesNotMatch(tagsPageSource, /关闭后扫描只匹配已有标签。/);
  assert.doesNotMatch(tagsPageSource, /<strong>\{autoGenerateTagsEnabled \? "开启" : "关闭"\}<\/strong>/);
  assert.doesNotMatch(tagsPageSource, /AI 辅助打标|AI 打标|tagging\.llm/);
  assert.match(tagsPageSource, /const TAG_SOURCE_FILTERS = \["user", "generated"\]/);
  assert.match(tagsPageSource, /function tagSourceKey/);
  assert.match(tagsPageSource, /tag\.crawlerOwned \|\| tag\.source === "generated" \? "generated" : tag\.source/);
  assert.match(tagsPageSource, /source === "crawler" \|\| source === "generated"/);
  assert.match(tagsPageSource, /tagCardSourceLabel\(tag\)/);
  assert.match(tagsPageSource, /data-source=\{tagCardSourceKey\(tag\)\}/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-card__id/);
  assert.doesNotMatch(tagsPageSource, /#\{tag\.id\}/);
  assert.match(tagsPageSource, /function tagCardSourceLabel/);
  assert.match(tagsPageSource, /function tagCardSourceKey/);
  assert.match(tagsPageSource, /tag\.crawlerOwned \|\| tag\.source === "crawler"/);
  assert.match(tagsPageSource, /return "爬虫脚本"/);
  assert.match(tagsPageSource, /return "crawler"/);
  assert.match(tagsPageSource, /tag\.source === "generated"/);
  assert.doesNotMatch(tagsPageSource, /return "AV"/);
  assert.doesNotMatch(tagsPageSource, /return "av"/);
  assert.doesNotMatch(tagsPageSource, /const displayAliases = tagDisplayAliases\(tag\);/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-card__aliases/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-card__alias-pill/);
  assert.doesNotMatch(tagsPageSource, /function tagDisplayAliases/);
  assert.match(tagsPageSource, /avCodePrefixes: joinRuleTerms\(rules\.avCodePrefixes\)/);
  assert.doesNotMatch(tagsPageSource, /ADMIN_SEARCH_DEBOUNCE_MS|searchInput|setSearchInput/);
  assert.match(
    tagsPageSource,
    /<AdminPagination\s+page=\{currentPage\}[\s\S]*?totalPages=\{totalPages\}[\s\S]*?total=\{filteredTags\.length\}[\s\S]*?itemLabel="标签"[\s\S]*?onPage=\{setPage\}\s*\/>/
  );
  assert.doesNotMatch(tagsPageSource, /首页|末页|admin-tags-pagination|admin-table-pagination__info/);
  assert.doesNotMatch(tagsPageSource, /admin-tag-card__keywords|admin-tag-card__keyword-pill|tagKeywordTerms/);
  assert.doesNotMatch(tagsPageSource, /function uniqueDisplayAliases/);
  assert.doesNotMatch(tagsPageSource, /系统内置车牌已自动参与匹配/);
  assert.match(tagsPageSource, /const TAG_DISPLAY_GROUP_ORDER: Record<string, number>/);
  assert.match(tagsPageSource, /function tagDisplayGroupKey/);
  assert.match(tagsPageSource, /function tagDisplayGroupRank/);
  assert.match(tagsPageSource, /if \(filterSource !== "all"\) return matches;/);
  assert.match(tagsPageSource, /tagDisplayGroupRank\(a\.tag\) - tagDisplayGroupRank\(b\.tag\)/);
  assert.match(tagsPageSource, /return rankDelta \|\| a\.index - b\.index;/);
  assert.doesNotMatch(tagsPageSource, /sourceLabel\(tag\.source,\s*tag\)/);
  assert.match(tagsPageSource, /return "自动生成"/);
  assert.doesNotMatch(tagsPageSource, /return "爬虫"/);
  assert.doesNotMatch(tagsPageSource, /return "系统"/);
  assert.doesNotMatch(tagsPageSource, /return "旧数据"/);
  assert.doesNotMatch(tagsPageSource, /tag\.source !== "system"/);
  assert.doesNotMatch(tagsPageSource, /tag\.source === "system"\) return/);
  assert.match(adminCss, /\.admin-tag-card\.is-selectable:focus-visible\s*\{[^}]*outline\s*:\s*2px solid var\(--border-accent\)/s);
  assert.doesNotMatch(adminCss, /\.admin-tag-card\.is-selectable:focus-within\s*\{[^}]*box-shadow/s);
  assert.match(adminCss, /\.admin-tag-card:not\(\.is-selectable\):hover\s*\{/);
});

test("admin tags batch delete runs deletions sequentially", () => {
  assert.match(tagsPageSource, /for \(const id of ids\) \{/);
  assert.match(tagsPageSource, /await api\.deleteTag\(id\);/);
  assert.doesNotMatch(
    tagsPageSource,
    /Promise\.allSettled\(\s*ids\.map\(\(id\) => api\.deleteTag\(id\)\)\s*\)/
  );
});

test("admin tag card delete action appears before edit action", () => {
  const actionsStart = tagsPageSource.indexOf('className="admin-tag-card__footer-actions"');
  const deleteIndex = tagsPageSource.indexOf('className="admin-tag-card__delete"', actionsStart);
  const editIndex = tagsPageSource.indexOf('className="admin-tag-card__edit"', actionsStart);
  const actionsEnd = tagsPageSource.indexOf("</div>", editIndex);
  const actionsSource = tagsPageSource.slice(actionsStart, actionsEnd);

  assert.ok(actionsStart >= 0, "tag card footer actions should exist");
  assert.ok(deleteIndex > actionsStart, "delete action should be inside tag card actions");
  assert.ok(editIndex > deleteIndex, "edit action should stay to the right of delete action");
  assert.doesNotMatch(actionsSource, /<Trash2\b|<Pencil\b/);
  assert.doesNotMatch(tagsPageSource, /import \{[^}]*\bPencil\b/);
});

test("mobile tag delete action only opens for the tapped card", () => {
  assert.match(tagsPageSource, /activeTagActionsId/);
  assert.match(tagsPageSource, /const actionsOpen = activeTagActionsId === tag\.id;/);
  assert.match(tagsPageSource, /is-actions-open/);
  assert.match(tagsPageSource, /onClick=\{\(\) => toggleTagActions\(tag\.id\)\}/);
  assert.match(tagsPageSource, /onKeyDown=\{\(event\) => handleTagCardKeyDown\(event, tag\.id\)\}/);
  assert.match(tagsPageSource, /event\.stopPropagation\(\);\s*handleDelete\(tag\);/);
  assert.match(tagsPageSource, /event\.stopPropagation\(\);\s*setEditingTag\(tag\);/);
  assert.match(
    adminCss,
    /@media \(hover: none\)\s*\{[\s\S]*?\.admin-tag-card__delete\s*\{[\s\S]*?opacity\s*:\s*0;[\s\S]*?width\s*:\s*0;[\s\S]*?\.admin-tag-card\.is-actions-open \.admin-tag-card__delete\s*\{[\s\S]*?opacity\s*:\s*1;[\s\S]*?width\s*:\s*auto;/s
  );
});

test("admin tag dialogs use the lightweight modal style", () => {
  assert.match(
    tagsPageSource,
    /modalClassName="admin-modal--delete-confirm admin-modal--tag-dialog admin-modal--tag-delete-confirm"/
  );
  assert.match(
    tagsPageSource,
    /title="新增标签"[\s\S]*?className="admin-modal--tag-rules admin-modal--tag-dialog admin-modal--tag-create"/
  );
  assert.match(tagsPageSource, /className="admin-modal--tag-rules admin-modal--tag-dialog admin-modal--tag-create"/);
  assert.match(tagsPageSource, /restoreFocus=\{false\}/);
  assert.match(adminCss, /\.admin-modal--tag-dialog\s*\{[^}]*border\s*:\s*0/s);
  assert.match(adminCss, /\.admin-modal--tag-dialog \.admin-modal__header\s*\{[^}]*border-bottom\s*:\s*0/s);
  assert.match(adminCss, /\.admin-modal--tag-dialog \.admin-modal__footer\s*\{[^}]*border-top\s*:\s*0/s);
  assert.match(adminCss, /\.admin-modal--tag-create \.admin-modal__body\s*\{[^}]*padding-bottom\s*:\s*6px/s);
  assert.match(adminCss, /\.admin-modal--tag-create \.admin-modal__footer\s*\{[^}]*padding-top\s*:\s*6px/s);
  assert.match(adminCss, /\.admin-tag-create-row\s*\{[^}]*position\s*:\s*relative/s);
  assert.match(adminCss, /\.admin-tag-create-warning\s*\{[^}]*color\s*:\s*var\(--danger\)/s);
  assert.match(adminCss, /\.admin-tag-create-warning\s*\{[^}]*position\s*:\s*absolute/s);
  assert.doesNotMatch(adminCss, /\.admin-tag-create-warning\s*\{[^}]*min-height/s);
  assert.match(adminCss, /\.admin-tag-create-warning\s*\{[^}]*visibility\s*:\s*hidden/s);
  assert.match(adminCss, /\.admin-tag-create-warning\.is-visible\s*\{[^}]*visibility\s*:\s*visible/s);
  assert.match(adminCss, /\.admin-modal--tag-delete-confirm \.admin-confirm\s*\{[^}]*display\s*:\s*block/s);
});

test("admin tag source badges share one readable palette across themes", () => {
  const badge = ruleBody(adminCss, ".admin-tag-card__source-badge");
  const userBadge = ruleBody(adminCss, '.admin-tag-card__source-badge[data-source="user"]');
  const generatedBadge = ruleBody(adminCss, '.admin-tag-card__source-badge[data-source="generated"]');
  const crawlerBadge = ruleBody(adminCss, '.admin-tag-card__source-badge[data-source="crawler"]');
  const darkTokens = ruleBody(tokensCss, ':root[data-theme="dark"]');
  const pinkTokens = ruleBody(tokensCss, ':root[data-theme="pink"]');
  const skyTokens = ruleBody(tokensCss, ':root[data-theme="sky"]');

  assert.match(badge, /font-weight\s*:\s*var\(--weight-medium\)/);
  assert.match(badge, /background\s*:\s*var\(--tag-source-bg/);
  assert.match(badge, /color\s*:\s*var\(--tag-source-fg/);
  assert.doesNotMatch(badge, /box-shadow/);
  assert.doesNotMatch(adminCss, /--tag-source-border/);
  assert.doesNotMatch(adminCss, /:root\[data-theme="(?:pink|sky)"\] \.admin-tag-card__source-badge/);

  assert.match(userBadge, /--tag-source-bg\s*:\s*var\(--tag-source-user-bg\)/);
  assert.match(userBadge, /--tag-source-fg\s*:\s*var\(--tag-source-user-fg\)/);
  assert.doesNotMatch(adminCss, /data-source="av"/);
  assert.match(generatedBadge, /--tag-source-bg\s*:\s*var\(--tag-source-generated-bg\)/);
  assert.match(generatedBadge, /--tag-source-fg\s*:\s*var\(--tag-source-generated-fg\)/);
  assert.match(crawlerBadge, /--tag-source-bg\s*:\s*var\(--tag-source-crawler-bg\)/);
  assert.match(crawlerBadge, /--tag-source-fg\s*:\s*var\(--tag-source-crawler-fg\)/);

  assert.match(darkTokens, /--tag-source-user-bg\s*:\s*#dff3ec/);
  assert.match(darkTokens, /--tag-source-user-fg\s*:\s*#126b47/);
  assert.match(darkTokens, /--tag-source-generated-bg\s*:\s*#f9eddc/);
  assert.match(darkTokens, /--tag-source-generated-fg\s*:\s*#7a4a00/);
  assert.match(darkTokens, /--tag-source-crawler-bg\s*:\s*#eeeafc/);
  assert.match(darkTokens, /--tag-source-crawler-fg\s*:\s*#62429b/);
  assert.doesNotMatch(pinkTokens, /--tag-source-/);
  assert.doesNotMatch(skyTokens, /--tag-source-/);
});

test("admin tag edit dialog edits match rules directly", () => {
  const editModalStart = tagsPageSource.indexOf("function EditTagModal");
  const editModalSource = tagsPageSource.slice(editModalStart);
  assert.ok(editModalStart >= 0, "EditTagModal should exist");
  assert.match(editModalSource, /const \[draft, setDraft\] = useState\(\(\) => tagRuleDraft\(tag\)\)/);
  assert.match(editModalSource, /const parsedRules = matchRulesFromDraft\(nextDraft, isAV\);/);
  assert.match(editModalSource, /title=\{tag\.label\}/);
  assert.doesNotMatch(editModalSource, /footer=\{[\s\S]*?保存中|footer=\{[\s\S]*?取消/);
  assert.match(editModalSource, /inputId="admin-tag-rule-keywords"/);
  assert.match(editModalSource, /function KeywordPillEditor/);
  assert.match(editModalSource, /function PrefixPillEditor/);
  assert.match(editModalSource, /function RulePillEditor/);
  assert.match(editModalSource, /onCommit=\{\(value\) => void persistDraft\(\{ \.\.\.draft, keywords: value \}\)\}/);
  assert.match(editModalSource, /function singleRuleTerm/);
  assert.match(editModalSource, /inputLabel="添加包含词"/);
  assert.match(editModalSource, /inputLabel="添加车牌前缀"/);
  assert.match(editModalSource, /const showDuplicateWarning = pendingTerm !== "" && pendingExists;/);
  assert.match(editModalSource, /aria-describedby=\{showDuplicateWarning \? warningId : undefined\}/);
  assert.match(editModalSource, /当前包含词已存在/);
  assert.match(editModalSource, /当前车牌前缀已存在/);
  assert.match(editModalSource, /<div className="admin-form__row">\s*<KeywordPillEditor/);
  assert.match(editModalSource, /<div className="admin-form__row">\s*<PrefixPillEditor/);
  assert.doesNotMatch(editModalSource, /<label htmlFor="admin-tag-rule-keywords">包含词<\/label>/);
  assert.doesNotMatch(editModalSource, /番号前缀|<textarea/);
  assert.match(editModalSource, /className="admin-tag-rule-keyword-list"/);
  assert.match(editModalSource, /className="admin-tag-rule-keyword-pill"/);
  assert.match(editModalSource, /className="admin-tag-rule-keyword-input-row"/);
  assert.doesNotMatch(editModalSource, /admin-tag-rule-words|整词匹配/);
  assert.doesNotMatch(editModalSource, /admin-tag-rule-excludes|排除词/);
  assert.match(editModalSource, /inputId="admin-tag-rule-prefixes"/);
  assert.match(editModalSource, /onCommit=\{\(value\) => void persistDraft\(\{ \.\.\.draft, avCodePrefixes: value \}\)\}/);
  assert.match(editModalSource, /splitTerms=\{splitPrefixTerms\}/);
  assert.match(editModalSource, /allowEmpty/);
  assert.match(editModalSource, /await api\.updateTag\(tag\.id, parsedRules\);/);
  assert.doesNotMatch(editModalSource, /disabled=\{saving \|\| !canSave\}/);
  assert.doesNotMatch(editModalSource, /aliasDraft|aliases|editTagAliases|pendingAliasAdditions|duplicateAliasInputs/);
  assert.doesNotMatch(adminCss, /admin-tag-alias/);
  assert.match(adminCss, /\.admin-tag-rule-keyword-list\s*\{[^}]*padding\s*:\s*2px 0/s);
  assert.match(adminCss, /\.admin-tag-rule-keyword-list\s*\{[^}]*transform\s*:\s*translateY\(-6px\)/s);
  assert.match(adminCss, /\.admin-tag-rule-keyword-pill\s*\{[^}]*background\s*:\s*transparent/s);
  assert.match(adminCss, /\.admin-tag-rule-keyword-warning\s*\{[^}]*color\s*:\s*var\(--danger\)/s);
  assert.doesNotMatch(adminCss, /\.admin-modal--tag-dialog \.admin-modal__header::after/);
  assert.match(adminCss, /\.admin-modal--tag-dialog \.admin-modal__body\s*\{[^}]*padding\s*:\s*14px 20px 24px/s);
  assert.doesNotMatch(adminCss, /\.admin-tag-rule-keyword-input-row input\s*\{/);
  assert.match(adminCss, /\.admin-tag-rule-form textarea\s*\{[^}]*min-height\s*:\s*72px/s);
  assert.match(tagsPageSource, /function tagRuleDraft\(tag: api\.AdminTag\): RuleDraft/);
  assert.match(tagsPageSource, /function matchRulesFromDraft\(draft: RuleDraft, isAV: boolean\): api\.TagMatchRules/);
  assert.match(tagsPageSource, /function splitRuleTerms/);
  assert.match(tagsPageSource, /function splitPrefixTerms/);
});

test("admin videos render tag assignment source and evidence", () => {
  assert.match(apiSource, /tagSources\?: Record<string, string>/);
  assert.match(apiSource, /tagEvidence\?: Record<string, string>/);
  assert.match(videosPageSource, /data-source=\{v\.tagSources\?\.\[t\]/);
  assert.match(videosPageSource, /tagAssignmentSourceLabel/);
  assert.match(videosPageSource, /tagAssignmentTitle/);
  assert.match(videosPageSource, /video\.tagEvidence\?\.\[label\]/);
});

test("tag management has no built-in source category", () => {
 assert.doesNotMatch(tagsPageSource, /builtin|内置/);
 assert.doesNotMatch(adminCss, /data-source="builtin"/);
 assert.doesNotMatch(tokensCss, /tag-source-builtin-/);
});
