import { copyFile, cp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { resolve } from 'node:path';
import { build } from 'vite';
import { buildDocs } from './site-docs.mjs';

const web = fileURLToPath(new URL('../', import.meta.url));
const root = resolve(web, '..');
const outDir = resolve(root, 'dist/site');
const publicDir = resolve(root, 'dist/site-public');
const sourceDir = resolve(root, 'dist/site-source');

try {
  await rm(outDir, { recursive: true, force: true });
  await rm(publicDir, { recursive: true, force: true });
  await rm(sourceDir, { recursive: true, force: true });
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
  // Stage explicit public sources. Vite keeps assets portable to a subdirectory.
  await mkdir(publicDir, { recursive: true });
  await mkdir(sourceDir, { recursive: true });
  for (const file of ['index.html', 'style.css', 'docs.css', 'docs.ts'])
    await copyFile(resolve(web, 'launch', file), resolve(sourceDir, file));
  await cp(resolve(web, 'launch/public'), publicDir, { recursive: true });
  await copyFile(resolve(web, 'static/favicon.svg'), resolve(publicDir, 'favicon.svg'));
  await copyFile(resolve(root, 'docs/dashboard.png'), resolve(publicDir, 'dashboard.png'));
  await copyFile(
    resolve(root, 'docs/launch-assets/social-card.png'),
    resolve(publicDir, 'social-card.png')
  );
  const docs = await buildDocs(root, sourceDir, publicDir);
  await copyFile(
    resolve(root, 'docs/configuration.example.json'),
    resolve(publicDir, 'docs/configuration.example.json')
  );
  await build({
    configFile: false,
    root: sourceDir,
    base: './',
    publicDir,
    build: {
      outDir,
      emptyOutDir: true,
      sourcemap: false,
      rollupOptions: { input: [resolve(sourceDir, 'index.html'), ...docs] }
    }
  });
  // A 404 can be served at any depth, so its assets must resolve from the domain root.
  const notFound = resolve(outDir, '404.html');
  await writeFile(notFound, (await readFile(notFound, 'utf8')).replaceAll('="./', '="/'));
  await cp(resolve(root, 'dist/showcase'), resolve(outDir, 'showcase'), { recursive: true });
  console.log(`Public launch site: ${outDir}`);
} catch (error) {
  await rm(outDir, { recursive: true, force: true });
  console.error(`Site build failed: ${error instanceof Error ? error.message : String(error)}`);
  process.exitCode = 1;
} finally {
  await rm(publicDir, { recursive: true, force: true });
  await rm(sourceDir, { recursive: true, force: true });
}
