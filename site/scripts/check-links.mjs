// Checks the built site (dist/): every internal link must reach a built page
// or file, and every #anchor must exist on its target page. Links to the
// repository on GitHub must name a file or directory that exists in it.
// Run after `npm run build`.
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const config = readFileSync(new URL("../src/site.config.ts", import.meta.url), "utf8");
const siteConfig = {
  url: config.match(/^\s*url: "([^"]+)"/m)[1],
  repo: config.match(/^\s*repo: "([^"]+)"/m)[1],
};
const dist = fileURLToPath(new URL("../dist/", import.meta.url));
const repoRoot = fileURLToPath(new URL("../../", import.meta.url));
const base = new URL(siteConfig.url).pathname.replace(/\/$/, "");
const repoPrefix = `${siteConfig.repo}/`;

const pages = new Map();
for (const file of walk(dist)) {
  if (file.endsWith(".html")) pages.set(file, readFileSync(file, "utf8"));
}

const ids = (html) => new Set([...html.matchAll(/\sid="([^"]+)"/g)].map((m) => m[1]));
const problems = [];
let checked = 0;

for (const [file, html] of pages) {
  const from = path.relative(dist, file);
  for (const [, href] of html.matchAll(/\shref="([^"]+)"/g)) {
    if (href.startsWith(repoPrefix)) {
      const m = href.slice(repoPrefix.length).match(/^(?:blob|tree)\/main\/([^#?]*)/);
      if (m && !existsSync(path.join(repoRoot, decodeURIComponent(m[1])))) problems.push(`${from}: ${href} (no such file in the repository)`);
      checked++;
      continue;
    }
    if (/^[a-z]+:/i.test(href) || href.startsWith("//")) continue;
    let [target, anchor] = href.split("#");
    if (target === "") target = "/" + path.relative(dist, path.dirname(file)).split(path.sep).join("/") + "/";
    else if (!target.startsWith(base + "/")) {
      problems.push(`${from}: ${href} (not under ${base}/)`);
      continue;
    } else target = target.slice(base.length);
    let resolved = path.join(dist, decodeURIComponent(target));
    if (target.endsWith("/")) resolved = path.join(resolved, "index.html");
    checked++;
    if (!existsSync(resolved)) {
      problems.push(`${from}: ${href} (no such page)`);
      continue;
    }
    if (anchor && resolved.endsWith(".html") && !ids(pages.get(resolved) ?? "").has(decodeURIComponent(anchor))) {
      problems.push(`${from}: ${href} (no #${anchor} on that page)`);
    }
  }
}

if (problems.length) {
  console.error(problems.join("\n"));
  console.error(`${problems.length} broken link(s)`);
  process.exit(1);
}
console.log(`${checked} links checked, all fine`);

function* walk(dir) {
  for (const name of readdirSync(dir)) {
    const p = path.join(dir, name);
    if (statSync(p).isDirectory()) yield* walk(p);
    else yield p;
  }
}
