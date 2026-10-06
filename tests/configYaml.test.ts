import assert from "node:assert/strict";
import test from "node:test";

import {
  DEFAULT_DRAFT,
  applyVisualFields,
  changedVisualFields,
  configDocument,
  parseConfig,
  type SettingsDraft,
  type VisualField,
} from "../src/admin/settings/configYaml";
import { buildConfigDiff } from "../src/admin/settings/configDiff";

const nightlyDisabled = new Set<VisualField>(["nightlyDisabled"]);
const nightlyStartTime = new Set<VisualField>(["nightlyStartTime"]);
const nightlyTimezone = new Set<VisualField>(["nightlyTimezone"]);
const previewConcurrency = new Set<VisualField>(["previewConcurrency"]);

test("global preview switch defaults on and rejects non-boolean YAML", () => {
  assert.equal(parseConfig("").draft.previewEnabled, true);
  assert.equal(parseConfig("preview: { enabled: false }").draft.previewEnabled, false);
  assert.throws(() => parseConfig('preview: { enabled: "false" }'), /preview.enabled/);
});

test("global preview switch round-trips through visual edits without changing other fields", () => {
  for (const source of [
    "# keep\npreview:\n  enabled: true # hot reload\n  width: 480\n",
    "preview: { width: 480 }\n",
    "preview:\n",
    "preview: null\n",
    "generation: { preview_concurrency: 3 }\n",
  ]) {
    const before = parseConfig(source).draft;
    const draft = { ...before, previewEnabled: false };
    const fields = changedVisualFields(before, draft);
    assert.deepEqual([...fields], ["previewEnabled"]);
    const updated = applyVisualFields(source, draft, fields);
    assert.deepEqual(parseConfig(updated).draft, draft);
    assert.equal(
      configDocument(updated).getIn(["preview", "width"]),
      configDocument(source).getIn(["preview", "width"])
    );
    if (source.includes("# hot reload")) {
      assert.equal(updated, source.replace("enabled: true", "enabled: false"));
    }
  }
});

function settingsDraft(overrides: Partial<SettingsDraft> = {}): SettingsDraft {
  return { ...DEFAULT_DRAFT, ...overrides };
}

function updateNightlyDisabled(source: string, value = true) {
  return applyVisualFields(
    source,
    settingsDraft({ nightlyDisabled: value }),
    nightlyDisabled
  );
}

function updateStartTime(source: string, value = "02:00") {
  return applyVisualFields(
    source,
    settingsDraft({ nightlyStartTime: value }),
    nightlyStartTime
  );
}


function updateTimezone(source: string, value = "Asia/Shanghai") {
  return applyVisualFields(
    source,
    settingsDraft({ nightlyTimezone: value }),
    nightlyTimezone
  );
}

function updatePreviewConcurrency(source: string, value = 3) {
  return applyVisualFields(
    source,
    settingsDraft({ previewConcurrency: value }),
    previewConcurrency
  );
}

test("visual config updates only the start_time scalar in the original YAML", () => {
  const source = [
    "scanner:",
    '  video_extensions: [".mp4", ".mkv", ".mov", ".webm", ".avi"]',
    "nightly:",
    "  # keep the schedule comment",
    "  start_time: 01:00 # hot reload",
    "preview:",
    '  ffmpeg_path: "ffmpeg"',
    "",
  ].join("\n");

  const updated = updateStartTime(source);
  assert.equal(
    updated,
    source.replace("start_time: 01:00", "start_time: 02:00")
  );

  const diff = buildConfigDiff(source, updated);
  assert.equal(diff.additions, 1);
  assert.equal(diff.deletions, 1);
  assert.deepEqual(
    diff.hunks.flatMap((hunk) => hunk.lines)
      .filter((line) => line.kind !== "context")
      .map((line) => line.content),
    ["  start_time: 01:00 # hot reload", "  start_time: 02:00 # hot reload"]
  );
});

test("visual config preserves scalar quoting and CRLF line endings", () => {
  assert.equal(
    updateStartTime('nightly:\n  start_time: "01:00" # keep\n'),
    'nightly:\n  start_time: "02:00" # keep\n'
  );
  assert.equal(
    updateStartTime("nightly:\n  start_time: '01:00'\n"),
    "nightly:\n  start_time: '02:00'\n"
  );
  assert.equal(
    updateStartTime("nightly:\r\n  enabled: true\r\ntail: ok\r\n"),
    "nightly:\r\n  enabled: true\r\n  start_time: 02:00\r\ntail: ok\r\n"
  );
});

test("visual config migrates cron_hour in place without reserializing neighbors", () => {
  assert.equal(
    updateStartTime(
      "nightly:\n  # legacy schedule\n  cron_hour: 1 # keep\n  enabled: true\n"
    ),
    "nightly:\n  # legacy schedule\n  start_time: 02:00 # keep\n  enabled: true\n"
  );

  assert.equal(
    updateStartTime(
      "nightly:\n  start_time: 01:00\n  cron_hour: 1\n  enabled: true\n"
    ),
    "nightly:\n  start_time: 02:00\n  enabled: true\n"
  );
});

test("visual config inserts missing fields within block and flow mappings", () => {
  assert.equal(
    updateStartTime("nightly:\n  enabled: true\npreview: true\n"),
    "nightly:\n  enabled: true\n  start_time: 02:00\npreview: true\n"
  );
  assert.equal(
    updateStartTime("nightly: { enabled: true }\ntail: ok\n"),
    "nightly: { enabled: true, start_time: 02:00 }\ntail: ok\n"
  );
  assert.equal(
    updateStartTime("head: ok\n"),
    "head: ok\nnightly:\n  start_time: 02:00\n"
  );
});

test("visual config keeps YAML valid for an empty time input without touching other fields", () => {
  const source = 'video_extensions: [".mp4", ".mkv"]\nnightly:\n  start_time: 01:00\n';
  const updated = updateStartTime(source, "");

  assert.equal(
    updated,
    'video_extensions: [".mp4", ".mkv"]\nnightly:\n  start_time: ""\n'
  );
  assert.doesNotThrow(() => configDocument(updated));
});

test("visual config returns the exact source when no visual field is dirty", () => {
  const source = 'video_extensions: [".mp4", ".mkv"]\n';
  assert.equal(
    applyVisualFields(
      source,
      settingsDraft({ nightlyStartTime: "02:00" }),
      new Set()
    ),
    source
  );
});

test("visual config reads and validates the explicit nightly timezone", () => {
  assert.equal(parseConfig("{}\n").draft.nightlyTimezone, "Asia/Shanghai");
  assert.equal(
    parseConfig("nightly:\n  timezone: Asia/Shanghai\n").draft.nightlyTimezone,
    "Asia/Shanghai"
  );
  assert.throws(
    () => parseConfig("nightly:\n  timezone: Local\n"),
    /nightly\.timezone 必须是有效的 IANA 时区名/
  );
  assert.throws(
    () => parseConfig("nightly:\n  timezone: Mars\/Olympus\n"),
    /nightly\.timezone 必须是有效的 IANA 时区名/
  );
});

test("visual config edits only the timezone scalar and preserves YAML style", () => {
  assert.equal(
    updateTimezone('nightly:\n  timezone: "Etc/UTC" # keep\n'),
    'nightly:\n  timezone: "Asia/Shanghai" # keep\n'
  );
  assert.equal(
    updateTimezone("nightly: { start_time: 01:00 }\ntail: ok\n"),
    "nightly: { start_time: 01:00, timezone: Asia/Shanghai }\ntail: ok\n"
  );
  assert.equal(
    updateTimezone("head: ok\n"),
    "head: ok\nnightly:\n  timezone: Asia/Shanghai\n"
  );
});

test("visual config reads and validates the nightly stop switch", () => {
  assert.equal(parseConfig("{}\n").draft.nightlyDisabled, false);
  assert.equal(
    parseConfig("nightly:\n  disabled: true\n").draft.nightlyDisabled,
    true
  );
  assert.equal(
    parseConfig("nightly:\n  disabled: false\n").draft.nightlyDisabled,
    false
  );
  assert.throws(
    () => parseConfig('nightly:\n  disabled: "true"\n'),
    /nightly\.disabled 必须是布尔值/
  );
});

test("visual config updates only the nightly disabled boolean", () => {
  const source = [
    "nightly:",
    "  # keep the schedule comment",
    "  disabled: false # hot reload",
    "  start_time: 01:00",
    "future:",
    "  keep: yes",
    "",
  ].join("\n");
  assert.equal(
    updateNightlyDisabled(source),
    source.replace("disabled: false", "disabled: true")
  );
});

test("visual config inserts nightly disabled into block, flow, and empty mappings", () => {
  assert.equal(
    updateNightlyDisabled("nightly:\n  start_time: 01:00\ntail: ok\n"),
    "nightly:\n  start_time: 01:00\n  disabled: true\ntail: ok\n"
  );
  assert.equal(
    updateNightlyDisabled("nightly: { start_time: 01:00 }\ntail: ok\n"),
    "nightly: { start_time: 01:00, disabled: true }\ntail: ok\n"
  );
  assert.equal(
    updateNightlyDisabled("nightly:\ntail: ok\n"),
    "nightly:\n  disabled: true\ntail: ok\n"
  );
  assert.equal(
    updateNightlyDisabled("head: ok\n"),
    "head: ok\nnightly:\n  disabled: true\n"
  );
});

test("visual config reads and validates global preview concurrency", () => {
  assert.equal(parseConfig("{}\n").draft.previewConcurrency, 1);
  assert.equal(
    parseConfig("generation:\n  preview_concurrency: 3\n").draft.previewConcurrency,
    3
  );
  assert.equal(
    parseConfig("generation:\n  preview_concurrency: 5\n").draft.previewConcurrency,
    5
  );
  for (const invalid of [0, 6, 1.5]) {
    assert.throws(
      () => parseConfig(`generation:\n  preview_concurrency: ${invalid}\n`),
      /generation\.preview_concurrency 必须是 1-5 之间的整数/
    );
  }
  assert.throws(
    () => parseConfig('generation:\n  preview_concurrency: "3"\n'),
    /generation\.preview_concurrency 必须是 1-5 之间的整数/
  );
  assert.throws(() => parseConfig("generation: true\n"), /generation 必须是映射对象/);
});

test("visual config updates only preview concurrency and preserves YAML layout", () => {
  const source = [
    "generation:",
    "  # keep generation settings",
    "  preview_concurrency: 1 # hot reload",
    '  fingerprint_concurrency: 1',
    "future:",
    "  keep: yes",
    "",
  ].join("\n");
  assert.equal(
    updatePreviewConcurrency(source),
    source.replace("preview_concurrency: 1", "preview_concurrency: 3")
  );
  assert.equal(
    updatePreviewConcurrency("generation: { fingerprint_concurrency: 1 }\ntail: ok\n"),
    "generation: { fingerprint_concurrency: 1, preview_concurrency: 3 }\ntail: ok\n"
  );
  assert.equal(
    updatePreviewConcurrency("head: ok\n"),
    "head: ok\ngeneration:\n  preview_concurrency: 3\n"
  );
});

test("visual config applies multiple missing YAML fields without overlapping edits", () => {
  const fields = new Set<VisualField>([
    "nightlyDisabled",
    "nightlyStartTime",
    "nightlyTimezone",
    "previewConcurrency",
  ]);
  assert.equal(
    applyVisualFields(
      "head: ok\n",
      settingsDraft({
        nightlyDisabled: true,
        nightlyStartTime: "03:30",
        previewConcurrency: 3,
      }),
      fields
    ),
    "head: ok\nnightly:\n  disabled: true\n  start_time: 03:30\n  timezone: Asia/Shanghai\ngeneration:\n  preview_concurrency: 3\n"
  );
});

test("changed visual fields includes the nightly stop switch", () => {
  assert.deepEqual(
    changedVisualFields(
      settingsDraft(),
      settingsDraft({ nightlyDisabled: true })
    ),
    new Set<VisualField>(["nightlyDisabled"])
  );
});

test("changed visual fields includes the nightly timezone", () => {
  assert.deepEqual(
    changedVisualFields(
      settingsDraft({ nightlyTimezone: "Etc/UTC" }),
      settingsDraft()
    ),
    new Set<VisualField>(["nightlyTimezone"])
  );
});

test("changed visual fields includes preview concurrency", () => {
  assert.deepEqual(
    changedVisualFields(
      settingsDraft({ previewConcurrency: 1 }),
      settingsDraft({ previewConcurrency: 4 })
    ),
    new Set<VisualField>(["previewConcurrency"])
  );
});


test("global generation budgets default to one and validate all three limits", () => {
  const draft = parseConfig("{}\n").draft;
  assert.equal(draft.thumbnailConcurrency, 1);
  assert.equal(draft.previewConcurrency, 1);
  assert.equal(draft.fingerprintConcurrency, 1);
  for (const key of ["thumbnail_concurrency", "preview_concurrency", "fingerprint_concurrency"]) {
    for (const value of ["0", "-1", "6", "1.5", "true", '"2"']) {
      assert.throws(() => parseConfig(`generation: { ${key}: ${value} }\n`));
    }
  }
  assert.throws(() => parseConfig("generation: []\n"));
});

test("global budget edits preserve YAML styles and unrelated settings", () => {
  const fields = new Set<VisualField>(["thumbnailConcurrency", "previewConcurrency", "fingerprintConcurrency"]);
  const draft = settingsDraft({ thumbnailConcurrency: 2, previewConcurrency: 4, fingerprintConcurrency: 3 });
  const cases = [
    ["head: ok\n", "head: ok\ngeneration:\n  thumbnail_concurrency: 2\n  preview_concurrency: 4\n  fingerprint_concurrency: 3\n"],
    ["generation: { thumbnail_concurrency: 1, preview_concurrency: 1, fingerprint_concurrency: 1, future: yes } # keep\n", "generation: { thumbnail_concurrency: 2, preview_concurrency: 4, fingerprint_concurrency: 3, future: yes } # keep\n"],
    ["generation:\r\n  thumbnail_concurrency: 1 # cover\r\n  preview_concurrency: 1 # preview\r\n  fingerprint_concurrency: 1 # IO\r\n", "generation:\r\n  thumbnail_concurrency: 2 # cover\r\n  preview_concurrency: 4 # preview\r\n  fingerprint_concurrency: 3 # IO\r\n"],
  ];
  for (const [source, expected] of cases) {
    const result = applyVisualFields(source, draft, fields);
    assert.equal(result, expected);
    const parsed = parseConfig(result).draft;
    assert.equal(parsed.thumbnailConcurrency, 2);
    assert.equal(parsed.previewConcurrency, 4);
    assert.equal(parsed.fingerprintConcurrency, 3);
  }
  for (const source of ["generation: null\n", "generation: # keep\n", "{ generation: {} }\n"]) {
    const result = parseConfig(applyVisualFields(source, draft, fields)).draft;
    assert.equal(result.thumbnailConcurrency, 2);
    assert.equal(result.previewConcurrency, 4);
    assert.equal(result.fingerprintConcurrency, 3);
  }
  assert.deepEqual(changedVisualFields(settingsDraft(), draft), fields);
});

test("removed per-drive and combined settings do not control the independent budgets", () => {
  const source = "preview: { concurrency: 5 }\ngeneration: { media_concurrency: 4 }\n";
  const draft = parseConfig(source).draft;
  assert.equal(draft.thumbnailConcurrency, 1);
  assert.equal(draft.previewConcurrency, 1);
  const updated = updatePreviewConcurrency(source, 3);
  assert.equal(parseConfig(updated).draft.previewConcurrency, 3);
  assert.match(updated, /preview_concurrency: 3/);
});
