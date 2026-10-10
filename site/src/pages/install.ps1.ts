import type { APIRoute } from "astro";
import script from "../../../scripts/install.ps1?raw";

// Served at <site>/install.ps1, so the one-line install command never changes.
export const GET: APIRoute = () => new Response(script, { headers: { "Content-Type": "text/plain; charset=utf-8" } });
