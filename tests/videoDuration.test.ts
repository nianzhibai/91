import assert from "node:assert/strict";
import test from "node:test";
import { formatVideoDuration } from "../src/lib/format.ts";

test("card duration displays hours and carries minute-based API durations", () => {
  assert.equal(formatVideoDuration("00:00"), "00:00:00");
  assert.equal(formatVideoDuration("07:01"), "00:07:01");
  assert.equal(formatVideoDuration("90:15"), "01:30:15");
  assert.equal(formatVideoDuration("1:07:01"), "01:07:01");
  assert.equal(formatVideoDuration("123:59:59"), "123:59:59");
});

test("an unavailable or malformed duration retains its label", () => {
  for (const value of ["", "--:--", "unknown", "00:99", "-1:00", "9999999999999999999:00"]) {
    assert.equal(formatVideoDuration(value), value);
  }
});
