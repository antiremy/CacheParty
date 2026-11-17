#!/usr/bin/env python3
"""Trigger friendly-name resolution when CacheParty logs an unseen download."""

import argparse
import json
import os
import re
import selectors
import subprocess
import sys
import time
from pathlib import Path


SOURCE = Path(__file__).resolve().parent
PUBLISHED = Path("./game-names.json")
STEAM = re.compile(r'^\[steam\].*?"(?:GET|HEAD) /depot/([0-9]+)/(?:chunk|manifest)/')
BLIZZARD = re.compile(r'^\[blizzard\].*?"(?:GET|HEAD) /(?:tpr/)?([A-Za-z0-9_-]+)/')


def download(line):
    match = STEAM.search(line)
    if match:
        return "steam", "depot", match[1]
    match = BLIZZARD.search(line)
    if match and match[1].lower() not in ("config", "configs", "data", "patch"):
        return "blizzard", "product", match[1].lower()
    return None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--access-log", type=Path, required=True)
    parser.add_argument("--state", type=Path, default=Path("./game-names-state"))
    parser.add_argument("--output", type=Path, default=PUBLISHED)
    parser.add_argument("--dashboard", default="http://127.0.0.1:8080")
    args = parser.parse_args()
    base = [sys.executable, str(SOURCE / "refresh.py"), "--state", str(args.state),
            "--output", str(args.output), "--dashboard", args.dashboard]
    tail = subprocess.Popen(["tail", "-n", "0", "--follow=name", "--retry", str(args.access_log)], stdout=subprocess.PIPE)
    selector = selectors.DefaultSelector()
    selector.register(tail.stdout, selectors.EVENT_READ)
    pending, recent = set(), {}
    names, next_read, due, reconcile = {}, 0, 0, 0
    worker = None
    buffer = b""
    print("Watching download events; no client addresses or chunk paths are retained", flush=True)
    try:
        while tail.poll() is None:
            now = time.monotonic()
            if now >= next_read:
                try:
                    names = json.loads(args.output.read_text())["names"]
                except (OSError, ValueError, KeyError):
                    pass
                next_read = now + 1
                recent = {event: stamp for event, stamp in recent.items() if now - stamp < 300}
            for key, _ in selector.select(timeout=0.5):
                data = os.read(key.fd, 65536)
                if not data:
                    raise RuntimeError("CacheParty log follower closed")
                buffer += data
                lines = buffer.split(b"\n")
                buffer = lines.pop()
                for line in lines:
                    event = download(line.decode(errors="replace"))
                    if not event:
                        continue
                    service, kind, ident = event
                    entries = names.get(service, {}).get(kind, {})
                    if ident in entries or event in recent:
                        continue
                    if not pending:
                        due = now + 0.5
                    pending.add(event)
                    recent[event] = now
            if worker is not None and worker.poll() is not None:
                if worker.returncode:
                    print(f"Lookup worker exited {worker.returncode}; unseen IDs retry after five minutes", flush=True)
                worker = None
                next_read = 0
            if worker is None and ((pending and time.monotonic() >= due) or now >= reconcile):
                batch = sorted(pending)[:32]
                command = list(base)
                reconcile = now + 60
                for service, kind, ident in batch:
                    command += ["--depot" if service == "steam" else "--product", ident]
                    pending.remove((service, kind, ident))
                print("New download IDs: " + json.dumps(batch) + "; starting name lookup", flush=True)
                worker = subprocess.Popen(command)
    finally:
        selector.close()
        for child in (worker, tail):
            if child is not None and child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
    raise RuntimeError("CacheParty log follower exited")


if __name__ == "__main__":
    main()
