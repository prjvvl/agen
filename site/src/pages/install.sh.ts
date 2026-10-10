import type { APIRoute } from "astro";
import script from "../../../scripts/install.sh?raw";

// Served at <site>/install.sh, so the one-line install command never changes.
export const GET: APIRoute = () => new Response(script, { headers: { "Content-Type": "text/plain; charset=utf-8" } });
