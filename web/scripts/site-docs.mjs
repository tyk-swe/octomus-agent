import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { resolve, posix } from 'node:path';
import { Marked } from 'marked';
import GithubSlugger from 'github-slugger';

export const siteOrigin = 'https://octomus-agent.tyk.sh';
const repository = 'https://github.com/tyk-swe/octomus-agent';

// Explicit public inputs: never enumerate operator state or arbitrary Markdown files.
export const documents = [
  {
    slug: '',
    source: 'index',
    label: 'Overview',
    group: 'Start here',
    description: 'Meet Octomus and find your path from a first audit to reviewed pull requests.'
  },
  {
    slug: 'getting-started',
    source: 'getting-started',
    label: 'Getting started',
    group: 'Start here',
    description:
      'Build Octomus, prepare a dedicated host, connect your repository, and run your first audit.'
  },
  {
    slug: 'configuration',
    source: 'configuration',
    label: 'Configuration',
    group: 'Start here',
    description:
      'Set repository details, verification commands, model routes, and operating limits.'
  },
  {
    slug: 'model-routing',
    source: 'model-routing',
    label: 'Model routing',
    group: 'Run Octomus',
    description:
      'Choose explicit Codex and OpenCode models for discovery, review, execution, and repair.'
  },
  {
    slug: 'deployment',
    source: 'deployment',
    label: 'Deployment & operations',
    group: 'Run Octomus',
    description:
      'Operate Octomus on a dedicated host with systemd, private access, recovery, and backups.'
  },
  {
    slug: 'cost',
    source: 'cost',
    label: 'Usage & costs',
    group: 'Run Octomus',
    description: 'Understand session admissions, usage reports, and the limits of cost measurement.'
  },
  {
    slug: 'architecture',
    source: 'architecture',
    label: 'Architecture',
    group: 'Under the hood',
    description:
      'How discovery, independent review, execution, verification, and PR delivery fit together.'
  },
  {
    slug: 'run-evidence',
    source: 'run-evidence',
    label: 'Run evidence',
    group: 'Under the hood',
    description:
      'Read the RunEvidenceV1 export and understand recorded outcomes, provenance, and limitations.'
  },
  {
    slug: 'threat-model',
    source: 'threat-model',
    label: 'Security & trust',
    group: 'Under the hood',
    description:
      'Understand dedicated-host trust boundaries, repository instructions, and credential handling.'
  },
  {
    slug: 'showcase',
    source: 'showcase',
    label: 'Public showcase',
    group: 'Project',
    description: 'Build a standalone, read-only explorer from an explicit public evidence payload.'
  },
  {
    slug: 'releasing',
    source: 'releasing',
    label: 'Releases',
    group: 'Project',
    description: 'Build, package, and verify Octomus release artifacts and distribution.'
  },
  {
    slug: 'product-hunt',
    source: 'product-hunt',
    label: 'Product Hunt launch',
    group: 'Project',
    description:
      'Product Hunt listing copy, gallery assets, and the public Cloudflare Workers site.'
  }
];

/** @param {string} value */
const escape = (value) =>
  value.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
/** @param {(typeof documents)[number]} doc */
const docPath = (doc) => `docs/${doc.slug ? `${doc.slug}/` : ''}`;

/** @param {string} rootHref @param {string} current */
function navigation(rootHref, current) {
  return [...new Set(documents.map((doc) => doc.group))]
    .map(
      (group) => `
    <div class="docs-nav-group"><p>${group}</p><ul>${documents
      .filter((doc) => doc.group === group)
      .map(
        (doc) => `
      <li><a href="${rootHref}${docPath(doc)}"${doc.slug === current ? ' aria-current="page"' : ''}>${escape(doc.label)}</a></li>
    `
      )
      .join('')}</ul></div>
  `
    )
    .join('');
}

/** @param {string} rootHref */
function masthead(rootHref) {
  return `<a class="skip" href="#main">Skip to content</a>
    <header class="masthead wrap">
      <a class="brand" href="${rootHref}" aria-label="Octomus home"><img src="/favicon.svg" width="40" height="40" alt="" />octomus<span class="preview-tag">preview</span></a>
      <nav aria-label="Main navigation">
        <a href="${rootHref}#how-it-works">How it works</a>
        <a href="${rootHref}showcase/">The sample</a>
        <a href="${rootHref}docs/" aria-current="true">Docs</a>
        <a class="nav-github" href="${repository}">GitHub <span aria-hidden="true">↗</span></a>
      </nav>
    </header>`;
}

/** @param {string} rootHref */
function footer(rootHref) {
  return `<footer class="footer wrap docs-footer">
    <a class="brand" href="${rootHref}"><img src="/favicon.svg" width="32" height="32" alt="" />octomus</a>
    <p>A few extra arms. You keep the final say.</p>
    <div><a href="${repository}">Source</a><a href="${rootHref}docs/">Docs</a><a href="${repository}/issues">Feedback</a></div>
  </footer>`;
}

/** @param {string} title @param {string} description @param {string} path */
function head(title, description, path) {
  return `<meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <meta name="theme-color" content="#123e36" />
    <meta name="referrer" content="strict-origin-when-cross-origin" />
    <meta http-equiv="Content-Security-Policy" content="default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; font-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'" />
    <title>${escape(title)} · Octomus Docs</title>
    <meta name="description" content="${escape(description)}" />
    <link rel="canonical" href="${siteOrigin}/${path}" />
    <meta property="og:type" content="website" />
    <meta property="og:site_name" content="Octomus" />
    <meta property="og:title" content="${escape(title)} · Octomus Docs" />
    <meta property="og:description" content="${escape(description)}" />
    <meta property="og:url" content="${siteOrigin}/${path}" />
    <meta property="og:image" content="${siteOrigin}/social-card.png" />
    <meta name="twitter:card" content="summary_large_image" />
    <link rel="icon" type="image/svg+xml" href="/favicon.svg" />
    <link rel="stylesheet" href="/style.css" />
    <link rel="stylesheet" href="/docs.css" />`;
}

/** @param {string} root @param {string} sourceDir @param {string} publicDir */
export async function buildDocs(root, sourceDir, publicDir) {
  /** @type {{ title: string, section: string, href: string, text: string }[]} */
  const search = [];
  /** @type {string[]} */
  const inputs = [];
  const paths = new Map(documents.map((doc) => [`docs/${doc.source}.md`, docPath(doc)]));
  paths.set('docs/configuration.example.json', 'docs/configuration.example.json');
  const groups = [...new Set(documents.map((doc) => doc.group))];

  for (const [index, doc] of documents.entries()) {
    const markdown = await readFile(resolve(root, `docs/${doc.source}.md`), 'utf8');
    const rootHref = doc.slug ? '../../' : '../';
    const slugger = new GithubSlugger();
    /** @type {{id: string, title: string, depth: number}[]} */
    const headings = [];
    /** @param {string} href */
    const publicLink = (href) => {
      if (href.startsWith(`${siteOrigin}/`)) return rootHref + href.slice(siteOrigin.length + 1);
      if (/^(?:[a-z]+:|\/\/|#)/i.test(href)) return href;
      const [file, fragment] = href.split('#');
      const source = posix.normalize(posix.join('docs', file));
      const local = paths.get(source);
      return `${local ? rootHref + local : `${repository}/blob/main/${source}`}${fragment ? `#${fragment}` : ''}`;
    };
    const parser = new Marked({
      renderer: {
        heading({ depth, tokens }) {
          const content = this.parser.parseInline(tokens);
          const title = content.replace(/<[^>]*>/g, '');
          const id = slugger.slug(title);
          headings.push({ id, title, depth });
          return `<h${depth} id="${escape(id)}">${content}${depth > 1 ? ` <a class="heading-anchor" href="#${escape(id)}" aria-label="Link to ${escape(title)}">#</a>` : ''}</h${depth}>\n`;
        },
        link({ href, title, tokens }) {
          return `<a href="${escape(publicLink(href))}"${title ? ` title="${escape(title)}"` : ''}>${this.parser.parseInline(tokens)}</a>`;
        },
        table(token) {
          /** @param {import('marked').Tokens.TableCell[]} cells @param {boolean} header */
          const row = (cells, header) =>
            `<tr>${cells.map((cell) => `<${header ? 'th scope="col"' : 'td'}>${this.parser.parseInline(cell.tokens)}</${header ? 'th' : 'td'}>`).join('')}</tr>`;
          return `<div class="table-scroll" role="region" aria-label="Reference table" tabindex="0"><table><thead>${row(token.header, true)}</thead><tbody>${token.rows.map((cells) => row(cells, false)).join('')}</tbody></table></div>`;
        }
      }
    });
    const body = await parser.parse(markdown);
    // Search sections link directly to their generated heading, using the same IDs.
    let section = '';
    let href = docPath(doc);
    let content = '';
    let headingIndex = 0;
    const addSection = () => {
      if (content.trim())
        search.push({
          title: doc.label,
          section,
          href,
          text: content
            .replace(/\[([^\]]+)\]\([^)]+\)/g, '$1')
            .replace(/[`*#\[\]<>]/g, '')
            .replace(/\s+/g, ' ')
            .trim()
        });
    };
    for (const token of parser.lexer(markdown)) {
      if (token.type === 'heading') {
        addSection();
        const heading = headings[headingIndex++];
        section = token.depth === 1 ? '' : heading.title;
        href = docPath(doc) + (token.depth === 1 ? '' : `#${heading.id}`);
        content = '';
      } else content += token.raw + '\n';
    }
    addSection();
    const toc = headings.filter((heading) => heading.depth === 2);
    const previous = documents[index - 1];
    const next = documents[index + 1];
    const output = resolve(sourceDir, docPath(doc), 'index.html');
    await mkdir(resolve(sourceDir, docPath(doc)), { recursive: true });
    await writeFile(
      output,
      `<!doctype html>
<html lang="en"><head>${head(doc.label, doc.description, docPath(doc))}<script type="module" src="/docs.ts"></script></head>
<body class="docs-page">${masthead(rootHref)}
  <div class="docs-topbar wrap">
    <p><a href="${rootHref}docs/">Documentation</a><span aria-hidden="true"> / </span>${escape(doc.label)}</p>
    <div class="docs-search" hidden data-root="${rootHref}">
      <label for="docs-search">Search the docs</label>
      <div class="search-field"><span aria-hidden="true">⌕</span><input id="docs-search" type="search" placeholder="Search the docs…" autocomplete="off" aria-controls="search-results" /><kbd>/</kbd></div>
      <div class="search-panel" hidden><p class="search-status" role="status"></p><ul id="search-results"></ul></div>
    </div>
  </div>
  <details class="docs-mobile-nav wrap"><summary>Browse documentation <span aria-hidden="true">↓</span></summary><nav aria-label="Mobile documentation">${navigation(rootHref, doc.slug)}</nav></details>
  <div class="docs-layout wrap">
    <aside class="docs-sidebar"><nav aria-label="Documentation">${navigation(rootHref, doc.slug)}</nav><a class="sidebar-sample" href="${rootHref}showcase/"><span aria-hidden="true">✳</span> Curious first?<strong>Explore a sample run ↗</strong></a></aside>
    <main id="main" class="docs-article" tabindex="-1">
      <p class="doc-kicker"><span>${escape(doc.group)}</span><span>${String(groups.indexOf(doc.group) + 1).padStart(2, '0')} / FIELD GUIDE</span></p>
      <article class="prose">${body}</article>
      <div class="doc-source"><span>Something unclear?</span><a href="${repository}">Contribute on GitHub ↗</a></div>
      <nav class="doc-pagination" aria-label="Adjacent pages">
        ${previous ? `<a href="${rootHref}${docPath(previous)}"><span>← Previous</span><strong>${escape(previous.label)}</strong></a>` : '<div></div>'}
        ${next ? `<a href="${rootHref}${docPath(next)}"><span>Next →</span><strong>${escape(next.label)}</strong></a>` : ''}
      </nav>
    </main>
    <aside class="docs-toc"><nav aria-label="On this page"><p>ON THIS PAGE</p><ul>${toc.map((heading) => `<li><a href="#${escape(heading.id)}">${heading.title}</a></li>`).join('')}</ul></nav><a class="back-top" href="#main">Back to top ↑</a></aside>
  </div>${footer(rootHref)}
</body></html>`
    );
    inputs.push(output);
  }
  await mkdir(resolve(publicDir, 'docs'), { recursive: true });
  await writeFile(resolve(publicDir, 'docs/search-index.json'), JSON.stringify(search));
  await writeFile(
    resolve(publicDir, 'sitemap.xml'),
    `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">${['', ...documents.map(docPath)].map((path) => `<url><loc>${siteOrigin}/${path}</loc></url>`).join('')}</urlset>\n`
  );
  // Absolute links also work when this document is served at an unknown nested URL.
  const missing = resolve(sourceDir, '404.html');
  await writeFile(
    missing,
    `<!doctype html><html lang="en"><head>${head('Page not found', 'Find your way back to Octomus.', '404')}<meta name="robots" content="noindex" /></head><body>${masthead('/')}<main id="main" class="not-found wrap"><p class="eyebrow">404 / A LITTLE OFF COURSE</p><h1>This arm doesn’t<br />reach that far.</h1><p>The page may have moved. Let’s get you back to something useful.</p><div class="actions"><a class="button primary" href="/docs/">Explore the docs →</a><a class="button secondary" href="/">Back to the homepage</a></div></main>${footer('/')}</body></html>`
  );
  inputs.push(missing);
  return inputs;
}
