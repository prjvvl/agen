import type { APIRoute } from "astro";
import { siteConfig } from "../site.config";
import { withBase } from "../lib/url";

export const GET: APIRoute = () => {
  return new Response(
    JSON.stringify({
      name: siteConfig.name,
      short_name: siteConfig.name,
      description: siteConfig.description,
      start_url: withBase("/"),
      display: "standalone",
      background_color: "#12163f",
      theme_color: "#2331c7",
      icons: [
        { src: withBase("/icon-192.png"), sizes: "192x192", type: "image/png" },
        { src: withBase("/icon-512.png"), sizes: "512x512", type: "image/png" },
      ],
    }),
    { headers: { "Content-Type": "application/manifest+json" } }
  );
};
