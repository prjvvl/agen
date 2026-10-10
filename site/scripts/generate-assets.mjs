// One-off asset generator: rasterizes the SVG favicon into the PNG sizes
// needed for a web app manifest + apple-touch-icon, and builds a default
// OG share image. Run with `node scripts/generate-assets.mjs` whenever the
// mark or brand color changes. Not part of the build, output is committed.
import sharp from "sharp";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const favicon = readFileSync(`${root}/public/favicon.svg`);

for (const size of [192, 512]) {
  await sharp(favicon)
    .resize(size, size)
    .png()
    .toFile(`${root}/public/icon-${size}.png`);
}

// Apple touch icons expect a full-bleed square, no transparency.
await sharp(favicon)
  .resize(180, 180)
  .flatten({ background: "#2331c7" })
  .png()
  .toFile(`${root}/public/apple-touch-icon.png`);

const ogSvg = `
<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <rect width="1200" height="630" fill="#12163f" />
  <rect width="1200" height="630" fill="url(#glow)" />
  <defs>
    <radialGradient id="glow" cx="50%" cy="0%" r="75%">
      <stop offset="0%" stop-color="#3c42d9" stop-opacity="0.55" />
      <stop offset="100%" stop-color="#12163f" stop-opacity="0" />
    </radialGradient>
  </defs>
  <rect x="80" y="80" width="64" height="64" rx="16" fill="#2331c7" />
  <path d="M100 130 112 102l12 28M104.8 120h14.4" fill="none" stroke="#ffffff" stroke-width="6" stroke-linecap="round" stroke-linejoin="round" />
  <text x="80" y="260" font-family="Arial, sans-serif" font-size="88" font-weight="700" fill="#ffffff">Agen</text>
  <text x="80" y="330" font-family="Arial, sans-serif" font-size="34" fill="#c7cbf5">One agent engine, embedded in your app or run as a fleet</text>
</svg>
`;

await sharp(Buffer.from(ogSvg)).png().toFile(`${root}/public/og-image.png`);

console.log("Generated icon-192.png, icon-512.png, apple-touch-icon.png, og-image.png");
