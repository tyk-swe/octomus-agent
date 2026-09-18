type SearchEntry = { title: string; section: string; href: string; text: string };

const search = document.querySelector<HTMLElement>('.docs-search')!;
const input = document.querySelector<HTMLInputElement>('#docs-search')!;
const panel = document.querySelector<HTMLElement>('.search-panel')!;
const status = document.querySelector<HTMLElement>('.search-status')!;
const results = document.querySelector<HTMLUListElement>('#search-results')!;
const root = new URL(search.dataset.root!, location.href);
let index: Promise<SearchEntry[]> | undefined;

search.hidden = false;

async function showResults() {
  const query = input.value.trim().toLowerCase();
  results.replaceChildren();
  panel.hidden = query.length === 0;
  if (!query) return;
  status.textContent = 'Searching…';
  try {
    index ??= fetch(new URL('docs/search-index.json', root)).then((response) => {
      if (!response.ok) throw new Error('Search unavailable');
      return response.json() as Promise<SearchEntry[]>;
    });
    const entries = await index;
    if (input.value.trim().toLowerCase() !== query) return;
    const words = query.split(/\s+/);
    const matches = entries
      .map((entry) => {
        const title = `${entry.title} ${entry.section}`.toLowerCase();
        const text = `${title} ${entry.text}`.toLowerCase();
        return {
          entry,
          score: words.every((word) => text.includes(word))
            ? 1 + words.filter((word) => title.includes(word)).length * 5
            : 0
        };
      })
      .filter((match) => match.score > 0)
      .sort((a, b) => b.score - a.score)
      .slice(0, 8);
    status.textContent = matches.length
      ? `${matches.length} result${matches.length === 1 ? '' : 's'} — choose a section below`
      : 'No matching sections. Try “audit”, “models”, or “verification”.';
    for (const { entry } of matches) {
      const item = document.createElement('li');
      const link = document.createElement('a');
      link.href = new URL(entry.href, root).href;
      const title = document.createElement('strong');
      title.textContent = entry.title + (entry.section ? ` · ${entry.section}` : '');
      const snippet = document.createElement('span');
      const start = Math.max(0, entry.text.toLowerCase().indexOf(words[0]) - 35);
      snippet.textContent = (start ? '…' : '') + entry.text.slice(start, start + 150) + '…';
      link.append(title, snippet);
      link.addEventListener('click', () => {
        panel.hidden = true;
      });
      item.append(link);
      results.append(item);
    }
  } catch {
    index = undefined;
    if (input.value.trim().toLowerCase() === query)
      status.textContent = 'Search is unavailable. You can still browse every guide below.';
  }
}

input.addEventListener('input', showResults);
input.addEventListener('focus', () => {
  if (input.value.trim()) void showResults();
});
document.addEventListener('keydown', (event) => {
  if (event.key === 'Escape') {
    if (search.contains(document.activeElement)) input.focus();
    panel.hidden = true;
  }
  if (
    event.key === '/' &&
    !event.ctrlKey &&
    !event.metaKey &&
    !(
      event.target instanceof HTMLElement &&
      event.target.matches('input, textarea, [contenteditable]')
    )
  ) {
    event.preventDefault();
    input.focus();
  }
});
document.addEventListener('click', (event) => {
  if (event.target instanceof Node && !search.contains(event.target)) panel.hidden = true;
});

// Reading and navigation work without JavaScript; these are optional conveniences.
for (const pre of Array.from(document.querySelectorAll<HTMLPreElement>('.prose pre'))) {
  const code = pre.querySelector('code')!;
  const block = document.createElement('div');
  block.className = 'code-block';
  const toolbar = document.createElement('div');
  toolbar.className = 'code-toolbar';
  const language = document.createElement('span');
  language.textContent = code.className.replace('language-', '') || 'text';
  toolbar.append(language);
  if (navigator.clipboard && window.isSecureContext) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = 'copy-code';
    button.textContent = 'Copy';
    button.setAttribute('aria-label', 'Copy code');
    button.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(code.textContent ?? '');
        button.textContent = 'Copied!';
      } catch {
        button.textContent = 'Select to copy';
      }
      setTimeout(() => {
        button.textContent = 'Copy';
      }, 2000);
    });
    toolbar.append(button);
  }
  pre.before(block);
  block.append(toolbar, pre);
}

export {};
