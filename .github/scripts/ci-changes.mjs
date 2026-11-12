import { execFileSync } from 'node:child_process';
import { appendFileSync, readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

const all = () => ({ frontend: true, backend: true, images: true });

export function classifyChanges(paths) {
  const checks = { frontend: false, backend: false, images: false };
  for (const path of paths) {
    if (/^(docs\/|backend\/docs\/)/.test(path) || /(^|\/)(README[^/]*\.md|AGENTS\.md|CHANGELOG\.md|LICENSE)$/.test(path)) continue;
    if (/^(tests|sdk|examples)\//.test(path) || /^(package(-lock)?\.json|\.nvmrc|\.npmrc)$/.test(path)) {
      // Node tooling is not part of the image; frontends ship from frontend.lock.json.
      checks.frontend = true;
    } else if (path.startsWith('backend/')) {
      // Several Node tests also verify backend source and configuration.
      checks.frontend = checks.backend = true;
      if (!/(_test\.go$|\/testdata\/|^backend\/integration\/)/.test(path)) checks.images = true;
    } else {
      // Deployment, workflow and unfamiliar inputs conservatively run everything.
      return all();
    }
  }
  return checks;
}

const isCommit = value => /^[a-f0-9]{40}$/.test(value ?? '') && !/^0+$/.test(value);

// Pushes compare against the last successful run rather than the previous push:
// GitHub replaces pending runs, and failed runs publish nothing, yet their changes
// must still reach the published images.
function lastSuccessfulCommit({ GITHUB_REPOSITORY: repository, GITHUB_WORKFLOW_REF: workflowRef, GITHUB_REF_NAME: branch }) {
  if (!repository || !workflowRef || !branch) throw new Error('Missing repository, workflow or branch');
  const workflow = workflowRef.split('@')[0].split('/').pop();
  const query = new URLSearchParams({ branch, event: 'push', status: 'success', per_page: '1' });
  const runs = JSON.parse(execFileSync('gh', ['api', `repos/${repository}/actions/workflows/${workflow}/runs?${query}`], { encoding: 'utf8' }));
  return runs.workflow_runs[0]?.head_sha;
}

export function detectChanges(env = process.env) {
  if (env.GITHUB_EVENT_NAME === 'workflow_dispatch') return all();
  try {
    const head = env.GITHUB_SHA;
    const pullRequest = env.GITHUB_EVENT_NAME === 'pull_request';
    const base = pullRequest ? JSON.parse(readFileSync(env.GITHUB_EVENT_PATH, 'utf8')).pull_request.base.sha : lastSuccessfulCommit(env);
    if (![base, head].every(isCommit)) return all();
    // A rerun of an older commit or rewritten history cannot be diffed safely.
    if (!pullRequest) execFileSync('git', ['merge-base', '--is-ancestor', base, head], { stdio: 'pipe' });
    const range = pullRequest ? `${base}...${head}` : `${base}..${head}`;
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
