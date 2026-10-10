/**
 * Single source of truth for site metadata and navigation.
 * Brand colors and fonts live in src/styles/global.css (the @theme block).
 * Tailwind v4's config is CSS-first, so tokens stay there, not here.
 */
export const siteConfig = {
  name: "Agen",
  description: "One agent engine, embedded in your app or run as a fleet.",

  /** Set this to a custom domain (e.g. "example.com") to enable one; it is
   *  written to dist/CNAME on every build. Empty: served at `url`. */
  domain: "",

  url: "https://prjvvl.github.io/agen",

  repo: "https://github.com/prjvvl/agen",

  social: {
    github: "https://github.com/prjvvl/agen",
  },

  /** Header links. `icon` names an entry in src/lib/icons.ts; `match` lists
   *  the route prefixes that mark the item as the current section. */
  nav: [
    { label: "Install", href: "/install/", icon: "install", match: ["/install/"] },
    { label: "Get started", href: "/getting-started/", icon: "start", match: ["/getting-started/", "/embed/"] },
    {
      label: "Guides",
      href: "/guides/tools/",
      icon: "guides",
      match: ["/guides/", "/deploy/", "/troubleshooting/"],
    },
    { label: "Reference", href: "/reference/bundle/", icon: "reference", match: ["/reference/"] },
  ],

  /** The docs sidebar. Pages are files in the repository's docs/ directory,
   *  addressed by their path without `.md`. */
  sidebar: [
    { group: "Start here", icon: "start", pages: ["install", "getting-started", "embed"] },
    {
      group: "How-to guides",
      icon: "guides",
      pages: [
        "guides/console",
        "guides/traces",
        "guides/templates",
        "guides/tools",
        "guides/agents",
        "guides/permissions",
        "guides/budgets",
        "guides/notifications",
        "guides/triggers",
        "guides/memory",
        "guides/mcp",
        "deploy",
        "troubleshooting",
      ],
    },
    { group: "Reference", icon: "reference", pages: ["reference/bundle", "reference/cli", "reference/api", "reference/sdks"] },
    { group: "Explanation", icon: "explanation", pages: ["concepts", "architecture", "security", "development"] },
  ],
} as const;
