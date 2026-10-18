# Panel screenshots

Capture the README screenshots from the panel's built-in demo fleet
(`panel/demo.html`). No master, database or live fleet is needed.

```bash
cd panel
npm ci
cd ../tools/panelshots
npm ci
npx playwright install chromium
npm run shoot
```

The output goes to `docs/images/`:

| File | Content |
|---|---|
| `panel-overview.png` | Full overview: matches, players, warm buffer and events. |
| `panel-deploys.png` | Full deploys screen, including the version table. |
| `panel-stats.png` | Full statistics screen with 24-hour metrics and server states. |
| `panel-deploys-preview.png` | Versions, regions and pre-pull progress at a phone width. |
| `panel-stats-preview.png` | Players and matches charts stacked at a phone width. |

To refresh only the two previews while keeping the full screenshots:

```bash
npm run shoot:previews
```

## Capture conventions

- **Keep feature sections in one column.** A text-and-image table stays in two
  columns on GitHub Mobile, producing narrow text and a tiny image. Each section
  uses a heading, a short paragraph and a preview underneath.
- **Use real responsive layouts.** Previews are captured directly from the demo
  panel at a 392 px viewport width, at 2x pixel density. They do not redraw the
  interface or change its data. The same focused previews work on every screen
  without custom CSS or media queries. Each preview links to the full screenshot.
- **Keep Playwright outside `panel/`.** Panel dependencies are installed in CI
  and Docker builds; screenshot tooling and browser downloads belong here.
- **The data is synthetic.** Keep the demo-data disclosure directly below the
  first screenshot in every README that uses these images.
- **The shape is reproducible, the bytes are not.** Fixtures are deterministic,
  but metric curves follow the time of day. Values and timestamps can change.
- **Capture manually.** Screenshots are not regenerated in CI. If a screen label
  changes, update the readiness markers and preview locators in `shoot.mjs`.
