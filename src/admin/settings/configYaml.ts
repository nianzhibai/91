import {
 TELEGRAM_DEFAULTS, TELEGRAM_FIELDS, TELEGRAM_YAML_KEYS,
 telegramDraftFromDocument, telegramFieldValue, type TelegramDraft,
} from "./telegramYaml";
import {
  Scalar,
  isMap,
  isScalar,
  parseDocument,
  stringify,
  type Pair,
  type ParsedNode,
  type Range,
  type YAMLMap,
} from "yaml";

export type SettingsDraft = TelegramDraft & {
  previewEnabled: boolean;
  nightlyDisabled: boolean;
  nightlyStartTime: string;
  nightlyTimezone: string;
  previewConcurrency: number;
  thumbnailConcurrency: number;
  fingerprintConcurrency: number;
};

export type VisualField = keyof SettingsDraft;

export const DEFAULT_DRAFT: SettingsDraft = {
  ...TELEGRAM_DEFAULTS,
  previewEnabled: true,
  nightlyDisabled: false,
  nightlyStartTime: "01:00",
  nightlyTimezone: "Asia/Shanghai",
  previewConcurrency: 1,
  thumbnailConcurrency: 1,
  fingerprintConcurrency: 1,
};

export const MIN_GENERATION_CONCURRENCY = 1;
export const MAX_GENERATION_CONCURRENCY = 5;

type ParsedMap = YAMLMap<ParsedNode, ParsedNode | null>;
type ParsedPair = Pair<ParsedNode, ParsedNode | null>;

type SourceEdit = {
  start: number;
  end: number;
  text: string;
};

type RangedNode = {
  range?: Range | null;
};

export function isValidStartTime(value: string) {
  if (!/^\d{2}:\d{2}$/.test(value)) return false;
  const [hour, minute] = value.split(":").map(Number);
  return hour >= 0 && hour <= 23 && minute >= 0 && minute <= 59;
}

export function isValidTimezone(value: string) {
  if (value === "Local" || value === "" || value !== value.trim()) return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: value }).format(0);
    return true;
  } catch {
    return false;
  }
}

export function configDocument(source: string) {
  const document = parseDocument(source, {
    keepSourceTokens: true,
    prettyErrors: true,
  });
  if (document.errors.length > 0) {
    throw new Error(document.errors[0].message);
  }
  const root = document.toJS();
  if (root !== null && (typeof root !== "object" || Array.isArray(root))) {
    throw new Error("config.yaml 顶层必须是映射对象");
  }
  return document;
}

function draftFromDocument(document: ReturnType<typeof configDocument>): SettingsDraft {
  const nightlyNode = document.get("nightly", true);
  if (
    nightlyNode !== undefined &&
    nightlyNode !== null &&
    !isMap(nightlyNode) &&
    !(isScalar(nightlyNode) && nightlyNode.value === null)
  ) {
    throw new Error("nightly 必须是映射对象");
  }
  const configuredStart = document.getIn(["nightly", "start_time"]);
  let nightlyStartTime = DEFAULT_DRAFT.nightlyStartTime;
  if (configuredStart !== undefined && configuredStart !== null) {
    if (typeof configuredStart !== "string" || !isValidStartTime(configuredStart)) {
      throw new Error("nightly.start_time 必须是 HH:mm 格式的有效时间");
    }
    nightlyStartTime = configuredStart;
  } else {
    const legacyHour = document.getIn(["nightly", "cron_hour"]);
    if (typeof legacyHour === "number" && legacyHour >= 1 && legacyHour <= 23) {
      nightlyStartTime = `${String(legacyHour).padStart(2, "0")}:00`;
    }
  }

  const configuredTimezone = document.getIn(["nightly", "timezone"]);
  let nightlyTimezone = DEFAULT_DRAFT.nightlyTimezone;
  if (configuredTimezone !== undefined && configuredTimezone !== null) {
    if (typeof configuredTimezone !== "string" || !isValidTimezone(configuredTimezone)) {
      throw new Error("nightly.timezone 必须是有效的 IANA 时区名");
    }
    nightlyTimezone = configuredTimezone;
  }

  const configuredDisabled = document.getIn(["nightly", "disabled"]);
  let nightlyDisabled = DEFAULT_DRAFT.nightlyDisabled;
  if (configuredDisabled !== undefined && configuredDisabled !== null) {
    if (typeof configuredDisabled !== "boolean") {
      throw new Error("nightly.disabled 必须是布尔值");
    }
    nightlyDisabled = configuredDisabled;
  }

  const previewNode = document.get("preview", true);
  if (
    previewNode !== undefined &&
    previewNode !== null &&
    !isMap(previewNode) &&
    !(isScalar(previewNode) && previewNode.value === null)
  ) {
    throw new Error("preview 必须是映射对象");
  }
  const configuredPreviewEnabled = document.getIn(["preview", "enabled"]);
  let previewEnabled = DEFAULT_DRAFT.previewEnabled;
  if (configuredPreviewEnabled !== undefined && configuredPreviewEnabled !== null) {
    if (typeof configuredPreviewEnabled !== "boolean") {
      throw new Error("preview.enabled 必须是布尔值");
    }
    previewEnabled = configuredPreviewEnabled;
  }
  const generationNode = document.get("generation", true);
  if (
    generationNode !== undefined &&
    generationNode !== null &&
    !isMap(generationNode) &&
    !(isScalar(generationNode) && generationNode.value === null)
  ) {
    throw new Error("generation 必须是映射对象");
  }
  function generationConcurrency(key: string, fallback: number) {
    const value = document.getIn(["generation", key]);
    if (value === undefined || value === null) return fallback;
    if (
      typeof value !== "number" ||
      !Number.isInteger(value) ||
      value < MIN_GENERATION_CONCURRENCY ||
      value > MAX_GENERATION_CONCURRENCY
    ) {
      throw new Error(`generation.${key} 必须是 ${MIN_GENERATION_CONCURRENCY}-${MAX_GENERATION_CONCURRENCY} 之间的整数`);
    }
    return value;
  }
  const thumbnailConcurrency = generationConcurrency(
    "thumbnail_concurrency", DEFAULT_DRAFT.thumbnailConcurrency
  );
  const previewConcurrency = generationConcurrency(
    "preview_concurrency", DEFAULT_DRAFT.previewConcurrency
  );
  const fingerprintConcurrency = generationConcurrency(
    "fingerprint_concurrency", DEFAULT_DRAFT.fingerprintConcurrency
  );

  return {
    ...telegramDraftFromDocument(document),
    previewEnabled,
    nightlyDisabled,
    nightlyStartTime,
    nightlyTimezone,
    previewConcurrency,
    thumbnailConcurrency,
    fingerprintConcurrency,
  };
}

export function parseConfig(source: string) {
  const document = configDocument(source);
  return { document, draft: draftFromDocument(document) };
}

function requiredRange(node: RangedNode, description: string) {
  if (!node.range) {
    throw new Error(`无法定位 ${description} 在 config.yaml 中的位置`);
  }
  return node.range;
}

function findPair(map: ParsedMap, key: string): ParsedPair | undefined {
  return map.items.find(
    (pair) => isScalar(pair.key) && pair.key.value === key
  );
}

function yamlString(value: string, template?: ParsedNode | null) {
  if (isScalar(template)) {
    if (template.type === Scalar.QUOTE_DOUBLE) return JSON.stringify(value);
    if (template.type === Scalar.QUOTE_SINGLE) {
      return `'${value.replace(/'/g, "''")}'`;
    }
  }

  const serialized = stringify(value, { lineWidth: 0 });
  return serialized.endsWith("\n") ? serialized.slice(0, -1) : serialized;
}

function lineStart(source: string, offset: number) {
  let cursor = offset;
  while (cursor > 0 && source[cursor - 1] !== "\n" && source[cursor - 1] !== "\r") {
    cursor -= 1;
  }
  return cursor;
}

function lineEndIncludingBreak(source: string, offset: number) {
  let cursor = offset;
  while (cursor < source.length && source[cursor] !== "\n" && source[cursor] !== "\r") {
    cursor += 1;
  }
  if (source[cursor] === "\r" && source[cursor + 1] === "\n") return cursor + 2;
  if (source[cursor] === "\r" || source[cursor] === "\n") return cursor + 1;
  return cursor;
}

function lineEnding(source: string) {
  const match = /\r\n|\n|\r/.exec(source);
  return match?.[0] ?? "\n";
}

function insertLinesAtBoundary(
  source: string,
  position: number,
  lines: readonly string[]
): SourceEdit {
  const eol = lineEnding(source);
  const previousIsBreak =
    position === 0 || source[position - 1] === "\n" || source[position - 1] === "\r";
  const nextIsBreak = source[position] === "\n" || source[position] === "\r";
  const prefix = previousIsBreak ? "" : eol;
  const suffix =
    (position < source.length && !nextIsBreak) ||
    (position === source.length && position > 0 && previousIsBreak)
      ? eol
      : "";

  return {
    start: position,
    end: position,
    text: `${prefix}${lines.join(eol)}${suffix}`,
  };
}

function replaceStringPairValue(
  source: string,
  pair: ParsedPair,
  value: string,
  path: string
): SourceEdit {
  const node = pair.value;
  if (node && !isScalar(node)) {
    throw new Error(`${path} 必须是字符串值`);
  }

  if (node?.range && node.range[0] < node.range[1]) {
    return {
      start: node.range[0],
      end: node.range[1],
      text: yamlString(value, node),
    };
  }

  if (!isScalar(pair.key)) {
    throw new Error(`无法定位 ${path} 的键名`);
  }
  const keyRange = requiredRange(pair.key, path);
  const endOfLine = lineEndIncludingBreak(source, keyRange[1]);
  const colon = source.indexOf(":", keyRange[1]);
  if (colon === -1 || colon >= endOfLine) {
    throw new Error(`无法定位 ${path} 的值`);
  }

  let whitespaceEnd = colon + 1;
  while (source[whitespaceEnd] === " " || source[whitespaceEnd] === "\t") {
    whitespaceEnd += 1;
  }
  const commentGap = source[whitespaceEnd] === "#" ? " " : "";
  return {
    start: colon + 1,
    end: whitespaceEnd,
    text: ` ${yamlString(value, node)}${commentGap}`,
  };
}

function replacePairKey(pair: ParsedPair, key: string): SourceEdit {
  if (!isScalar(pair.key)) {
    throw new Error("无法定位 nightly.cron_hour 的键名");
  }
  const range = requiredRange(pair.key, "nightly.cron_hour");
  return {
    start: range[0],
    end: range[1],
    text: yamlString(key, pair.key),
  };
}

function isFlowMap(map: ParsedMap) {
  return map.srcToken?.type === "flow-collection";
}

function pairContentEnd(pair: ParsedPair) {
  const node = pair.value ?? pair.key;
  return requiredRange(node, "YAML 配置项")[1];
}

function removeMapPair(source: string, map: ParsedMap, pair: ParsedPair): SourceEdit {
  const index = map.items.indexOf(pair);
  if (index === -1) throw new Error("无法定位要移除的 YAML 配置项");

  if (isFlowMap(map)) {
    const currentStart = requiredRange(pair.key, "YAML 配置项")[0];
    const currentEnd = pairContentEnd(pair);
    if (index > 0) {
      return {
        start: pairContentEnd(map.items[index - 1]),
        end: currentEnd,
        text: "",
      };
    }
    if (map.items[index + 1]) {
      return {
        start: currentStart,
        end: requiredRange(map.items[index + 1].key, "YAML 配置项")[0],
        text: "",
      };
    }
    return { start: currentStart, end: currentEnd, text: "" };
  }

  const keyRange = requiredRange(pair.key, "YAML 配置项");
  return {
    start: lineStart(source, keyRange[0]),
    end: lineEndIncludingBreak(source, pairContentEnd(pair)),
    text: "",
  };
}

function insertFlowMapEntry(source: string, map: ParsedMap, entry: string): SourceEdit {
  const range = requiredRange(map, "YAML 映射");
  const openBrace = source.indexOf("{", range[0]);
  const closeBrace = source.lastIndexOf("}", range[1] - 1);
  if (openBrace === -1 || closeBrace <= openBrace) {
    throw new Error("无法定位 YAML 行内映射的边界");
  }

  if (map.items.length === 0) {
    const existingWhitespace = source.slice(openBrace + 1, closeBrace);
    return {
      start: openBrace + 1,
      end: closeBrace,
      text: existingWhitespace.length > 0 ? ` ${entry} ` : entry,
    };
  }

  const lastPair = map.items[map.items.length - 1];
  const position = pairContentEnd(lastPair);
  return { start: position, end: position, text: `, ${entry}` };
}

function insertBlockMapEntry(
  source: string,
  map: ParsedMap,
  entry: string,
  description = "YAML 映射"
): SourceEdit {
  const firstPair = map.items[0];
  if (!firstPair || !isScalar(firstPair.key)) {
    throw new Error(`无法确定 ${description} 配置项的缩进`);
  }
  const keyRange = requiredRange(firstPair.key, `${description} 配置项`);
  const indent = source.slice(lineStart(source, keyRange[0]), keyRange[0]);
  const mapRange = requiredRange(map, description);
  return insertLinesAtBoundary(source, mapRange[2], [`${indent}${entry}`]);
}

function insertAfterEmptyMapKey(
  source: string,
  pair: ParsedPair,
  entry: string,
  description = "YAML 映射"
): SourceEdit {
  const keyRange = requiredRange(pair.key, description);
  const parentIndent = source.slice(lineStart(source, keyRange[0]), keyRange[0]);
  const lineEnd = lineEndIncludingBreak(source, keyRange[1]);
  return insertLinesAtBoundary(source, lineEnd, [`${parentIndent}  ${entry}`]);
}

function addNightlyStringSection(
  source: string,
  document: ReturnType<typeof configDocument>,
  root: ParsedMap | null,
  key: "start_time" | "timezone",
  value: string
): SourceEdit {
  const renderedValue = yamlString(value);
  if (root && isFlowMap(root)) {
    return insertFlowMapEntry(
      source,
      root,
      `nightly: { ${key}: ${renderedValue} }`
    );
  }

  const position = root?.range?.[2] ?? document.range?.[1] ?? source.length;
  return insertLinesAtBoundary(source, position, [
    "nightly:",
    `  ${key}: ${renderedValue}`,
  ]);
}

function nightlyStartTimeEdits(
  source: string,
  document: ReturnType<typeof configDocument>,
  value: string
): SourceEdit[] {
  const root = isMap(document.contents) ? (document.contents as ParsedMap) : null;
  const nightlyPair = root ? findPair(root, "nightly") : undefined;
  if (!nightlyPair) {
    return [addNightlyStringSection(source, document, root, "start_time", value)];
  }

  const nightlyNode = nightlyPair.value;
  if (
    !nightlyNode ||
    (isScalar(nightlyNode) &&
      nightlyNode.value === null &&
      (!nightlyNode.range || nightlyNode.range[0] === nightlyNode.range[1]))
  ) {
    return [
      insertAfterEmptyMapKey(
        source,
        nightlyPair,
        `start_time: ${yamlString(value)}`
      ),
    ];
  }
  if (isScalar(nightlyNode) && nightlyNode.value === null) {
    const range = requiredRange(nightlyNode, "nightly");
    return [
      {
        start: range[0],
        end: range[1],
        text: `{ start_time: ${yamlString(value)} }`,
      },
    ];
  }
  if (!isMap(nightlyNode)) {
    throw new Error("nightly 必须是映射对象");
  }

  const nightly = nightlyNode as ParsedMap;
  const startTimePair = findPair(nightly, "start_time");
  const legacyHourPair = findPair(nightly, "cron_hour");
  if (startTimePair) {
    const edits = [
      replaceStringPairValue(source, startTimePair, value, "nightly.start_time"),
    ];
    if (legacyHourPair) edits.push(removeMapPair(source, nightly, legacyHourPair));
    return edits;
  }
  if (legacyHourPair) {
    return [
      replacePairKey(legacyHourPair, "start_time"),
      replaceStringPairValue(source, legacyHourPair, value, "nightly.start_time"),
    ];
  }

  const entry = `start_time: ${yamlString(value)}`;
  return [
    isFlowMap(nightly)
      ? insertFlowMapEntry(source, nightly, entry)
      : insertBlockMapEntry(source, nightly, entry),
  ];
}

function nightlyTimezoneEdits(
  source: string,
  document: ReturnType<typeof configDocument>,
  value: string
): SourceEdit[] {
  const root = isMap(document.contents) ? (document.contents as ParsedMap) : null;
  const nightlyPair = root ? findPair(root, "nightly") : undefined;
  if (!nightlyPair) {
    return [addNightlyStringSection(source, document, root, "timezone", value)];
  }

  const nightlyNode = nightlyPair.value;
  if (
    !nightlyNode ||
    (isScalar(nightlyNode) &&
      nightlyNode.value === null &&
      (!nightlyNode.range || nightlyNode.range[0] === nightlyNode.range[1]))
  ) {
    return [
      insertAfterEmptyMapKey(source, nightlyPair, `timezone: ${yamlString(value)}`),
    ];
  }
  if (isScalar(nightlyNode) && nightlyNode.value === null) {
    const range = requiredRange(nightlyNode, "nightly");
    return [
      {
        start: range[0],
        end: range[1],
        text: `{ timezone: ${yamlString(value)} }`,
      },
    ];
  }
  if (!isMap(nightlyNode)) {
    throw new Error("nightly 必须是映射对象");
  }

  const nightly = nightlyNode as ParsedMap;
  const timezonePair = findPair(nightly, "timezone");
  if (timezonePair) {
    return [
      replaceStringPairValue(source, timezonePair, value, "nightly.timezone"),
    ];
  }

  const entry = `timezone: ${yamlString(value)}`;
  return [
    isFlowMap(nightly)
      ? insertFlowMapEntry(source, nightly, entry)
      : insertBlockMapEntry(source, nightly, entry),
  ];
}

function replaceBooleanPairValue(
  source: string,
  pair: ParsedPair,
  value: boolean,
  path: string
): SourceEdit {
  const node = pair.value;
  if (node && !isScalar(node)) {
    throw new Error(`${path} 必须是布尔值`);
  }
  const rendered = value ? "true" : "false";

  if (node?.range && node.range[0] < node.range[1]) {
    return {
      start: node.range[0],
      end: node.range[1],
      text: rendered,
    };
  }

  if (!isScalar(pair.key)) {
    throw new Error(`无法定位 ${path} 的键名`);
  }
  const keyRange = requiredRange(pair.key, path);
  const endOfLine = lineEndIncludingBreak(source, keyRange[1]);
  const colon = source.indexOf(":", keyRange[1]);
  if (colon === -1 || colon >= endOfLine) {
    throw new Error(`无法定位 ${path} 的值`);
  }

  let whitespaceEnd = colon + 1;
  while (source[whitespaceEnd] === " " || source[whitespaceEnd] === "\t") {
    whitespaceEnd += 1;
  }
  const commentGap = source[whitespaceEnd] === "#" ? " " : "";
  return {
    start: colon + 1,
    end: whitespaceEnd,
    text: ` ${rendered}${commentGap}`,
  };
}

type BooleanField = {
  section: string;
  key: string;
  path: string;
};

function addBooleanSection(
  source: string,
  document: ReturnType<typeof configDocument>,
  root: ParsedMap | null,
  field: BooleanField,
  value: boolean
): SourceEdit {
  const rendered = value ? "true" : "false";
  if (root && isFlowMap(root)) {
    return insertFlowMapEntry(
      source,
      root,
      `${field.section}: { ${field.key}: ${rendered} }`
    );
  }

  const position = root?.range?.[2] ?? document.range?.[1] ?? source.length;
  return insertLinesAtBoundary(source, position, [
    `${field.section}:`,
    `  ${field.key}: ${rendered}`,
  ]);
}

function booleanFieldEdits(
  source: string,
  document: ReturnType<typeof configDocument>,
  field: BooleanField,
  value: boolean
): SourceEdit[] {
  const root = isMap(document.contents) ? (document.contents as ParsedMap) : null;
  const sectionPair = root ? findPair(root, field.section) : undefined;
  if (!sectionPair) {
    return [addBooleanSection(source, document, root, field, value)];
  }

  const rendered = value ? "true" : "false";
  const sectionNode = sectionPair.value;
  if (
    !sectionNode ||
    (isScalar(sectionNode) &&
      sectionNode.value === null &&
      (!sectionNode.range || sectionNode.range[0] === sectionNode.range[1]))
  ) {
    return [
      insertAfterEmptyMapKey(
        source,
        sectionPair,
        `${field.key}: ${rendered}`,
        field.section
      ),
    ];
  }
  if (isScalar(sectionNode) && sectionNode.value === null) {
    const range = requiredRange(sectionNode, field.section);
    return [
      {
        start: range[0],
        end: range[1],
        text: `{ ${field.key}: ${rendered} }`,
      },
    ];
  }
  if (!isMap(sectionNode)) {
    throw new Error(`${field.section} 必须是映射对象`);
  }

  const section = sectionNode as ParsedMap;
  const fieldPair = findPair(section, field.key);
  if (fieldPair) {
    return [replaceBooleanPairValue(source, fieldPair, value, field.path)];
  }

  const entry = `${field.key}: ${rendered}`;
  return [
    isFlowMap(section)
      ? insertFlowMapEntry(source, section, entry)
      : insertBlockMapEntry(source, section, entry, field.section),
  ];
}

function nightlyDisabledEdits(
  source: string,
  document: ReturnType<typeof configDocument>,
  value: boolean
) {
  return booleanFieldEdits(
    source,
    document,
    { section: "nightly", key: "disabled", path: "nightly.disabled" },
    value
  );
}

function replaceIntegerPairValue(
  source: string,
  pair: ParsedPair,
  value: number,
  path: string
): SourceEdit {
  const node = pair.value;
  if (node && !isScalar(node)) {
    throw new Error(`${path} 必须是整数`);
  }
  if (node && node.value !== null && typeof node.value !== "number") {
    throw new Error(`${path} 必须是整数`);
  }
  const rendered = String(value);

  if (node?.range && node.range[0] < node.range[1]) {
    return {
      start: node.range[0],
      end: node.range[1],
      text: rendered,
    };
  }

  if (!isScalar(pair.key)) {
    throw new Error(`无法定位 ${path} 的键名`);
  }
  const keyRange = requiredRange(pair.key, path);
  const endOfLine = lineEndIncludingBreak(source, keyRange[1]);
  const colon = source.indexOf(":", keyRange[1]);
  if (colon === -1 || colon >= endOfLine) {
    throw new Error(`无法定位 ${path} 的值`);
  }

  let whitespaceEnd = colon + 1;
  while (source[whitespaceEnd] === " " || source[whitespaceEnd] === "\t") {
    whitespaceEnd += 1;
  }
  const commentGap = source[whitespaceEnd] === "#" ? " " : "";
  return {
    start: colon + 1,
    end: whitespaceEnd,
    text: ` ${rendered}${commentGap}`,
  };
}

function integerSettingEdits(
  source: string,
  document: ReturnType<typeof configDocument>,
  section: string,
  key: string,
  value: number
): SourceEdit[] {
  const root = isMap(document.contents) ? (document.contents as ParsedMap) : null;
  const sectionPair = root ? findPair(root, section) : undefined;
  const entry = `${key}: ${value}`;
  if (!sectionPair) {
    if (root && isFlowMap(root)) {
      return [insertFlowMapEntry(source, root, `${section}: { ${entry} }`)];
    }
    const position = root?.range?.[2] ?? document.range?.[1] ?? source.length;
    return [insertLinesAtBoundary(source, position, [`${section}:`, `  ${entry}`])];
  }

  const sectionNode = sectionPair.value;
  if (
    !sectionNode ||
    (isScalar(sectionNode) &&
      sectionNode.value === null &&
      (!sectionNode.range || sectionNode.range[0] === sectionNode.range[1]))
  ) {
    return [insertAfterEmptyMapKey(source, sectionPair, entry, section)];
  }
  if (isScalar(sectionNode) && sectionNode.value === null) {
    const range = requiredRange(sectionNode, section);
    return [{ start: range[0], end: range[1], text: `{ ${entry} }` }];
  }
  if (!isMap(sectionNode)) {
    throw new Error(`${section} 必须是映射对象`);
  }

  const sectionMap = sectionNode as ParsedMap;
  const valuePair = findPair(sectionMap, key);
  if (valuePair) {
    return [
      replaceIntegerPairValue(
        source,
        valuePair,
        value,
        `${section}.${key}`
      ),
    ];
  }
  return [
    isFlowMap(sectionMap)
      ? insertFlowMapEntry(source, sectionMap, entry)
      : insertBlockMapEntry(source, sectionMap, entry, section),
  ];
}

// Edit only the selected value so comments and unrelated YAML remain intact.
function telegramSettingEdits(
 source: string, document: ReturnType<typeof configDocument>, key: string,
 value: string | number | boolean | number[]
): SourceEdit[] {
 const root = isMap(document.contents) ? document.contents as ParsedMap : null;
 const sectionPair = root ? findPair(root, "telegram") : undefined;
 const rendered = JSON.stringify(value);
 const entry = `${key}: ${rendered}`;
 if (!sectionPair) {
  if (root && isFlowMap(root)) return [insertFlowMapEntry(source, root, `telegram: { ${entry} }`)];
  const position = root?.range?.[2] ?? document.range?.[1] ?? source.length;
  return [insertLinesAtBoundary(source, position, ["telegram:", `  ${entry}`])];
 }
 const node = sectionPair.value;
 if (!node || (isScalar(node) && node.value === null && (!node.range || node.range[0] === node.range[1]))) {
  return [insertAfterEmptyMapKey(source, sectionPair, entry, "telegram")];
 }
 if (isScalar(node) && node.value === null) {
  const range = requiredRange(node, "telegram");
  return [{start: range[0], end: range[1], text: `{ ${entry} }`}];
 }
 if (!isMap(node)) throw new Error("telegram 必须是映射对象");
 const section = node as ParsedMap;
 const pair = findPair(section, key);
 if (!pair) return [isFlowMap(section) ? insertFlowMapEntry(source, section, entry) : insertBlockMapEntry(source, section, entry, "telegram")];
 if (typeof value === "string") return [replaceStringPairValue(source, pair, value, `telegram.${key}`)];
 if (typeof value === "boolean") return [replaceBooleanPairValue(source, pair, value, `telegram.${key}`)];
 if (typeof value === "number") return [replaceIntegerPairValue(source, pair, value, `telegram.${key}`)];
 if (pair.value?.range && pair.value.range[0] < pair.value.range[1]) {
  const range = pair.value.range;
  // Block sequences include their final newline in the value range.
  const suffix = source.slice(range[0], range[1]).endsWith("\n") ? lineEnding(source) : "";
  return [{start: range[0], end: range[1], text: rendered + suffix}];
 }
 const keyRange = requiredRange(pair.key, `telegram.${key}`);
 const colon = source.indexOf(":", keyRange[1]);
 if (colon < 0) throw new Error(`无法定位 telegram.${key}`);
 return [{start: colon + 1, end: colon + 1, text: ` ${rendered}`}];
}

function applySourceEdits(source: string, edits: readonly SourceEdit[]) {
  const ordered = [...edits].sort((left, right) => right.start - left.start);
  let boundary = source.length;
  let result = source;

  for (const edit of ordered) {
    if (
      edit.start < 0 ||
      edit.end < edit.start ||
      edit.end > source.length ||
      edit.end > boundary
    ) {
      throw new Error("YAML 局部修改范围无效或相互重叠");
    }
    result = `${result.slice(0, edit.start)}${edit.text}${result.slice(edit.end)}`;
    boundary = edit.start;
  }
  return result;
}

export function applyVisualFields(
  source: string,
  draft: SettingsDraft,
  fields: ReadonlySet<VisualField>
) {
  let updated = source;
  if (fields.has("previewEnabled")) {
    const document = configDocument(updated);
    updated = applySourceEdits(
      updated,
      booleanFieldEdits(
        updated,
        document,
        { section: "preview", key: "enabled", path: "preview.enabled" },
        draft.previewEnabled
      )
    );
  }
  if (fields.has("nightlyDisabled")) {
    const document = configDocument(updated);
    updated = applySourceEdits(
      updated,
      nightlyDisabledEdits(updated, document, draft.nightlyDisabled)
    );
  }
  if (fields.has("nightlyStartTime")) {
    const document = configDocument(updated);
    updated = applySourceEdits(
      updated,
      nightlyStartTimeEdits(updated, document, draft.nightlyStartTime)
    );
  }
  if (fields.has("nightlyTimezone")) {
    const document = configDocument(updated);
    updated = applySourceEdits(
      updated,
      nightlyTimezoneEdits(updated, document, draft.nightlyTimezone)
    );
  }
  for (const [field, key] of [
    ["thumbnailConcurrency", "thumbnail_concurrency"],
    ["previewConcurrency", "preview_concurrency"],
    ["fingerprintConcurrency", "fingerprint_concurrency"],
  ] as const) {
    if (fields.has(field)) {
      const document = configDocument(updated);
      updated = applySourceEdits(
        updated,
        integerSettingEdits(updated, document, "generation", key, draft[field])
      );
    }
  }
  for (const field of TELEGRAM_FIELDS) {
    if (!fields.has(field)) continue;
    updated = applySourceEdits(updated, telegramSettingEdits(
      updated, configDocument(updated), TELEGRAM_YAML_KEYS[field], telegramFieldValue(draft, field)
    ));
  }

  // Source ranges perform the write, while a fresh parse verifies that the
  // localized edit still produced one valid YAML document.
  configDocument(updated);
  return updated;
}

export function changedVisualFields(saved: SettingsDraft, draft: SettingsDraft) {
  const fields = new Set<VisualField>();
  if (saved.previewEnabled !== draft.previewEnabled) {
    fields.add("previewEnabled");
  }
  if (saved.nightlyDisabled !== draft.nightlyDisabled) {
    fields.add("nightlyDisabled");
  }
  if (saved.nightlyStartTime !== draft.nightlyStartTime) {
    fields.add("nightlyStartTime");
  }
  if (saved.nightlyTimezone !== draft.nightlyTimezone) {
    fields.add("nightlyTimezone");
  }
  if (saved.previewConcurrency !== draft.previewConcurrency) {
    fields.add("previewConcurrency");
  }
  if (saved.thumbnailConcurrency !== draft.thumbnailConcurrency) {
    fields.add("thumbnailConcurrency");
  }
  if (saved.fingerprintConcurrency !== draft.fingerprintConcurrency) {
    fields.add("fingerprintConcurrency");
  }
  for (const field of TELEGRAM_FIELDS) {
    if (saved[field] !== draft[field]) fields.add(field);
  }
  return fields;
}
