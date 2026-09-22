# /// script
# requires-python = ">=3.9"
# dependencies = ["markdown==3.7"]
# ///
"""Check the published course and its portable export using real build output.

Run with `uv run tools/docsite/test_course.py`.
"""

from html.parser import HTMLParser
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[2]
COURSE = ROOT / "docs/learning/remote-workspace"


class Page(HTMLParser):
    def __init__(self, path):
        super().__init__()
        self.links = []
        self.ids = []
        self.scripts = []
        self.feed(path.read_text(encoding="utf-8"))

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if "id" in attrs:
            self.ids.append(attrs["id"])
        for key in ("href", "src"):
            if key in attrs:
                self.links.append(attrs[key])
        if tag == "script" and "src" in attrs:
            self.scripts.append(attrs["src"])


class CourseBuildTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory()
        cls.addClassCleanup(cls.temp.cleanup)
        cls.out = Path(cls.temp.name) / "docs"
        cls.export = Path(cls.temp.name) / "teaching"
        cls.export.mkdir()
        # Exporting into a teaching workspace must preserve authored material.
        (cls.export / "MISSION.md").write_text("keep my mission", encoding="utf-8")
        subprocess.run(
            [sys.executable, str(ROOT / "tools/docsite/build.py"),
             "--out", str(cls.out), "--course-export", str(cls.export)],
            check=True, capture_output=True, text=True,
        )
        cls.registry = json.loads((COURSE / "course.json").read_text())
        cls.entries = cls.registry["lessons"] + cls.registry["references"]

    def test_every_course_page_is_discoverable_and_has_unique_ids(self):
        index = Page(self.out / "index.html")
        for entry in self.entries:
            with self.subTest(slug=entry["slug"]):
                self.assertIn("/docs/" + entry["slug"] + "/", index.links)
                page = Page(self.out / entry["slug"] / "index.html")
                self.assertEqual(len(page.ids), len(set(page.ids)))
                scripts = [s for s in page.scripts if "/_course/course.js?" in s]
                self.assertEqual(len(scripts), 1)

    def test_export_preserves_workspace_and_resolves_local_links(self):
        self.assertEqual((self.export / "MISSION.md").read_text(), "keep my mission")
        for kind, folder in (("lessons", "lessons"), ("references", "reference")):
            for entry in self.registry[kind]:
                path = self.export / folder / entry["html"]
                page = Page(path)
                for link in page.links:
                    url = urlsplit(link)
                    if url.scheme or url.netloc:
                        continue
                    with self.subTest(page=path.name, link=link):
                        self.assertFalse(url.path.startswith("/"), "export must be portable")
                        target = (path.parent / unquote(url.path)).resolve() if url.path else path
                        self.assertTrue(target.is_file(), "local asset or lesson is missing")
                        if url.fragment and target.suffix == ".html":
                            self.assertIn(unquote(url.fragment), Page(target).ids)


if __name__ == "__main__":
    unittest.main()
