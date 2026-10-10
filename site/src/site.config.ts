/**
 * Single source of truth for site metadata, navigation, and feature toggles.
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

  nav: [
    { label: "Get started", href: "/getting-started/" },
    { label: "Guides", href: "/guides/tools/" },
    { label: "Reference", href: "/reference/bundle/" },
    { label: "GitHub", href: "https://github.com/prjvvl/agen" },
  ],

  /** The docs sidebar. Pages are files in the repository's docs/ directory,
   *  addressed by their path without `.md`. */
  sidebar: [
    { group: "Start here", pages: ["getting-started", "concepts"] },
    {
      group: "Guides",
      pages: [
        "guides/tools",
        "guides/agents",
        "guides/permissions",
        "guides/budgets",
        "guides/triggers",
        "guides/memory",
        "guides/mcp",
        "deploy",
        "security",
        "troubleshooting",
      ],
    },
    { group: "Reference", pages: ["reference/bundle", "reference/cli", "reference/api", "reference/sdks", "architecture"] },
  ],

  features: {
    search: false,
    comments: false,
    contactForm: false,
  },
} as const;
