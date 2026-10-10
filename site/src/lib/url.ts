/**
 * Prefixes an absolute internal path (must start with "/") with Astro's
 * configured `base`, required because GitHub Pages serves a repo without a
 * custom domain at a subpath (username.github.io/repo-name/). Astro does
 * not auto-prefix hardcoded href="/..." strings, only its own generated
 * routes and asset imports. External URLs should never be passed through
 * this.
 */
export function withBase(path: string): string {
  return import.meta.env.BASE_URL.replace(/\/$/, "") + path;
}
