#!/usr/bin/env python3
"""Validate the portable static Nimlet landing before publishing to GitHub Pages."""
from html.parser import HTMLParser
from pathlib import Path
from urllib.parse import urlsplit, unquote
import re
import struct
import sys
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
SITE = ROOT / "site"
errors = []


def fail(message):
    errors.append(message)


class SiteParser(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.ids = set()
        self.refs = []
        self.images_without_alt = []
        self.title_count = 0
        self.h1_count = 0
        self.meta = {}

    def handle_starttag(self, tag, attrs):
        d = dict(attrs)
        if "id" in d:
            if d["id"] in self.ids:
                fail(f"Duplicate ID: {d['id']}")
            self.ids.add(d["id"])
        if tag == "title":
            self.title_count += 1
        if tag == "h1":
            self.h1_count += 1
        if tag == "img" and "alt" not in d:
            self.images_without_alt.append(d.get("src"))
        if tag == "meta":
            key = d.get("property", d.get("name", ""))
            if key:
                self.meta[key] = d.get("content", "")
        for key in ("href", "src"):
            if key in d:
                self.refs.append((tag, key, d[key]))


def validate_local_ref(raw, ids, filename):
    if raw.startswith(("mailto:", "tel:", "data:", "javascript:")):
        fail(f"Disallowed URL scheme in {filename}: {raw}")
        return
    u = urlsplit(raw)
    if u.scheme or u.netloc:
        return
    if raw.startswith("/"):
        fail(f"Root-relative URL would break on /nimlet/: {raw}")
        return
    if not u.path and u.fragment:
        if u.fragment not in ids:
            fail(f"Broken anchor in {filename}: #{u.fragment}")
        return
    if not u.path:
        return
    target = (SITE / unquote(u.path)).resolve()
    if not target.is_relative_to(SITE):
        fail(f"Link escapes site directory: {raw}")
        return
    if target.is_dir():
        target = target / "index.html"
    if not target.exists():
        fail(f"Missing local file in {filename}: {raw}")


def main():
    index = SITE / "index.html"
    assert index.exists(), "site/index.html is missing"
    parser = SiteParser()
    parser.feed(index.read_text(encoding="utf-8"))
    if parser.images_without_alt:
        fail("Images without alt attributes: " + str(parser.images_without_alt))
    if parser.h1_count != 1:
        fail(f"Expected exactly 1 h1, found {parser.h1_count}")
    for tag, attr, ref in parser.refs:
        validate_local_ref(ref, parser.ids, "index.html")
    expected = ["why", "use-cases", "features", "architecture", "demo", "benchmarks", "quickstart"]
    for section in expected:
        if section not in parser.ids:
            fail(f"Missing section anchor #{section}")
    for meta in ["description", "og:title", "og:description", "og:image", "og:url", "twitter:card"]:
        if not parser.meta.get(meta):
            fail(f"Missing metadata {meta}")
    for path in ["site/app.js", "site/styles.css", "site/sitemap.xml", "site/.nojekyll", "site/404.html"]:
        if not (ROOT / path).exists():
            fail(f"Missing file: {path}")
    for svg in (SITE / "assets").glob("*.svg"):
        try:
            ET.parse(svg)
        except ET.ParseError as e:
            fail(f"Invalid SVG {svg.name}: {e}")
    for css in (SITE / "styles.css",):
        css_text = css.read_text(encoding="utf-8")
        for match in re.findall(r'url\(\s*["\']?([^\)"\']+)', css_text):
            validate_local_ref(match.strip(), parser.ids, css.name)
    og = SITE / "assets" / "og-cover.png"
    if og.exists():
        with og.open("rb") as fh:
            data = fh.read(24)
        if not data.startswith(b"\x89PNG\r\n\x1a\n") or struct.unpack(">II", data[16:24]) != (1200, 630):
            fail("Open Graph image must be a 1200x630 PNG")
    else:
        fail("Missing Open Graph PNG")
    try:
        ET.parse(SITE / "sitemap.xml")
    except ET.ParseError as e:
        fail(f"Invalid sitemap.xml: {e}")
    if errors:
        print("Static site validation failed:\n  - " + "\n  - ".join(errors), file=sys.stderr)
        return 1
    print(f"Site OK: {len(parser.ids)} IDs, {len(parser.refs)} links/assets, metadata and images validated.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
