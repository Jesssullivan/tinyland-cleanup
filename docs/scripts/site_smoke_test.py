#!/usr/bin/env python3
"""Hermetic smoke test for the built MkDocs site.

Asserts the expected pages exist and that every internal link on the home page
resolves on disk. Reads only the built TreeArtifact, so it needs no network or
browser (the cacheable analogue of site.scaffold's Playwright smoke test).
"""
import html.parser
import os
import sys

site = sys.argv[1]

required = [
    "index.html",
    "installation/index.html",
    "usage/index.html",
    "configuration/index.html",
    "plugins/index.html",
    "json-report-schema/index.html",
    "search/search_index.json",  # proves the search plugin ran
]
missing = [p for p in required if not os.path.exists(os.path.join(site, p))]
assert not missing, f"missing expected pages: {missing}"


class _Links(html.parser.HTMLParser):
    def __init__(self):
        super().__init__()
        self.hrefs = []

    def handle_starttag(self, tag, attrs):
        if tag == "a":
            for key, value in attrs:
                if key == "href" and value:
                    self.hrefs.append(value)


parser = _Links()
with open(os.path.join(site, "index.html"), encoding="utf-8") as handle:
    parser.feed(handle.read())

for href in parser.hrefs:
    if href.startswith(("http://", "https://", "#", "mailto:")):
        continue
    target = href.split("#", 1)[0].strip("/")
    if not target:
        continue
    on_disk = os.path.join(site, target)
    if os.path.exists(on_disk) or os.path.exists(os.path.join(on_disk, "index.html")):
        continue
    raise AssertionError(f"dangling internal link in index.html: {href}")

print("docs site smoke OK")
