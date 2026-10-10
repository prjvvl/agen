// Rewrites relative links in the docs, which are written for GitHub
// (`guides/tools.md#retries`, `../reference/api.md`), to site routes
// (`<base>/guides/tools/#retries`). Relative links that leave docs/, or that
// are not Markdown pages, point at the file on GitHub instead.
import path from "node:path";
import { fileURLToPath } from "node:url";

/**
 * A Sätteri mdast plugin factory (it needs each document's path).
 * @param {{ docsRoot: string, repoRoot: string, base: string, repo: string }} options
 */
export function docLinks({ docsRoot, repoRoot, base, repo }) {
  const prefix = base.replace(/\/$/, "");

  /** @param {string} url @param {string} from */
  function rewrite(url, from) {
    if (/^[a-z][a-z0-9+.-]*:/i.test(url) || url.startsWith("#") || url.startsWith("/")) return url;
    const [target, hash = ""] = url.split("#");
    const anchor = hash ? `#${hash}` : "";
    const abs = path.resolve(from, target);
    const inDocs = path.relative(docsRoot, abs).split(path.sep).join("/");
    if (inDocs.startsWith("..") || !target.endsWith(".md")) {
      const inRepo = path.relative(repoRoot, abs).split(path.sep).join("/");
      return `${repo}/${path.extname(target) ? "blob" : "tree"}/main/${inRepo}${anchor}`;
    }
    const route = inDocs.replace(/\.md$/, "").replace(/(^|\/)index$/, "");
    return `${prefix}/${route}${route ? "/" : ""}${anchor}`;
  }

  /** @param {{ fileURL: URL | undefined }} doc */
  return (doc) => {
    if (!doc.fileURL) return null;
    const from = path.dirname(fileURLToPath(doc.fileURL));
    const fix = (node, ctx) => {
      const url = rewrite(node.url, from);
      if (url !== node.url) ctx.setProperty(node, "url", url);
    };
    return { name: "agen-doc-links", link: fix, definition: fix };
  };
}
