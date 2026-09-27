import { readFileSync } from 'node:fs';
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';
export default defineConfig({
  plugins: [sveltekit()],
  // The repository's single version source, which the service binary embeds too.
  define: {
    __APP_VERSION__: JSON.stringify(
      readFileSync(new URL('../VERSION', import.meta.url), 'utf8').trim()
    )
  },
  server: { proxy: { '/api': 'http://127.0.0.1:4200', '/healthz': 'http://127.0.0.1:4200' } }
});
