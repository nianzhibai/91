import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
const shortsFeedGoSource = readFileSync(new URL("../backend/internal/api/shorts_feed.go", import.meta.url), "utf8");
test("shorts DTO retains background and bitrate metadata", () => {
  assert.match(
    shortsFeedGoSource,
    /BackgroundPoster\s+string\s+`json:"backgroundPoster,omitempty"`/
  );
  assert.match(
    shortsFeedGoSource,
    /BackgroundPoster:\s+shortsBackgroundPosterURL\(poster\),/
  );
  assert.match(
    shortsFeedGoSource,
    /SizeBytes\s+int64\s+`json:"sizeBytes,omitempty"`/
  );
  assert.match(
    shortsFeedGoSource,
    /DurationSeconds\s+int\s+`json:"durationSeconds,omitempty"`/
  );
  assert.match(
    shortsFeedGoSource,
    /videoSrc, sizeBytes := s\.videoSourceAndSize\(video\)\s*durationSeconds := video\.DurationSeconds/
  );
  assert.match(
    shortsFeedGoSource,
    /if sizeBytes <= 0 \|\| durationSeconds <= 0 \{\s*sizeBytes = 0\s*durationSeconds = 0\s*\}/
  );
  assert.match(
    shortsFeedGoSource,
    /VideoSrc:\s+videoSrc,\s*PlaybackURL:\s+playbackURL\(video.ID\),\s*Poster:[\s\S]*?SizeBytes:\s+sizeBytes,\s*DurationSeconds:\s+durationSeconds,/
  );
});
