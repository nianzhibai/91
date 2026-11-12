import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { appendFileSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const archives = ['91-linux-amd64.tar.gz', '91-linux-arm64.tar.gz'];

export function parseVersion(tag) {
  const match = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$/.exec(tag);
  if (!match || match[4]?.split('.').some(part => /^\d+$/.test(part) && part.length > 1 && part.startsWith('0'))) {
    throw new Error(`Invalid release tag: ${tag}; expected vMAJOR.MINOR.PATCH[-prerelease]`);
  }
  return { tag, version: tag.slice(1), major: match[1], minor: match[2], patch: match[3], prerelease: Boolean(match[4]) };
}

function compare(a, b) {
  for (const key of ['major', 'minor', 'patch']) {
    if (BigInt(a[key]) !== BigInt(b[key])) return BigInt(a[key]) > BigInt(b[key]) ? 1 : -1;
  }
  return 0;
}

export function promotionTags(version, releases) {
  if (version.prerelease) return [];
  const newer = releases.filter(release => !release.draft && !release.prerelease).flatMap(release => {
    try {
      const parsed = parseVersion(release.tag_name);
      return !parsed.prerelease && compare(parsed, version) > 0 ? [parsed] : [];
    } catch { return []; }
  });
  const tags = [];
  if (!newer.some(other => other.major === version.major && other.minor === version.minor)) tags.push(`${version.major}.${version.minor}`);
  if (!newer.some(other => other.major === version.major)) tags.push(version.major);
  if (newer.length === 0) tags.push('latest', 'stable');
  return tags;
}

function command(program, args, options = {}) {
  return execFileSync(program, args, { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024, ...options });
}

// Releases promote shared image channels, so they must come from reviewed history.
export function verifyReleaseCommit(sha, branch) {
  if (!/^[a-f0-9]{40}$/.test(sha ?? '') || !branch) throw new Error('Missing release commit or branch');
  try {
    command('git', ['merge-base', '--is-ancestor', sha, `refs/remotes/origin/${branch}`], { stdio: 'pipe' });
  } catch (error) {
    if (error.status === 1) throw new Error(`Release commit ${sha} is not on ${branch}`);
    throw error;
  }
}

function publishImages(tags, sourceTag) {
  command('bash', [fileURLToPath(new URL('./publish-images.sh', import.meta.url))], {
    stdio: 'inherit',
    env: { ...process.env, TAGS: tags.map(tag => `${process.env.IMAGE}:${tag}`).join('\n'), SOURCE_TAG: sourceTag ?? '' },
  });
}

function digest(path) {
  return `sha256:${createHash('sha256').update(readFileSync(path)).digest('hex')}`;
}

export function verifyAssets(release, expected) {
  for (const [name, checksum] of Object.entries(expected)) {
    const asset = release.assets.find(asset => asset.name === name);
    if (!asset || asset.state !== 'uploaded' || asset.size <= 0 || asset.digest !== checksum) {
      throw new Error(`Release asset is missing, incomplete or has the wrong checksum: ${name}`);
    }
  }
}

export function publishRelease() {
  const { RELEASE_TAG: tag, RELEASE_DIR: directory, GITHUB_REPOSITORY: repository, GITHUB_SHA: sha } = process.env;
  const version = parseVersion(tag);
  if (!/^[a-f0-9]{40}$/.test(sha ?? '')) throw new Error('A release must be bound to its triggering commit');
  if (!directory || !repository || !process.env.IMAGE) throw new Error('Missing release directory, repository or image');

  // Refuse to attach artifacts built from one commit to a subsequently moved tag.
  const refs = command('git', ['ls-remote', '--exit-code', '--tags', 'origin', `refs/tags/${tag}`, `refs/tags/${tag}^{}`]).trim().split('\n');
  const commit = (refs.find(line => line.endsWith('^{}')) ?? refs[0]).split(/\s+/)[0];
  if (commit !== sha) throw new Error(`Release tag ${tag} moved after this run started`);

  const endpoint = `repos/${repository}/releases/tags/${tag}`;
  const api = path => JSON.parse(command('gh', ['api', path]));
  let release;
  try {
    release = api(endpoint);
  } catch (error) {
    if (!String(error.stderr).includes('(HTTP 404)')) throw error;
  }

  if (release && (release.target_commitish !== sha || release.prerelease !== version.prerelease)) {
    throw new Error('Existing release belongs to a different commit or release channel');
  }

  if (!release || release.draft) {
    const expected = {};
    for (const name of archives) {
      const path = join(directory, name);
      if (statSync(path).size === 0) throw new Error(`Empty release archive: ${name}`);
      expected[name] = digest(path);
    }
    const sumsPath = join(directory, 'SHA256SUMS');
    writeFileSync(sumsPath, archives.map(name => `${expected[name].slice(7)}  ${name}\n`).join(''));
    expected.SHA256SUMS = digest(sumsPath);

    if (!release) {
      const notes = command('git', ['tag', '-l', tag, '--format=%(contents)']).trim() || 'Prebuilt Linux release packages.';
      const notesPath = join(directory, 'release-notes.md');
      writeFileSync(notesPath, `${notes}\n\nCommit: \`${sha}\`\n`);
      command('gh', ['release', 'create', tag, '--repo', repository, '--draft', '--verify-tag', '--target', sha,
        '--title', tag, '--notes-file', notesPath, ...(version.prerelease ? ['--prerelease'] : [])]);
    }
    command('gh', ['release', 'upload', tag, '--repo', repository, '--clobber', ...Object.keys(expected).map(name => join(directory, name))]);
    release = api(endpoint);
    verifyAssets(release, expected);
    // Version tags are prepared before the release or any shared channel goes live.
    publishImages([tag, version.version]);
    command('gh', ['release', 'edit', tag, '--repo', repository, '--draft=false', '--latest=false']);
  } else {
    // A full rerun may rebuild different archive bytes. Reuse the published assets
    // and version images; never replace downloads that users have already received.
    const sums = command('gh', ['release', 'download', tag, '--repo', repository, '--pattern', 'SHA256SUMS', '--output', '-']);
    const expected = {};
    for (const name of archives) {
      const checksum = sums.split('\n').find(line => line.endsWith(`  ${name}`))?.split('  ')[0];
      if (!/^[a-f0-9]{64}$/.test(checksum ?? '')) throw new Error(`Published checksums omit ${name}`);
      expected[name] = `sha256:${checksum}`;
    }
    expected.SHA256SUMS = `sha256:${createHash('sha256').update(sums).digest('hex')}`;
    verifyAssets(release, expected);
    console.log(`Reusing published assets for ${tag}`);
  }

  // Serialization prevents concurrent promotion; comparison also protects an old
  // tag retried after a newer release. Older minor lines may still advance themselves.
  const releases = JSON.parse(command('gh', ['api', '--paginate', '--slurp', `repos/${repository}/releases?per_page=100`])).flat();
  const channels = promotionTags(version, releases);
  if (channels.length) publishImages(channels, tag);
  if (channels.includes('latest')) command('gh', ['release', 'edit', tag, '--repo', repository, '--latest=true']);
  const summary = `Published ${tag} from ${sha}. Image channels: ${channels.join(', ') || 'version only'}.\n`;
  console.log(summary);
  if (process.env.GITHUB_STEP_SUMMARY) appendFileSync(process.env.GITHUB_STEP_SUMMARY, summary);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  if (process.argv[2] === 'metadata') {
    const version = parseVersion(process.env.RELEASE_TAG);
    verifyReleaseCommit(process.env.GITHUB_SHA, process.env.RELEASE_BRANCH);
    appendFileSync(process.env.GITHUB_OUTPUT, `version=${version.tag}\n`);
  } else if (process.argv[2] === 'publish') {
    publishRelease();
  } else {
    throw new Error('Expected metadata or publish');
  }
}
