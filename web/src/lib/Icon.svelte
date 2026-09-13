<script lang="ts">
  let { name, size = 20 }: { name: string; size?: number } = $props();
  const paths: Record<string, string> = {
    overview: 'M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z',
    queue: 'M9 6h12M9 12h12M9 18h12M3 6h.01M3 12h.01M3 18h.01',
    proposals: 'M9 18h6M10 22h4M8 14c-5-5-2-12 4-12s9 7 4 12l-1 2H9z',
    prs: 'M6 8v12M6 4v.01M18 16V8c0-2-2-3-4-3h-2m3-3-3 3 3 3M3 4a3 3 0 1 0 6 0 3 3 0 1 0-6 0M15 19a3 3 0 1 0 6 0 3 3 0 1 0-6 0',
    settings:
      'M12 8a4 4 0 1 0 0 8 4 4 0 1 0 0-8M12 2v3M12 19v3M2 12h3M19 12h3M5 5l2 2M17 17l2 2M5 19l2-2M17 7l2-2',
    arrow: 'M5 12h14m-5-5 5 5-5 5',
    play: 'm8 5 11 7-11 7z',
    pause: 'M8 5v14M16 5v14',
    check: 'm5 12 4 4L19 6',
    close: 'm6 6 12 12M6 18 18 6',
    clock: 'M12 8v5l3 2M22 12a10 10 0 1 1-20 0 10 10 0 1 1 20 0',
    branch:
      'M6 3v12c0 4 12 4 12-2V9M3 5a3 3 0 1 0 6 0 3 3 0 1 0-6 0M15 6a3 3 0 1 0 6 0 3 3 0 1 0-6 0M3 19a3 3 0 1 0 6 0 3 3 0 1 0-6 0',
    external: 'M14 3h7v7m0-7L10 14M10 3H3v18h18v-7',
    search: 'm16 16 5 5M18 10a8 8 0 1 1-16 0 8 8 0 1 1 16 0',
    activity: 'M2 12h5l3-8 4 16 3-8h5',
    shield: 'm12 2 9 4v6c0 5-9 10-9 10S3 17 3 12V6zM8 12l3 3 5-6',
    code: 'm8 6-6 6 6 6m8-12 6 6-6 6M14 3l-4 18',
    refresh: 'M20 8a9 9 0 1 0 1 7M20 2v6h-6',
    key: 'M15 7a5 5 0 1 1-3 9L4 23H1v-4l8-8a5 5 0 0 1 6-4M16 10h.01',
    chevron: 'm9 5 7 7-7 7',
    alert: 'm12 3 10 18H2zM12 9v5M12 17h.01',
    menu: 'M3 6h18M3 12h18M3 18h18',
    logout: 'M9 3H3v18h6M10 12h11m-4-4 4 4-4 4',
    copy: 'M9 9h11v11H9zM15 9V4H4v11h5'
  };
</script>

<svg
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width="1.65"
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"><path d={paths[name] ?? paths.activity} /></svg
>
