#!/usr/bin/env python3
"""Regenerate the committed PWA icons from mark.svg (requires rsvg-convert)."""
from pathlib import Path
import subprocess
import xml.etree.ElementTree as ET

web = Path(__file__).resolve().parents[2] / "internal/portal/web"
out = web / "icons"
out.mkdir(exist_ok=True)
source = (web / "mark.svg").read_bytes()
for name, size in [("icon-192", 192), ("icon-512", 512),
                   ("maskable-512", 512), ("apple-touch-icon", 180)]:
    svg = ET.fromstring(source)
    if name in ("maskable-512", "apple-touch-icon"):
        # OS supplies the corner mask; extend the existing dark background.
        svg.find("{*}rect").set("rx", "0")
    if name == "maskable-512":
        # Keep the entire white stroke inside the central radius-40% circle.
        group = svg.find("{*}g")
        group.set("transform", "translate(16 16) scale(.8) translate(-16 -16) "
                  + group.get("transform"))
    subprocess.run(["rsvg-convert", "-w", str(size), "-h", str(size),
                    "-o", str(out / (name + ".png"))],
                   input=ET.tostring(svg), check=True)
