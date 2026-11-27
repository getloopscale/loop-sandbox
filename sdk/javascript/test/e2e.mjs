// SDK end-to-end ship criterion. Runs the FULL agent flow against a REAL running
// LoopSandbox API (real HTTP, real PostgreSQL, real API-key auth):
//
//   create → write file → exec (cat) → read → list → destroy → 404
//
// Env: LSB_BASE_URL (default http://localhost:8080), LSB_API_KEY (required).
// Exits 0 on success, 1 on the first failed assertion. Orchestrated by
// scripts/sdk-e2e.sh (boots PG + API + a tenant key, then runs this).

import { LoopSandbox, NotFoundError } from '../dist/index.js';

const baseUrl = process.env.LSB_BASE_URL ?? 'http://localhost:8080';
const apiKey = process.env.LSB_API_KEY;
const image = process.env.LSB_IMAGE ?? 'busybox:latest';

if (!apiKey) {
  console.error('LSB_API_KEY is required');
  process.exit(1);
}

function assert(cond, msg) {
  if (!cond) {
    console.error(`FAIL: ${msg}`);
    process.exit(1);
  }
  console.log(`  ok: ${msg}`);
}

// ---- The documented happy-path flow (the <20-line agent surface) ----
const client = new LoopSandbox({ baseUrl, apiKey });
const templates = await client.templates.list();
const sandbox = await client.sandboxes.create({ image, ttlSeconds: 300 });
await sandbox.files.write('/workspace/hello.txt', 'hello from the sdk\n');
const exec = await sandbox.commands.run(['cat', '/workspace/hello.txt']);
const downloaded = await sandbox.files.read('/workspace/hello.txt');
const listing = await sandbox.files.list('/workspace');
await sandbox.destroy();
// ---------------------------------------------------------------------

console.log('SDK E2E flow:');
assert(templates.length >= 3, `template catalog has >= 3 entries (${templates.length})`);
assert(sandbox.id.length > 0, `created sandbox ${sandbox.id}`);
assert(sandbox.info.state === 'running', `state is running (${sandbox.info.state})`);
assert(exec.exitCode === 0, `exec cat exit code 0 (${exec.exitCode})`);
assert(exec.stdout.includes('hello from the sdk'), 'exec cat stdout has the written content');
assert(downloaded.includes('hello from the sdk'), 'download returns the written content');
assert(
  listing.some((f) => f.name.endsWith('hello.txt')),
  'listing includes the written file',
);

// After destroy, GET is 404 (indistinguishable from never-existed — US-05 shape).
let gotNotFound = false;
try {
  await client.sandboxes.get(sandbox.id);
} catch (e) {
  gotNotFound = e instanceof NotFoundError;
}
assert(gotNotFound, 'GET after destroy raises NotFoundError (404)');

// ---- The rest of the surface, where the deployment supports it ----
const report = await client.capabilities.get();
const supports = (name) => report.capabilities.some((c) => c.name === name && c.supported);
console.log(`\nExtended surface (driver ${report.driver}):`);
const box = await client.sandboxes.create({ image, ttlSeconds: 300, metadata: { suite: 'sdk-e2e' } });
try {
  const listed = await client.sandboxes.list({ state: 'running' });
  assert(listed.sandboxes.some((s) => s.id === box.id), 'sandboxes.list shows the new sandbox');

  const failing = await box.commands.run(['sh', '-c', 'echo oops >&2; exit 3']);
  assert(failing.exitCode === 3 && failing.stderr.includes('oops'), 'a failing command returns its exit code and stderr');

  if (supports('file_ops')) {
    await box.files.makeDir('/workspace/a/b');
    await box.files.write('/workspace/a/b/x.txt', 'x');
    await box.files.move('/workspace/a/b/x.txt', '/workspace/a/y.txt');
    const info = await box.files.info('/workspace/a/y.txt');
    assert(info.size === 1 && info.isDir === false, 'makeDir, move and info agree on the moved file');
  }

  if (supports('processes')) {
    const proc = await box.processes.start(['sh', '-c', 'read line; echo got:$line']);
    await box.processes.writeStdin(proc.pid, 'ping\n');
    let out = { stdout: '', next: 0, process: { running: true } };
    let stdout = '';
    for (let i = 0; i < 10 && out.process.running; i++) {
      out = await box.processes.output(proc.pid, { after: out.next, waitMs: 2000 });
      stdout += out.stdout;
    }
    assert(stdout.includes('got:ping') && out.process.exitCode === 0, 'a background process reads stdin and its output is read by cursor');
    assert((await box.processes.list()).some((p) => p.pid === proc.pid), 'processes.list shows the process');
  }

  if (supports('file_watch')) {
    const watch = await box.watches.start('/workspace');
    await box.files.write('/workspace/watched.txt', 'w');
    const ev = await box.watches.events(watch.watchId, { waitMs: 5000 });
    assert(ev.events.some((e) => e.path.endsWith('watched.txt')), 'a watch reports the file that was written');
    await box.watches.stop(watch.watchId);
  }

  if (supports('metrics')) {
    const m = await box.metrics();
    assert(m.memoryLimitBytes > 0, `metrics report a memory limit (${m.memoryLimitBytes})`);
  }

  if (supports('logs')) {
    const logs = await box.logs({ after: 0 });
    assert(Array.isArray(logs.entries), 'logs returns a list of entries');
  }

  let signed = null;
  try {
    signed = await box.files.createUrl('/workspace/up.txt', 'upload', { expiresInSeconds: 60 });
  } catch (e) {
    if (e.status !== 501) throw e;
    console.log('  skipped: signed URLs are off in this deployment');
  }
  if (signed) {
    await client.signedFiles.upload(signed.url, 'by-url');
    const down = await box.files.createUrl('/workspace/up.txt', 'download');
    const bytes = await client.signedFiles.download(down.url);
    assert(new TextDecoder().decode(bytes) === 'by-url', 'a file uploaded by signed URL is downloaded by signed URL');
  }

  if (supports('fork')) {
    const forks = await box.forkMany(2);
    assert(forks.length === 2, 'forkMany(2) returns two sandboxes');
    for (const f of forks) await f.destroy();
  }
} finally {
  await box.destroy();
}

console.log('\nSDK E2E PASSED');
