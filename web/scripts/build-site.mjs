import { copyFile, cp, mkdir, rm } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { build } from 'vite';

const web = fileURLToPath(new URL('../', import.meta.url));
const root = resolve(web, '..');
const outDir = resolve(root, 'dist/site');
const publicDir = resolve(root, 'dist/site-public');

try {
  await rm(outDir, { recursive: true, force: true });
  await rm(publicDir, { recursive: true, force: true });
  // Always rebuild the explicit launch fixture. Showcase tests produce other payloads.
  execFileSync(
    process.execPath,
    [
      resolve(web, 'scripts/build-showcase.mjs'),
      '--mode',
      'fixture',
      '--input',
      resolve(web, 'launch/sample.public.json')
    ],
    { cwd: web, stdio: 'inherit' }
  );
  // Stage only public artwork; Vite rewrites its URLs for the Pages subdirectory.
  await mkdir(publicDir, { recursive: true });
  await cp(resolve(web, 'launch/public'), publicDir, { recursive: true });
  await copyFile(resolve(web, 'static/favicon.svg'), resolve(publicDir, 'favicon.svg'));
  await copyFile(resolve(root, 'docs/dashboard.png'), resolve(publicDir, 'dashboard.png'));
  await copyFile(
    resolve(root, 'docs/launch-assets/social-card.png'),
    resolve(publicDir, 'social-card.png')
  );
  await build({
    configFile: false,
    root: resolve(web, 'launch'),
    base: './',
    publicDir,
    build: { outDir, emptyOutDir: true, sourcemap: false }
  });
  await cp(resolve(root, 'dist/showcase'), resolve(outDir, 'showcase'), { recursive: true });
  console.log(`Public launch site: ${outDir}`);
} catch (error) {
  await rm(outDir, { recursive: true, force: true });
  console.error(`Site build failed: ${error instanceof Error ? error.message : String(error)}`);
  process.exitCode = 1;
} finally {
  await rm(publicDir, { recursive: true, force: true });
}
