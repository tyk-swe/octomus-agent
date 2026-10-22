import { readFileSync } from 'node:fs';
import { sveltekit } from '@sveltejs/kit/vite';
import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vite';
export default defineConfig({
  plugins: [
    sveltekit({
      preprocess: vitePreprocess(),
      adapter: adapter({ fallback: '200.html' }),
      version: { pollInterval: 0 }
    })
  ],
  define: {
    __APP_VERSION__: JSON.stringify(
      readFileSync(new URL('../VERSION', import.meta.url), 'utf8').trim()
    )
  },
  server: { proxy: { '/api': 'http://127.0.0.1:4200', '/healthz': 'http://127.0.0.1:4200' } }
});
