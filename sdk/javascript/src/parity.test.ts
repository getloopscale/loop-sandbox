import { describe, expect, it } from 'vitest';
import openapiText from '../../../api/openapi.yaml?raw';
import { LoopSandbox } from './client.js';
import type { FetchLike } from './http.js';

// The parity contract: every operation of api/openapi.yaml is reachable through
// a typed SDK method, or is excluded here with a written reason. Adding an
// operation to the API without touching one of the two tables fails this file.

// COVERAGE maps an operationId to the SDK method that calls it. A path starts
// at one of the roots built in resolveRoots: "client" (a LoopSandbox),
// "sandbox" (a Sandbox handle) or "session" (a Session handle).
const COVERAGE: Record<string, string> = {
  // sandboxes
  listSandboxes: 'client.sandboxes.list',
  createSandbox: 'client.sandboxes.create',
  getSandbox: 'client.sandboxes.get',
  destroySandbox: 'client.sandboxes.destroy',
  setSandboxTimeout: 'client.sandboxes.setTimeout',
  updateSandboxNetwork: 'client.sandboxes.updateNetwork',
  listNetworkDenials: 'client.sandboxes.networkDenials',
  forkSandbox: 'client.sandboxes.fork',
  pauseSandbox: 'client.sandboxes.pause',
  resumeSandbox: 'client.sandboxes.resume',
  getSandboxMetrics: 'client.sandboxes.metrics',
  getSandboxLogs: 'client.sandboxes.logs',
  // executions
  createExecution: 'sandbox.commands.run',
  // files
  listFiles: 'sandbox.files.list',
  putFile: 'sandbox.files.write',
  getFile: 'sandbox.files.read',
  deleteFile: 'sandbox.files.delete',
  getFileInfo: 'sandbox.files.info',
  makeDirectory: 'sandbox.files.makeDir',
  moveFile: 'sandbox.files.move',
  createFileUrl: 'sandbox.files.createUrl',
  getArtifactUrl: 'sandbox.files.artifactUrl',
  downloadSignedFile: 'client.signedFiles.download',
  uploadSignedFile: 'client.signedFiles.upload',
  startWatch: 'sandbox.watches.start',
  getWatchEvents: 'sandbox.watches.events',
  stopWatch: 'sandbox.watches.stop',
  // processes
  startProcess: 'sandbox.processes.start',
  listProcesses: 'sandbox.processes.list',
  getProcess: 'sandbox.processes.get',
  getProcessOutput: 'sandbox.processes.output',
  writeProcessStdin: 'sandbox.processes.writeStdin',
  closeProcessStdin: 'sandbox.processes.closeStdin',
  signalProcess: 'sandbox.processes.signal',
  // sessions
  startSession: 'sandbox.sessions.start',
  runCell: 'session.execute',
  closeSession: 'session.close',
  // terminal
  openPty: 'sandbox.pty.open',
  // browser
  browserNavigate: 'sandbox.browser.navigate',
  browserScreenshot: 'sandbox.browser.screenshot',
  browserDom: 'sandbox.browser.dom',
  browserInput: 'sandbox.browser.input',
  // secrets
  putSecret: 'client.secrets.put',
  listSecrets: 'client.secrets.list',
  deleteSecret: 'client.secrets.delete',
  // snapshots
  createSnapshot: 'client.snapshots.create',
  listSnapshots: 'client.snapshots.list',
  getSnapshot: 'client.snapshots.get',
  deleteSnapshot: 'client.snapshots.delete',
  // volumes
  createVolume: 'client.volumes.create',
  listVolumes: 'client.volumes.list',
  deleteVolume: 'client.volumes.delete',
  // api keys
  listApiKeys: 'client.apiKeys.list',
  createApiKey: 'client.apiKeys.create',
  revokeApiKey: 'client.apiKeys.revoke',
  // platform
  getCapabilities: 'client.capabilities.get',
  healthz: 'client.platform.health',
  readyz: 'client.platform.ready',
  metrics: 'client.platform.metrics',
  // templates
  listTemplates: 'client.templates.list',
  registerTemplate: 'client.templates.register',
  getTemplate: 'client.templates.get',
};

// EXCLUSIONS maps an operationId to the reason it has no SDK method. Use it
// only when a typed method makes no sense for the operation. Empty today.
const EXCLUSIONS: Record<string, string> = {};

const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'patch', 'head', 'options', 'trace'];

interface SpecOperations {
  /** Every operationId under "paths", in document order. */
  operationIds: string[];
  /** How many HTTP operations "paths" declares, with or without an operationId. */
  operationCount: number;
}

// readOperations scans the "paths" section of the OpenAPI document line by
// line. The document nests an operation at four spaces and its operationId at
// six, which is all this needs, so the SDK takes no YAML parser dependency.
function readOperations(text: string): SpecOperations {
  const lines = text.split(/\r?\n/);
  const start = lines.indexOf('paths:');
  const end = lines.indexOf('components:');
  if (start === -1 || end === -1 || end < start) {
    throw new Error('api/openapi.yaml: could not find the "paths" and "components" sections');
  }
  const operationIds: string[] = [];
  let operationCount = 0;
  for (const line of lines.slice(start + 1, end)) {
    const method = /^ {4}([a-z]+):\s*$/.exec(line);
    if (method && HTTP_METHODS.includes(method[1]!)) operationCount++;
    const id = /^ {6}operationId:\s*([A-Za-z0-9_]+)\s*$/.exec(line);
    if (id) operationIds.push(id[1]!);
  }
  return { operationIds, operationCount };
}

const sandboxBody = {
  id: 'sbx_1',
  tenant_id: 'tenant-a',
  state: 'running',
  created_at: '2026-07-16T00:00:00Z',
};

// resolveRoots builds real SDK objects, so that a coverage path is checked
// against what a caller actually gets, not against a class declaration.
async function resolveRoots(): Promise<Record<string, unknown>> {
  const fetch: FetchLike = async () =>
    new Response(JSON.stringify(sandboxBody), { status: 201, headers: { 'Content-Type': 'application/json' } });
  const client = new LoopSandbox({ baseUrl: 'https://sandbox.test', apiKey: 'lsb_k', fetch });
  const sandbox = await client.sandboxes.create();
  const session = sandbox.sessions.attach('ses_1');
  return { client, sandbox, session };
}

// resolvePath walks "root.property.method" and returns what it finds, or
// undefined when a step is missing.
function resolvePath(roots: Record<string, unknown>, path: string): unknown {
  let current: unknown = roots;
  for (const step of path.split('.')) {
    if (typeof current !== 'object' || current === null) return undefined;
    current = (current as Record<string, unknown>)[step];
  }
  return current;
}

describe('API parity with api/openapi.yaml', () => {
  const spec = readOperations(openapiText);

  it('finds an operationId for every HTTP operation of the document', () => {
    expect(spec.operationCount).toBeGreaterThan(0);
    expect(spec.operationIds).toHaveLength(spec.operationCount);
    expect(new Set(spec.operationIds).size).toBe(spec.operationIds.length);
  });

  it('covers or excludes every operation of the API', () => {
    const unaccounted = spec.operationIds.filter(
      (id) => !Object.hasOwn(COVERAGE, id) && !Object.hasOwn(EXCLUSIONS, id),
    );

    expect(unaccounted, 'operations with no SDK method and no exclusion').toEqual([]);
  });

  it('names no operation that the API no longer has', () => {
    const known = new Set(spec.operationIds);
    const stale = [...Object.keys(COVERAGE), ...Object.keys(EXCLUSIONS)].filter((id) => !known.has(id));

    expect(stale, 'table entries absent from api/openapi.yaml').toEqual([]);
  });

  it('maps every covered operation to a function that exists on the SDK', async () => {
    const roots = await resolveRoots();

    const missing = Object.entries(COVERAGE)
      .filter(([, path]) => typeof resolvePath(roots, path) !== 'function')
      .map(([id, path]) => `${id} -> ${path}`);

    expect(missing, 'coverage paths that are not SDK methods').toEqual([]);
  });

  it('never lists an operation as both covered and excluded', () => {
    const both = Object.keys(EXCLUSIONS).filter((id) => Object.hasOwn(COVERAGE, id));

    expect(both).toEqual([]);
  });

  it('gives a written reason for every exclusion', () => {
    const unexplained = Object.entries(EXCLUSIONS)
      .filter(([, reason]) => reason.trim().length < 20)
      .map(([id]) => id);

    expect(unexplained).toEqual([]);
  });
});
