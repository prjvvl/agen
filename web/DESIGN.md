# Console design

The console is where an operator answers two questions within seconds:
**what is my fleet doing, and does anything need me?** Then it lets them dig
from any summary down to the exact model call or tool call behind it.

## Principles

- **Attention first.** Pending approvals, failures and budgets that ran out
  come before anything else on the overview and in the inbox. Each item says
  what to do next.
- **Every number links to its evidence.** A failure count opens the failed
  runs; a run opens its trace; a span opens the messages and arguments behind
  it.
- **Live, without jumping.** Updates arrive over the event stream
  (`/api/v1/events`). Rows update in place; nothing reorders under the
  pointer.
- **One accent, used for meaning.** Indigo marks the current place and the
  primary action. Status colours (green, amber, red, blue) only mark status
  and always come with an icon and a word.
- **Tables and panels, not cards.** Lists are tables with hairline rows;
  details open in a side panel so the list stays in view. Modals are not used.
- **Quiet chrome.** No gradients, glows or decorative charts. Charts show
  real counts over time and say what they count.

## Look

Shared with the docs site ([Trestle](https://prjvvl.github.io/trestle/)):

- Inter for text, Space Grotesk for the wordmark and page titles,
  JetBrains Mono only for ids, code, JSON and logs. Numbers use tabular
  figures.
- Dark navy by default, a light theme that follows the system or the
  toggle. Colours are tokens in `src/styles/tokens.css`; components never
  hard-code a colour.
- Type scale: 12, 13, 14 (body), 16, 20, 24 px. Spacing on a 4 px grid.
- Corners 6 px on controls, 8 px on panels. Borders, not shadows, separate
  surfaces; the side panel alone has a shadow.
- Motion only for live activity (a pulse on an active edge or a running
  status) and panel entry, 150 ms, off under `prefers-reduced-motion`.

## Every view

- Loading, empty, error and live states. Empty states say how to get data.
- Full keyboard use: visible focus, `Ctrl+K` to jump anywhere, `Esc` closes
  panels, arrow keys move through the trace.
- Works at 200 % zoom and at phone width (tables scroll inside their frame,
  the side panel becomes full screen).
- Long names and ids truncate with the full value in a tooltip and a copy
  button.
