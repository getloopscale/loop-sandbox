import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

const manifest = JSON.parse(
  readFileSync(new URL('../package.json', import.meta.url), 'utf8'),
) as { name: string };

describe('package manifest', () => {
  it('is published as the unscoped package loopsandbox', () => {
    expect(manifest.name).toBe('loopsandbox');
  });
});
