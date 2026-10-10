// @ts-check
import { defineConfig } from "astro/config";
import { fileURLToPath } from "node:url";
import { writeFileSync } from "node:fs";

import react from "@astrojs/react";
import sitemap from "@astrojs/sitemap";
import tailwindcss from "@tailwindcss/vite";

import { siteConfig } from "./src/site.config.ts";
import { satteri } from "@astrojs/markdown-satteri";
import { docLinks } from "./src/lib/doc-links.mjs";

/**
 * Writes dist/CNAME from siteConfig.domain on every build, so a custom domain
 * survives rebuilds instead of being silently dropped by hand.
 */
function cnameIntegration() {
  return {
    name: "agen-cname",
    hooks: {
      "astro:build:done": (/** @type {{ dir: URL }} */ { dir }) => {
        if (!siteConfig.domain) return;
        writeFileSync(fileURLToPath(new URL("CNAME", dir)), siteConfig.domain);
      },
    },
  };
}

// `site` must be the origin only, and `base` the path prefix; conflating
// them breaks every internal link, the favicon/manifest/OG image, and the
// sitemap once someone deploys without a custom domain. A GitHub Pages
// project site with no custom domain is served at
// https://<user>.github.io/<repo>/, so the "/<repo>" part has to be
// Astro's `base`, not baked into `site`.
const usingCustomDomain = Boolean(siteConfig.domain);
const fallbackURL = new URL(siteConfig.url);
const base = usingCustomDomain ? "/" : fallbackURL.pathname;

export default defineConfig({
  site: usingCustomDomain ? `https://${siteConfig.domain}` : fallbackURL.origin,
  base,
  trailingSlash: "always",
  integrations: [react(), sitemap(), cnameIntegration()],
  markdown: {
    processor: satteri({
      mdastPlugins: [
        docLinks({
          docsRoot: fileURLToPath(new URL("../docs", import.meta.url)),
          repoRoot: fileURLToPath(new URL("..", import.meta.url)),
          base,
          repo: siteConfig.repo,
        }),
      ],
    }),
  },
  vite: {
    plugins: [tailwindcss()],
  },
});
