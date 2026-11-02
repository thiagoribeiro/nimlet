# Landing page

`site/` is the dependency-free static landing page published at
https://thiagoribeiro.github.io/nimlet/ (plain HTML, CSS and JS; no build step, CDN, fonts,
analytics or external requests).

## Preview

```bash
cd site && python3 -m http.server 8000   # http://localhost:8000/
```

## Validate

```bash
python3 .github/scripts/verify_site.py   # anchors, local assets, metadata, OG image, SVGs, sitemap
node --check site/app.js
```

Also check 1440, 768, 390 and 320 px widths (no horizontal scroll), the mobile menu (`Escape`
closes it), the code tabs and copy button, and the demo (offline → publish → reconnect).

## Deploy

`.github/workflows/pages.yml` validates on pull requests and deploys `site/` to GitHub Pages on
pushes to `main` that touch it (or via manual dispatch). Pages' source must be set to
"GitHub Actions" once in the repository settings.

## Keeping it accurate

- Claims must match README.md: PostgreSQL is the source of truth; Redis is required at startup
  (wake-ups, presence, push queue) but stores no events; AppService callbacks are best-effort.
- Benchmark figures (2,500 msg/s, 50k deliveries/s) come from the phase-04 sync-heavy run in
  `docs/benchmarks.md` and still hold in phase 06. Re-check that file before changing them.
- All local URLs are relative so the page works under the `/nimlet/` subpath. `404.html` is the
  exception: it uses `/nimlet/...` because GitHub Pages serves it from any path.
- Canonical URL, `og:url`, `og:image` and `sitemap.xml` point to
  `https://thiagoribeiro.github.io/nimlet/`; update them all if a custom domain is added, and
  bump `<lastmod>` in `sitemap.xml` on content changes.
- `robots.txt` only works at the host root, so it lives in the `thiagoribeiro.github.io`
  repository, which lists this sitemap.
- `site/googled3a93fd256d1471a.html` verifies the Google Search Console property for
  `https://thiagoribeiro.github.io/nimlet/`. Don't remove it, or the property loses verification.
