import type { APIRoute } from "astro";
import { withBase } from "../lib/url";

export const GET: APIRoute = ({ site }) => {
  const sitemapURL = new URL(withBase("/sitemap-index.xml"), site);
  return new Response(`User-agent: *\nAllow: /\n\nSitemap: ${sitemapURL}\n`, {
    headers: { "Content-Type": "text/plain" },
  });
};
