/**
 * Shared Tailwind class strings for patterns that need to look identical in
 * both Astro components and React islands. Astro components can't be
 * imported into React, so a React island that needs a card or button
 * (like TagFilter) can't reuse Card.astro/Button.astro directly; importing
 * these avoids copy-pasting the class string a second time, where the two
 * copies can silently drift apart.
 */
export const cardClickableClass =
  "hover-elevate block rounded-card border border-border bg-surface p-6 hover:border-brand-300";

export function pillClass(active: boolean): string {
  return `min-h-11 rounded-card px-3 text-sm transition-colors duration-200 ${
    active
      ? "bg-brand text-brand-fg"
      : "border border-border text-fg-muted hover:border-brand-300 hover:text-fg"
  }`;
}

const buttonBaseClass =
  "hover-elevate inline-flex min-h-11 items-center justify-center gap-2 rounded-card px-5 py-2.5 text-sm font-medium";

export const buttonVariantClass = {
  primary: `${buttonBaseClass} bg-brand text-brand-fg hover:bg-brand-700`,
  secondary: `${buttonBaseClass} border border-border text-fg hover:border-brand-300 hover:bg-surface`,
} as const;
