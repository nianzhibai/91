import { execFileSync } from 'node:child_process';
import { appendFileSync, readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

const all = () => ({ frontend: true, backend: true, images: true });

export function classifyChanges(paths) {
  const checks = { frontend: false, backend: false, images: false };
  for (const path of paths) {
    if (/^(docs\/|backend\/docs\/)/.test(path) || /(^|\/)(README[^/]*\.md|AGENTS\.md|CHANGELOG\.md|LICENSE)$/.test(path)) continue;
    if (path.startsWith('tests/')) {
      checks.frontend = true;
    } else if (path.startsWith('backend/')) {
      // Several Node tests also verify backend source and configuration.
      checks.frontend = checks.backend = true;
      if (!path.endsWith('_test.go') && !path.includes('/testdata/')) checks.images = true;
    } else if (/^(src\/|public\/|index\.html$|vite\.config\.|tsconfig\.|package(-lock)?\.json$|\.nvmrc$|\.npmrc$)/.test(path)) {
      checks.frontend = checks.images = true;
    } else {
      // Deployment, workflow and unfamiliar inputs conservatively run everything.
      return all();
    }
  }
  return checks;
}

export function detectChanges(env = process.env) {
  if (env.FORCE_ALL === 'true' || env.GITHUB_REF_TYPE === 'tag' || env.GITHUB_EVENT_NAME === 'workflow_dispatch') return all();
  try {
    const event = JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8'));
    const base = event.pull_request?.base.sha ?? event.before;
    const head = env.GITHUB_SHA;
    if (![base, head].every(value => /^[a-f0-9]{40}$/.test(value ?? '') && !/^0+$/.test(value))) return all();
    const range = event.pull_request ? `${base}...${head}` : `${base}..${head}`;
    const paths = execFileSync('git', ['diff', '--name-only', '--no-renames', '-z', range], { encoding: 'utf8', maxBuffer: 16 * 1024 * 1024 }).split('\0').filter(Boolean);
    return classifyChanges(paths);
  } catch (error) {
    console.warn(`Change detection unavailable; running all checks: ${error.message}`);
    return all();
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const checks = detectChanges();
  const output = Object.entries(checks).map(([key, value]) => `${key}=${value}\n`).join('');
  console.log(output.trim());
  if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, output);
}
