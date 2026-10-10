import { BookOpen, Download, Library, Lightbulb, Rocket } from "@lucide/astro";

/** Icons referenced by name from site.config.ts (nav items, sidebar groups),
 *  so the config stays plain data. */
export const icons = {
  install: Download,
  start: Rocket,
  guides: BookOpen,
  reference: Library,
  explanation: Lightbulb,
} as const;

export type IconName = keyof typeof icons;
