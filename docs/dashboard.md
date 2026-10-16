# Dashboard Guide

> [简体中文](dashboard.zh-CN.md) | English

This document covers the embedded dashboard served by `token-usage serve`: how to start it, what the Dashboard and Config pages do, privacy masking and screenshot sharing, and the HTTP API behind them. Source-code behavior takes precedence over this document.

## Starting the Dashboard

The dashboard always runs in the background, managed by the `serve` command group:

```bash
token-usage serve start --open
```

The server listens on `127.0.0.1:8619` by default (`--addr` changes it), and `--open` opens the page in your default browser once the background server is up. `serve status`, `serve stop`, `serve restart`, the state file, logs, and the single-instance contract are documented in the [`serve` section of the CLI Reference](cli.md#serve).

## Security Boundary

The HTTP surface is loopback-first, same-origin only (no CORS headers), and unauthenticated. The data endpoints and the page itself are strictly read-only; the only write path is `PUT /api/config`, which saves the user configuration through the same locked, revision-checked, atomic write chain as the CLI config commands, and configuration changes never touch the database. Binding a public address such as `0.0.0.0` exposes your usage data and local configuration editing to the local network — keep the server on the loopback interface (see [`--addr`](cli.md#serve)).

## The Dashboard Page

The embedded page is bilingual (Chinese/English, remembered per browser) with five accent palettes and light/dark themes (remembered per browser, dark follows the system by default; neither affects config dirty state). The Dashboard page renders everything client-side from the numeric rows: seven headline metrics that are independent of the output-column layout, the four built-in dimensions as two-column tables with per-card totals and a single trailing "other (N items)" row (plus a donut composition mode), a heat map that follows the selected range — one day as 24 hour cells, 2–7 days as date × hour, 8–31 days as date × 4-hour slots, 32+ days as a GitHub-style per-day calendar, with cells after today shown as blank ("not yet") rather than zero — a usage trend chart, custom-view tables derived from `query.subqueries` with a conservation-guaranteed "other combinations" row, and top sessions with full original titles (an info button opens the complete title).

Auto refresh, manual refresh, and range changes share one request entry and one loading/success/failure state (stale responses never overwrite the current range; a failed refresh clears stale stats into a full-page error), the interval coming from `[refresh].dashboard_interval`. Preset ranges plus a boundary-free custom calendar (future dates submit and show real empty results; the selection is remembered across same-tab reloads).

## The Config Page

The Config page edits the full user configuration with live dirty-state diffing against the last saved snapshot — including the two query refresh intervals (`[refresh] dashboard_interval`/`watch_interval`, dropdowns 10/20/30/60 seconds plus a custom 1–3600 value; untouched fields keep their user-layer value so an unrelated save never solidifies the implicit defaults) — with revision-conflict (409) and validation (400) reporting that never overwrite local edits, and restore-to-saved as a confirmed action.

## Privacy Masking and Screenshot Sharing

The Dashboard page has a privacy-mask toggle in the control bar. When masking is on, project names and session titles are displayed as `***` everywhere they appear — dimension rows, composition views, custom-view tables, and the session table. The aggregated "other" rows and the "(uncategorized)" placeholder are never masked, and the client, model, and provider dimensions are not masked. Masking is a display-layer treatment: the HTTP API responses and the raw data held in the browser remain plaintext, and nothing is rewritten in the database.

The mask state follows three rules:

- Masking is off by default and a page reload resets it — it is not persisted. Turning it on also closes any open full-title dialog.
- Auto refresh, manual refresh, and range changes keep the current mask state.
- The screenshot dialog starts from the page's current mask state; the toggle inside the dialog affects that image only and is not persisted.

The **Share screenshot** button opens a dialog that composes a full-page PNG:

- The image always includes the brand header, the selected date range, and the headline metrics. The remaining content is selectable: the dimension list and composition views, the trend and heat charts, each configured custom view (with select-all / clear-all), and the highest-usage sessions.
- Content selections are remembered per browser, separately from the mask state.
- The preview shown in the dialog and the image copied to the clipboard are the same PNG — what you preview is exactly what you paste.

## HTTP API

| Endpoint | Parameters | Returns |
|------|------|---------|
| `GET /api/meta` | — | version (as passed by the binary), `min_date`/`max_date` (whole database), `data_through`, `last_collection`; the last three are `null` when absent |
| `GET /api/dashboard` | `from`, `to` (`YYYY-MM-DD`; defaults to the 30 days ending today; span at most 366 days; future ranges return a well-formed all-zero payload) | `data_through`, `last_collection`, range, totals (integers, including `active_days`), `columns` (the configured output-column metric IDs — affects detail tables only, never the headline metrics), `trend` (`granularity` `hour` for a single day, `day` for 2–62 days, `week` for longer; integer `buckets` with boundary keys, `peak_index`/`peak_total`, and `empty` for an all-zero range — no "peak 0"), four fixed dimension arrays (`client`/`model`/`provider`/`project`; at most 9 independent rows in `total`-descending order — ties by name — plus at most one trailing "other" row carrying machine-readable `is_other`/`other_count`, so independent rows + the tail sum exactly to `totals` per metric), `heatmap` with a `mode` field selecting the display buckets: `day_hour` (1–7 days; per-day 24 `hours` in local time), `day_block` (8–31 days; per-day six 4-hour `blocks` summed server-side), or `calendar` (32+ days; per-day totals only), each day carrying `future: true` when it is after today — the frontend renders those as blank ("not yet") while past zero days stay real zeros — `custom_views` (one entry per configured subquery in name order: declared dimension names plus final rows — at most 19 independent combos in `total`-descending order plus at most one `is_other` "other combinations" row, seven metrics conserved), and the top 20 sessions with original titles. All truncation, tail merging, and bucket shaping happen server-side inside one read snapshot, so a single refresh cannot mix snapshots and the browser never re-truncates or re-aggregates |
| `GET /api/config` | — | the editable configuration model (daemon, log, clients with data-source paths, routers, provider aliases, and the parsed query section with effective values and, when the on-disk query section cannot be parsed, `diagnostics`), plus the `revision` of the on-disk file (a fixed sentinel when the file does not yet exist); `data_dir` is deliberately not part of the model — data-directory migration stays a CLI-only confirmed operation |
| `PUT /api/config` | body `{revision, config}` — the draft as served by `GET /api/config`; the `query` section may be omitted to keep the on-disk query section untouched (the page omits it unless the user edited the section or explicitly restored all defaults, so an unrelated save never rewrites a query section that fails to parse) | on success the normalized saved draft, the new `revision`, `changed`, `warnings` (non-empty when the file was saved but a side effect such as autostart sync failed), and `suggested_steps`; `409` with the current `revision` when the file changed elsewhere (local edits are kept, nothing is overwritten), `400` for validation failures (unknown clients/path keys, invalid query definitions, out-of-range values), `500` for write failures |
| `GET /`, `GET /assets/…` | — | embedded HTML page and static assets (`Cache-Control: no-store`) |

- Errors are uniform JSON `{"error":{"message":"…"}}`: `400` for invalid parameters or drafts, `404` for missing assets, `409` for revision conflicts, `500` for query or write failures.
- The data surface keeps no CORS headers (same-origin use) and no daemon interaction, and never writes the database; the only write path is `PUT /api/config` through the shared `configapp` chain.
- Dimension rows are plain integers — K/M/B formatting is left to the frontend; `provider` rows apply `[provider_aliases]` exactly like the query views, and config changes take effect on the next request (provider aliases and custom views included).
