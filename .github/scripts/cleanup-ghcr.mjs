import { execFileSync } from 'node:child_process';
import { appendFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

const SHA_TAG_PATTERN = /^sha-[a-f0-9]+(?:-rockchip)?$/;

function command(program, args, options = {}) {
  return execFileSync(program, args, { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024, ...options });
}

export function isShaOnlyVersion(version) {
  const tags = version.metadata?.container?.tags ?? [];
  return tags.length > 0 && tags.every(tag => SHA_TAG_PATTERN.test(tag));
}

export function selectVersionsToDelete(versions, { nowMs = Date.now(), keepDays = 14, keepShaVersions = 10, referencedDigests = new Set() } = {}) {
  const cutoffMs = nowMs - keepDays * 24 * 60 * 60 * 1000;
  const sorted = [...versions].sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at));
  const toDelete = [];
  let keptSha = 0;

  for (const version of sorted) {
    const tags = version.metadata?.container?.tags ?? [];
    const createdMs = Date.parse(version.created_at);
    const isOld = Number.isFinite(createdMs) && createdMs < cutoffMs;

    if (tags.length === 0) {
      if (isOld && !referencedDigests.has(version.name)) {
        toDelete.push({ id: version.id, name: version.name, reason: 'untagged' });
      }
      continue;
    }

    if (isShaOnlyVersion(version)) {
      if (keptSha < keepShaVersions) {
        keptSha++;
      } else if (isOld) {
        toDelete.push({ id: version.id, name: version.name, reason: `stale-sha:${tags.join(',')}` });
      }
    }
  }
  return toDelete;
}

export function collectReferencedDigests(versions, toDeleteIds, inspectRaw) {
  const referenced = new Set();
  for (const version of versions) {
    const tags = version.metadata?.container?.tags ?? [];
    if (tags.length === 0 || toDeleteIds.has(version.id)) continue;
    const raw = inspectRaw(version.name);
    const manifest = JSON.parse(raw);
    for (const entry of manifest.manifests ?? []) {
      if (entry.digest) referenced.add(entry.digest);
    }
  }
  return referenced;
}

function packageEndpoint(repository) {
  const [owner, repo] = repository.split('/');
  const ownerInfo = JSON.parse(command('gh', ['api', `users/${owner}`]));
  const scope = ownerInfo.type === 'Organization' ? `orgs/${owner}` : `users/${owner}`;
  return `${scope}/packages/container/${encodeURIComponent(repo.toLowerCase())}/versions`;
}

export function cleanupRegistry(env = process.env) {
  const repository = env.GITHUB_REPOSITORY;
  if (!repository) throw new Error('GITHUB_REPOSITORY is required');
  const keepDays = Number(env.KEEP_DAYS ?? 14);
  const keepShaVersions = Number(env.KEEP_SHA_VERSIONS ?? 10);
  const dryRun = env.DRY_RUN === 'true';
  const image = `ghcr.io/${repository.toLowerCase()}`;
  const endpoint = packageEndpoint(repository);

  const versions = JSON.parse(command('gh', ['api', '--paginate', '--slurp', `${endpoint}?per_page=100`])).flat();
  // First determine which stale sha-* tagged versions will be pruned, then inspect all
  // surviving tagged manifests so child manifests of any kept image are never deleted.
  const staleSha = selectVersionsToDelete(versions, { keepDays, keepShaVersions, referencedDigests: new Set(versions.map(v => v.name)) });
  const staleShaIds = new Set(staleSha.map(v => v.id));
  const referencedDigests = collectReferencedDigests(versions, staleShaIds, digest =>
    command('docker', ['buildx', 'imagetools', 'inspect', '--raw', `${image}@${digest}`]),
  );
  const targets = selectVersionsToDelete(versions, { keepDays, keepShaVersions, referencedDigests })
    .sort((a, b) => Number(a.reason === 'untagged') - Number(b.reason === 'untagged'));

  for (const target of targets) {
    if (dryRun) {
      console.log(`[dry-run] would delete version ${target.id} (${target.name}, ${target.reason})`);
    } else {
      command('gh', ['api', '--method', 'DELETE', `${endpoint}/${target.id}`]);
      console.log(`Deleted version ${target.id} (${target.name}, ${target.reason})`);
    }
  }

  const summary = `GHCR cleanup (${dryRun ? 'dry-run' : 'live'}): ${targets.length} of ${versions.length} versions ${dryRun ? 'eligible' : 'deleted'}.\n`;
  console.log(summary.trim());
  if (env.GITHUB_STEP_SUMMARY) appendFileSync(env.GITHUB_STEP_SUMMARY, summary);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  cleanupRegistry();
}
