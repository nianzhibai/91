# Internal duplicate records

`duplicate_records` stores committed duplicate decisions for internal SQL
inspection. It has no HTTP endpoint or website presentation. `catalog.Open`
creates the table for new and existing databases when the updated server starts.
Only new duplicate decisions are recorded. Startup does not backfill old
tombstones, crawler seen entries, scan summaries or operational logs. Existing
audit records remain unchanged, and the original tombstones and crawler seen
state continue to control admission independently of the audit table.

The history is included when **all resource types** are selected for backup.
Partial backups omit it because a decision may retain comparison snapshots
from unselected storage. Restore preserves target history and merges history
from full resource backups without rewriting historical IDs. Re-importing the
same decision uses the earliest first observation, latest last observation and
largest occurrence count, rather than adding counts from overlapping backups.
An older backup without this table restores without inventing audit records
from its tombstones or crawler seen entries.

| `origin` | `outcome` | When recorded |
| --- | --- | --- |
| `scan` | `skipped_import` | A new source was rejected as a live duplicate |
| `scan` | `matched_existing` | A scanned, already admitted row matched another row |
| `crawler` | `skipped_import` | An exact or perceptual match prevented import |
| `crawler` | `replaced` | A larger incoming video replaced a previous row |
| `maintenance` | `merged` | Server maintenance committed its merge plan |
| `command` | `merged` | `dedupe-dryrun -apply` committed its merge plan |

Read-only planning does not write records. Records never act as tombstones,
admission blockers or canonical redirects. Identity-only re-observations of an
existing source and skips caused by an existing tombstone do not create new
duplicate decisions. For this reason, a scan's total `duplicateCount` is not
necessarily the number of newly created records.

`reason` is one of `content_hash`, `file_name_size`, `sampled_sha256`,
`title_duration_thumbnail`, `content_frames`, or `unknown`. `content_hash`
means the provider hash strings matched under the current scanner policy; no
algorithm is inferred from hash length. `size_bytes` and snapshot `sizeBytes`
retain the reported value, without claiming independent size verification.
`unknown` is used when an internal caller supplies no matching evidence.

Each row keeps source, canonical, matched and selected video snapshots and
versioned JSON `evidence`. File names, titles, durations and hashes remain
available after the live rows are renamed or removed. Counters, transient paths
and observation timestamps are excluded from the snapshots. The record key
covers the origin, outcome, snapshots and evidence: identical decisions update
`last_seen_at` and `occurrences`; changed observations create a new record.

Each available snapshot also preserves `parentId`, `dirName` and
`ancestorDirIds` when known. `parentId` can be used with the recorded `driveId`
to list the parent directory and locate `fileId` again. `dirName` is the direct
parent's name, not a full path; `ancestorDirIds` is an ordered array from the
scan root through the parent, including both endpoints. IDs are opaque provider
values and are never joined or split as path strings. Directory moves or name
changes produce a new observation while the previous location stays recorded.
Revalidation must use current drive credentials and confirm the file still
exists; these are historical locations, not a guarantee of future access.

Older records with no directory fields remain unchanged. A subsequent scan can
record the newly observed location; startup never fills old snapshots with
today's catalog metadata. Sources without known directory metadata omit these fields.

`matched_video_id` names an actual comparison partner. `canonical_video_id`
names the final survivor **at the time of the decision**. For a transitive
A -> B -> C merge, A records its A-B comparison and retains B's snapshot even
though C survives. B's record supplies the next edge. Scores keep their original
`leftId`/`rightId` orientation, including directional cross-match counts.
`selected_snapshot` and `evidence.selectedVideoId` identify the original channel
winner explained by `selection_reason`; a later channel may replace it.
Historical records are not rewritten when canonical references later change.

Evidence version 1 uses the current rules: title similarity >= 0.90, thumbnail
SSIM >= 0.95 and duration tolerance 2 seconds; aligned content needs at least
6 valid comparisons and median SSIM >= 0.80. Cross matching needs equal durations,
at least 8 valid frames on each side and >= 75% strong matches in both directions
(frame SSIM >= 0.95). Content candidates must be at least 120 seconds long.
Increment `EvidenceVersion` when changing the meaning of these rules or scores.

From the project root, for example:

```sh
sqlite3 -readonly backend/data/91.db
```

```sql
-- Distinct recorded decisions versus repeated observations.
SELECT origin, outcome, reason, COUNT(*) AS decisions,
       SUM(occurrences) AS observations
FROM duplicate_records
GROUP BY origin, outcome, reason;

-- Find same-name/size matches without joining potentially removed video rows.
SELECT file_name, drive_id, size_bytes, canonical_video_id,
       json_extract(source_snapshot, '$.parentId') AS source_parent_id,
       json_extract(source_snapshot, '$.dirName') AS source_directory,
       json_extract(source_snapshot, '$.ancestorDirIds') AS source_ancestors,
       json_extract(canonical_snapshot, '$.fileName') AS retained_name,
       json_extract(canonical_snapshot, '$.driveId') AS retained_drive,
       json_extract(canonical_snapshot, '$.parentId') AS retained_parent_id,
       datetime(last_seen_at / 1000, 'unixepoch') AS last_seen_utc,
       occurrences
FROM duplicate_records
WHERE reason = 'file_name_size'
ORDER BY last_seen_at DESC
LIMIT 50;
```
