# AGENTS.md

Instructions for AI coding agents working on Agen's docs site, built on
[Trestle](https://github.com/prjvvl/trestle). Read this before making changes.

## Where things live

- **Content** — the repository's `docs/**/*.md`, read in place (the same files
  render on GitHub and are checked by the Go docs tests). Write links as
  relative `.md` paths; `src/lib/remark-doc-links.mjs` turns them into site
  routes. A new page appears in the sidebar once it is listed in
  `src/site.config.ts` `sidebar`.
- **Content schema** — `src/content.config.ts`. Frontmatter is optional; the
  title comes from the first `# ` heading.
- **Site metadata, nav, feature flags** — `src/site.config.ts`. One file, not
  scattered config.
- **Brand colors, fonts, spacing** — `src/styles/global.css`, inside the
  `@theme` block. This is Tailwind v4's CSS-first config — do not create a
  `tailwind.config.js`, it won't be picked up.
- **Reusable UI** — `src/components/*.astro` (Nav, Footer, Hero, Card,
  Button). Reuse these instead of writing new one-off markup for common
  patterns like a card grid or a CTA button. A React island that needs the
  same look (Card.astro and Button.astro can't be imported into React) can
  import the class strings from `src/lib/styles.ts` instead of copying
  Tailwind classes by hand.
- **Pages** — `src/pages/*.astro`. File-based routing.
- **Deploy** — the repository's `.github/workflows/pages.yml` builds this
  site on every PR and deploys it from `main`.

## Rules

- **Don't hardcode colors, fonts, or spacing values in components.** Use the
  Tailwind utility classes that map to the tokens in `global.css`
  (`bg-brand`, `bg-brand-100`...`bg-brand-900`, `text-fg`, `border-border`,
  `bg-surface`, `rounded-card`, `font-display`, semantic `text-success` /
  `text-error` / `text-warning` / `text-info`, etc.), not raw hex codes or
  arbitrary pixel values. This is what keeps a site visually consistent as
  it grows. Headings (`h1`–`h4`) automatically get `--font-display`
  (Space Grotesk) — body text stays on `--font-sans` (Inter); don't override
  this per-component.
- **Never hardcode an internal `href="/..."`.** GitHub Pages without a
  custom domain serves the site at a subpath (`username.github.io/repo/`),
  which Astro's `base` config handles — but plain `<a href="/blog">` isn't
  auto-prefixed. Use `withBase("/blog")` from `src/lib/url.ts` for any
  internal link, or go through `<Button>` / `<Card>` (both already apply it
  automatically for any `href` starting with `/`). External URLs need no
  change. Forgetting this is a silent bug, not a build error — the site
  still builds, links just 404 once deployed without a custom domain.
- **Elevation/hover on interactive surfaces**: use the `hover-elevate` CSS
  class (defined in `global.css`), not ad hoc `hover:shadow-*` utilities —
  `box-shadow` is nearly invisible on a dark background, so `hover-elevate`
  switches to a background/border brightness shift in dark mode instead.
- **Card usage**: only pass `href` to `<Card>` when it's genuinely
  clickable. An `href`-less Card renders as a plain informational block
  (no border/background) rather than a bordered box — styling every content
  block identically, whether clickable or not, is one of the most common
  "this is a generic template" tells.
- **Any image or embed must set an explicit `width`/`height` or
  `aspect-ratio`.** The browser needs to reserve the right amount of space
  before the file loads — an unsized image is one of the most common causes
  of content jumping around as a page loads. Prefer Astro's `<Image />`
  (`astro:assets`) for local images, which enforces this automatically.
- **Don't edit `astro.config.mjs`'s CNAME logic.** It reads `siteConfig.domain`
  automatically — set the domain in `site.config.ts`, don't write a CNAME
  file by hand or duplicate the logic elsewhere.
- **Content that fails to build is a schema mismatch, not a bug to route
  around.** If `npm run build` reports an `InvalidContentEntryDataError`, fix
  the frontmatter in the named file (or the schema, if the field genuinely
  needs to change) — don't loosen the schema to `z.any()` to make the error
  go away.
- **Need interactivity?** Drop a React component into a page or inside a
  component's `<slot />` and hydrate it with a `client:*` directive (e.g.
  `client:load`). Don't reach for a client-side framework beyond React —
  `@astrojs/react` is the only UI framework integration installed.
- **Dark/light theme**: dark is the default palette; light applies only
  when the system explicitly prefers it, or the visitor toggles it via
  `ThemeToggle.astro` (persisted in localStorage, no "system" option in
  the toggle itself). Don't reach for Tailwind's built-in `dark:` variant,
  it only checks `prefers-color-scheme` and ignores the explicit toggle.
  Use the `--color-*` tokens directly (they already respond to both
  triggers) or, for a new light-specific style, follow the pattern in
  `global.css`: duplicate the rule once under
  `@media (prefers-color-scheme: light) { :root:not([data-theme="dark"]) ... }`
  and once under `:root[data-theme="light"] ...`.
- **Building the contact form or anything else with validation**: validate
  each field on blur (not on every keystroke — that's worse, not better),
  plus a final check on submit that also summarizes remaining errors. Use
  `text-error` / `bg-error-bg` (and the matching `-success`/`-warning`/
  `-info` tokens) for state, not ad hoc colors — they're defined in
  `global.css` specifically so nobody has to invent them per-form. Use
  `<Button loading={...}>` (shows a spinner, disables the button) to
  prevent double-submit during the async request.
- **Building anything that loads content asynchronously** (search results,
  etc.): use a skeleton placeholder shaped like the real content, not a
  spinner — it avoids the layout jumping when the content arrives, same
  reasoning as the image-sizing rule above. A spinner is only correct for a
  single action with no shape to preview (a button's own pending state).
- **Before finishing a change**, run `npm run check` (type-check) and
  `npm run build` — both must pass with zero errors. The CI workflow runs the
  same two commands on every PR.
