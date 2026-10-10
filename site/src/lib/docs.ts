import { getCollection, type CollectionEntry } from "astro:content";
import { siteConfig } from "../site.config";

export type Doc = CollectionEntry<"docs">;

/** The page title: frontmatter, else the first `# ` heading, else the id. */
export function docTitle(doc: Doc): string {
  return doc.data.title ?? doc.body?.match(/^#\s+(.+)$/m)?.[1]?.trim() ?? doc.id;
}

/** The first paragraph of prose, without Markdown, for the meta description. */
export function docDescription(doc: Doc): string {
  if (doc.data.description) return doc.data.description;
  let inFence = false;
  const para: string[] = [];
  for (const line of (doc.body ?? "").split("\n")) {
    const t = line.trim();
    if (t.startsWith("```")) inFence = !inFence;
    if (inFence || t.startsWith("```")) continue;
    if (!t || /^(#|\||<|-|\*|\d+\.|>)/.test(t)) {
      if (para.length) break;
      continue;
    }
    para.push(t);
  }
  const text = para
    .join(" ")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/[`*_]/g, "");
  return text.length > 200 ? `${text.slice(0, 197).trimEnd()}...` : text || siteConfig.description;
}

export async function allDocs(): Promise<Doc[]> {
  return getCollection("docs");
}

export interface SidebarGroup {
  group: string;
  items: { id: string; title: string }[];
}

export function sidebar(docs: Doc[]): SidebarGroup[] {
  const byId = new Map(docs.map((d) => [d.id, d]));
  return siteConfig.sidebar.map(({ group, pages }) => ({
    group,
    items: pages.flatMap((id) => {
      const doc = byId.get(id);
      return doc ? [{ id, title: docTitle(doc) }] : [];
    }),
  }));
}

/** The pages before and after `id` in sidebar order. */
export function neighbours(groups: SidebarGroup[], id: string) {
  const flat = groups.flatMap((g) => g.items);
  const i = flat.findIndex((p) => p.id === id);
  return { prev: i > 0 ? flat[i - 1] : undefined, next: i >= 0 ? flat[i + 1] : undefined };
}
