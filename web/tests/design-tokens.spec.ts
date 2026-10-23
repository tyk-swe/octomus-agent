import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { expect, test } from '@playwright/test';

// Design-system guard: colours live in tokens.css as custom properties; the
// other stylesheets and component <style> blocks reference them with var().
const src = fileURLToPath(new URL('../src', import.meta.url));

function* svelteFiles(dir: string): Generator<string> {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) yield* svelteFiles(path);
    else if (entry.name.endsWith('.svelte')) yield path;
  }
}

test('raw colour literals live only in tokens.css', () => {
  const stylesDir = join(src, 'styles');
  const sources: { label: string; css: string }[] = readdirSync(stylesDir)
    .filter((name) => name.endsWith('.css') && name !== 'tokens.css')
    .map((name) => ({
      label: `src/styles/${name}`,
      css: readFileSync(join(stylesDir, name), 'utf8')
    }));
  for (const file of svelteFiles(src)) {
    const content = readFileSync(file, 'utf8');
    const label = `src/${file.split('/src/')[1]} <style>`;
    for (const match of content.matchAll(/<style[^>]*>([\s\S]*?)<\/style>/g))
      sources.push({ label, css: match[1] });
  }
  const failures: string[] = [];
  for (const { label, css } of sources) {
    // Encoded colours inside data: URIs (the select chevron) are allowed.
    css
      .replace(/url\((['"])data:[\s\S]*?\1\)/g, 'url(data:removed)')
      .split('\n')
      .forEach((line, i) => {
        if (/#[0-9a-fA-F]{3,8}\b/.test(line) || /\b(rgba?|hsla?)\(/.test(line))
          failures.push(`${label}:${i + 1}: ${line.trim()}`);
      });
  }
  expect(failures).toEqual([]);
});

test('every declared token is referenced', () => {
  const declared = [
    ...readFileSync(join(src, 'styles', 'tokens.css'), 'utf8').matchAll(/(--[\w-]+)\s*:/g)
  ].map((m) => m[1]);
  function* sourceFiles(dir: string): Generator<string> {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const path = join(dir, entry.name);
      if (entry.isDirectory()) yield* sourceFiles(path);
      else if (/\.(css|svelte|ts|html)$/.test(entry.name)) yield path;
    }
  }
  const corpus = [...sourceFiles(src)].map((file) => readFileSync(file, 'utf8')).join('\n');
  const unused = declared.filter(
    (name) => !new RegExp(`var\\(${name.replace(/-/g, '\\-')}[),\\s]`).test(corpus)
  );
  expect(unused).toEqual([]);
});

test('tokens.css declares only custom properties inside :root', () => {
  const text = readFileSync(join(src, 'styles', 'tokens.css'), 'utf8');
  const body = text.replace(/\/\*[\s\S]*?\*\//g, '');
  expect(body.trim()).toMatch(/^:root \{[\s\S]*\}$/);
  const inner = body.trim().slice(':root {'.length, -1);
  const failures = inner
    .split('\n')
    .map((line) => line.trim())
    .filter(Boolean)
    .filter((line) => !line.startsWith('--'));
  expect(failures).toEqual([]);
});
