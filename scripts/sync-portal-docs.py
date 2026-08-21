#!/usr/bin/env python3
"""Push docs/portal/ into the portal's docs database.

The portal renders whatever is in Postgres, but that database has been lost
once already (2026-07-16, relay app deleted from the thunderbox console, taking
its attached database with it) and everything hand-written in the browser was
unrecoverable. So git is the source of truth now and this script is how it gets
in: edit the markdown here, run this, the portal catches up.

Idempotent — existing slugs are updated in place, missing ones are created.

The markdown bodies in git are deployment-neutral: they refer to
https://relay.example.com and https://portal.example.com. This script rewrites
those placeholders to the target instance's real origins before pushing, taken
from --relay-origin/--portal-origin (default: the WANCTL_RELAY/WANCTL_PORTAL
environment variables, i.e. the same instance the wanctl CLI will talk to).
A body that still contains example.com after rewriting aborts the sync, so a
half-configured run can't publish placeholder text to a live portal.

Usage:
    scripts/sync-portal-docs.py [--dry-run] [--wanctl PATH]
        [--relay-origin https://relay.example.org]
        [--portal-origin https://portal.example.org]

Requires a logged-in controller (`wanctl login`) or WANCTL_TOKEN in the
environment. Articles are written under the caller's namespace.
"""

import argparse
import json
import os
import pathlib
import subprocess
import sys
import urllib.parse

ROOT = pathlib.Path(__file__).resolve().parent.parent
PORTAL = ROOT / "docs" / "portal"


def substitute(body, origins):
    """Rewrite <name>.example.com placeholders to the configured origins."""
    for name, origin in origins.items():
        if not origin:
            continue
        origin = origin.rstrip("/")
        host = urllib.parse.urlsplit(origin).netloc or origin
        body = body.replace("https://%s.example.com" % name, origin)
        body = body.replace("%s.example.com" % name, host)
    return body


def run(wanctl, args, stdin=None, check=True):
    p = subprocess.run(
        [wanctl, "docs", *args],
        input=stdin,
        capture_output=True,
        text=True,
    )
    if check and p.returncode != 0:
        raise SystemExit(f"wanctl docs {' '.join(args)} failed:\n{p.stderr.strip()}")
    return p


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true")
    ap.add_argument("--wanctl", default="wanctl", help="path to the wanctl binary")
    ap.add_argument("--relay-origin", default=os.environ.get("WANCTL_RELAY", ""),
                    help="relay origin substituted for relay.example.com "
                         "(default: $WANCTL_RELAY)")
    ap.add_argument("--portal-origin", default=os.environ.get("WANCTL_PORTAL", ""),
                    help="portal origin substituted for portal.example.com "
                         "(default: $WANCTL_PORTAL)")
    args = ap.parse_args()
    origins = {"relay": args.relay_origin, "portal": args.portal_origin}

    manifest = json.loads((PORTAL / "manifest.json").read_text(encoding="utf-8"))

    # `wanctl docs groups` prints "<position>  <slug>  <title>" per line.
    existing_groups = set()
    p = run(args.wanctl, ["groups"], check=False)
    if p.returncode == 0:
        for ln in p.stdout.splitlines():
            parts = ln.split()
            if len(parts) >= 2:
                existing_groups.add(parts[1])

    for g in manifest["groups"]:
        if g["slug"] in existing_groups:
            print(f"group {g['slug']}: 已存在，跳过")
            continue
        print(f"group {g['slug']}: 新建")
        if not args.dry_run:
            run(args.wanctl, ["group", "new", "--slug", g["slug"],
                              "--title", g["title"], "--position", str(g["position"])])

    for a in manifest["articles"]:
        body = substitute((PORTAL / a["file"]).read_text(encoding="utf-8"), origins)
        if "example.com" in body:
            raise SystemExit(
                f"{a['file']} still contains example.com placeholders after "
                "substitution; pass --relay-origin/--portal-origin (or export "
                "WANCTL_RELAY/WANCTL_PORTAL) so real origins get filled in")
        exists = run(args.wanctl, ["get", a["slug"]], check=False).returncode == 0
        verb = "更新" if exists else "新建"
        print(f"{a['slug']}: {verb} ({len(body)} 字符)")
        if args.dry_run:
            continue
        common = ["--title", a["title"], "--group", a["group"],
                  "--position", str(a["position"])]
        if exists:
            # Go's flag package stops parsing at the first positional argument,
            # so the slug has to come after the flags, not before.
            run(args.wanctl, ["edit", *common, a["slug"]], stdin=body)
        else:
            run(args.wanctl, ["new", "--slug", a["slug"], *common], stdin=body)

    print("\n完成。门户刷新即可看到。")


if __name__ == "__main__":
    sys.exit(main())
