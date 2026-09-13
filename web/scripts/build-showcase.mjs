import { readFile, stat, mkdir, rm, writeFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { parseArgs } from 'node:util';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { build } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { publicPayload, publicApproval } from '../showcase/contract.mjs';
import { parseUniqueJson } from './public-json.mjs';

const root = fileURLToPath(new URL('../showcase/', import.meta.url));
const outDir = fileURLToPath(new URL('../../dist/showcase/', import.meta.url));
async function localBytes(path) {
  if (!path || /^[a-z]+:\/\//i.test(path))
    throw new Error('An explicit local JSON file is required');
  const info = await stat(path);
  if (!info.isFile() || info.size > 5_000_000)
    throw new Error('Input must be a regular file, at most 5 MB');
  return readFile(path);
}
try {
  // A failed invocation must never leave an older build looking like its result.
  await rm(outDir, { recursive: true, force: true });
  const { values } = parseArgs({
    options: { mode: { type: 'string' }, input: { type: 'string' }, approval: { type: 'string' } }
  });
  if (!['fixture', 'recorded'].includes(values.mode))
    throw new Error('Select --mode fixture or --mode recorded, with --input <public.json>');
  const bytes = await localBytes(values.input);
  const payload = publicPayload(parseUniqueJson(bytes.toString('utf8')), values.mode);
  const hash = createHash('sha256').update(bytes).digest('hex');
  let approval = null;
  if (values.mode === 'recorded')
    approval = publicApproval(
      parseUniqueJson((await localBytes(values.approval)).toString('utf8')),
      hash
    );
  else if (values.approval) throw new Error('Fixture mode does not accept approval');
  const bundle = { payload, hash, approval };
  await build({
    configFile: false,
    root,
    base: './',
    publicDir: false,
    // Shared pure helpers must not require the dashboard's generated SvelteKit config.
    esbuild: {
      tsconfigRaw: JSON.stringify({
        compilerOptions: { target: 'ES2022', useDefineForClassFields: true }
      })
    },
    plugins: [
      svelte({ configFile: false }),
      {
        name: 'explicit-public-payload',
        resolveId(id) {
          if (id === 'virtual:public-run') return '\0public-run';
        },
        load(id) {
          if (id === '\0public-run')
            return `export default JSON.parse(${JSON.stringify(JSON.stringify(bundle))})`;
        }
      }
    ],
    build: { outDir, emptyOutDir: true, sourcemap: false }
  });
  await mkdir(outDir, { recursive: true });
  await writeFile(resolve(outDir, 'public-run.json'), bytes);
  if (approval)
    await writeFile(resolve(outDir, 'approval.json'), JSON.stringify(approval, null, 2) + '\n');
  console.log(`Showcase ${values.mode}: ${outDir}\nPublic payload SHA-256: ${hash}`);
} catch (error) {
  await rm(outDir, { recursive: true, force: true });
  console.error(`Showcase build refused: ${error.message}`);
  process.exitCode = 1;
}
