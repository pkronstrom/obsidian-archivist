#!/usr/bin/env python3
"""Reference consumer: react to vault changes.

This is the shape an AI worker should take. It is deliberately ~100 lines of
standard library so the pattern is visible rather than buried in a framework.

    ./watch-vault.py --url https://vault.example.net --token "$TOKEN"

The pattern, and the reason it is this shape:

  /v1/events   says WHEN something changed, and enough to triage it
  the cursor   says WHAT changed, durably, however long you were away

Events are deliberately lossy. Never treat the stream as the record: keep the
cursor, and on every reconnect ask /v1/changes what you missed. That is why
there is no retry queue, no acknowledgement, and no backlog to manage here --
a missed event costs nothing.

If the worker runs on the same host as vaultsync, it should read the changed
files straight off disk rather than fetching them: the vault is an ordinary
directory. --vault enables that. Otherwise it falls back to /v1/content.
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

CURSOR_FILE = os.path.expanduser("~/.vaultsync-worker-cursor")

# Line-buffered, so output appears as it happens rather than when a pipe's
# buffer fills. Without this the worker looks dead under `docker logs`.
sys.stdout.reconfigure(line_buffering=True)


def api(url, token, path, data=None):
    req = urllib.request.Request(f"{url.rstrip('/')}{path}", data=data)
    req.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(req, timeout=30) as r:
        return r.read()


def load_cursor():
    try:
        with open(CURSOR_FILE) as f:
            return f.read().strip()
    except FileNotFoundError:
        return ""


def save_cursor(c):
    # Written only after the work succeeded, so a crash replays rather than skips.
    with open(CURSOR_FILE, "w") as f:
        f.write(c)


def content_of(args, change):
    """Read a changed file: off disk when local, over HTTP otherwise."""
    if args.vault:
        p = os.path.join(args.vault, change["path"])
        try:
            with open(p, "rb") as f:
                return f.read()
        except FileNotFoundError:
            return None  # deleted, or already changed again
    return api(args.url, args.token, f"/v1/content/{change['hash']}")


def handle(args, change):
    """Where your worker does its actual job.

    The event already carries path, extension, kind and size, so most triage
    needs no I/O at all -- skip what you cannot use before touching the file.
    """
    if change["op"] == "del":
        print(f"  deleted   {change['path']}")
        return
    if change.get("kind") == "binary":
        print(f"  skipped   {change['path']} ({change['ext']}, {change['size']} bytes, binary)")
        return
    if change.get("ext") not in ("md", "txt", ""):
        print(f"  skipped   {change['path']} (not text)")
        return

    body = content_of(args, change)
    if body is None:
        print(f"  vanished  {change['path']}")
        return
    text = body.decode("utf-8", "replace")
    first = next((l for l in text.splitlines() if l.strip()), "")
    words = len(text.split())
    # Replace this with: embed it, summarise it, tag it, file it, whatever.
    print(f"  note      {change['path']}  {words} words  | {first[:60]}")


def catch_up(args):
    """Ask what changed since the cursor. This is the durable path."""
    cursor = load_cursor()
    try:
        raw = api(args.url, args.token, f"/v1/changes?since={cursor}")
    except urllib.error.HTTPError as e:
        if e.code != 409:
            raise
        # Our cursor predates a history rewrite, or belongs elsewhere. There is
        # no valid diff to ask for; adopt the current head and move on.
        print("! cursor not recognised; resetting to head")
        head = json.loads(api(args.url, args.token, "/v1/head"))["head"]
        save_cursor(head)
        return
    d = json.loads(raw)
    if d["entries"]:
        print(f"catching up: {len(d['entries'])} change(s) since {cursor[:8] or 'the beginning'}")
        for c in d["entries"]:
            handle(args, c)
    save_cursor(d["head"])


def stream(args):
    """Follow the event stream, reconnecting forever."""
    backoff = 1
    while True:
        try:
            req = urllib.request.Request(f"{args.url.rstrip('/')}/v1/events")
            req.add_header("Authorization", f"Bearer {args.token}")
            with urllib.request.urlopen(req) as r:
                print("connected")
                backoff = 1
                for line in r:
                    line = line.decode().strip()
                    if not line.startswith("data:"):
                        continue  # keep-alives and blanks
                    ev = json.loads(line[5:].strip())
                    if not ev.get("changes"):
                        continue  # the on-connect head, or a truncated event
                    print(f"event {ev['head'][:8]}  {ev['count']} change(s)")
                    for c in ev["changes"]:
                        handle(args, c)
                    # Only after the work is done, so a crash replays instead of skipping.
                    save_cursor(ev["head"])
                    if ev.get("truncated"):
                        print("  (truncated event; catching up from the cursor)")
                        catch_up(args)
        except Exception as e:
            print(f"! disconnected: {e}; retrying in {backoff}s")
            time.sleep(backoff)
            backoff = min(backoff * 2, 60)
            # Anything missed while disconnected comes back from the cursor.
            try:
                catch_up(args)
            except Exception as e2:
                print(f"! catch-up failed: {e2}")


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--url", default=os.environ.get("VAULTSYNC_URL", "http://localhost:8090"))
    ap.add_argument("--token", default=os.environ.get("VAULTSYNC_TOKEN", ""))
    ap.add_argument("--vault", default=os.environ.get("VAULTSYNC_VAULT", ""),
                    help="read files from disk instead of over HTTP (same-host workers)")
    args = ap.parse_args()
    if not args.token:
        raise SystemExit("a token is required (--token or VAULTSYNC_TOKEN)")

    # Always catch up before streaming: whatever happened while we were not
    # running is invisible to the stream.
    catch_up(args)
    stream(args)


if __name__ == "__main__":
    main()
