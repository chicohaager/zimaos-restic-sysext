#!/usr/bin/env python3
"""End-to-end check of the restic tile through the ZimaOS gateway, with a real login.

Run after tools/host-test.sh on the host (it leaves the source folder
/DATA/AppData/restic-hosttest-src this test backs up):

    python3 tools/tile-test.py --url http://<zimaos-host> --user <name> --password-file <file>

Creates the job "tiletest" (repository /DATA/AppData/restic-tiletest-repo-<time>,
generated password, *.tmp excluded), refuses a wrong password and a missing
folder, backs up through "Back up now", reads the snapshot, restores it into
a new folder, then deletes the job again (the repository stays; host-test.sh
cleanup removes it). Options: --expect (version, default: VERSION),
--keep (leave the job). The password is read from a file and never printed;
API answers are printed field by field.
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
SRC = "/DATA/AppData/restic-hosttest-src"
REPO = "/DATA/AppData/restic-tiletest-repo"


class Host:
    def __init__(self, url: str) -> None:
        self.url = url.rstrip("/")
        self.token = ""

    def call(self, method: str, path: str, body: dict | None = None, origin: str | None = None) -> tuple[int, dict]:
        req = urllib.request.Request(self.url + path, method=method,
                                     data=None if body is None else json.dumps(body).encode())
        req.add_header("Content-Type", "application/json")
        req.add_header("Origin", origin or self.url)
        if self.token:
            req.add_header("Authorization", "Bearer " + self.token)
        try:
            with urllib.request.urlopen(req, timeout=180) as r:
                return r.status, json.loads(r.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read() or b"{}")
            except json.JSONDecodeError:
                return e.code, {}

    def login(self, user: str, password: str) -> None:
        st, d = self.call("POST", "/v1/users/login", {"username": user, "password": password})
        try:
            self.token = d["data"]["token"]["access_token"]
        except (KeyError, TypeError):
            raise SystemExit(f"✗ login failed: HTTP {st}") from None


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--url", required=True)
    ap.add_argument("--user", required=True)
    ap.add_argument("--password-file", required=True)
    ap.add_argument("--expect", default=(ROOT / "VERSION").read_text().strip())
    ap.add_argument("--keep", action="store_true")
    a = ap.parse_args()

    h = Host(a.url)
    results: list[bool] = []

    def check(name: str, cond: bool, info: object = "") -> bool:
        results.append(bool(cond))
        print(("OK   " if cond else "FAIL ") + name + (f"  ({info})" if info != "" else ""))
        return bool(cond)

    st, _ = h.call("GET", "/restic/jobs")
    check("no token: 401", st == 401, st)
    st, d = h.call("GET", "/restic/health")
    check(f"health reports {a.expect}", st == 200 and d.get("version") == a.expect, d.get("version"))
    h.login(a.user, pathlib.Path(a.password_file).read_text().strip())

    repo = REPO + time.strftime("-%Y%m%dT%H%M%S")  # one per run: the generated password opens only its own
    job = {"name": "tiletest", "repository": repo, "password": "", "paths": [SRC],
           "excludes": ["*.tmp"], "schedule": "", "init": True}
    st, _ = h.call("POST", "/restic/jobs", job, origin="http://evil.example")
    check("foreign Origin: 403", st == 403, st)
    st, d = h.call("POST", "/restic/jobs", job)
    if not check("create job (new repository, generated password)",
                 st == 200 and d.get("status") in ("created", "connected") and len(d.get("generated_password", "")) >= 30,
                 f"{st} {d.get('status') or d.get('code')}"):
        return 1
    st, d = h.call("POST", "/restic/jobs", dict(job, name="tilewrong", password="wrong", init=False))
    check("wrong password refused", st == 400 and d.get("code") == "wrong_password", f"{st} {d.get('code')}")
    st, d = h.call("POST", "/restic/jobs", dict(job, name="tilemiss", paths=["/DATA/does-not-exist"]))
    check("missing folder refused", st == 400 and d.get("code") == "missing_path", f"{st} {d.get('code')}")

    st, _ = h.call("POST", "/restic/jobs/run", {"name": "tiletest"})
    check("back up now", st == 200, st)
    j: dict = {}
    for _ in range(90):
        time.sleep(2)
        _, d = h.call("GET", "/restic/jobs")
        j = next((x for x in d.get("jobs", []) if x.get("name") == "tiletest"), {})
        if j.get("state") not in ("activating", "active") and j.get("last"):
            break
    last = j.get("last") or {}
    check("backup finished, rc 0, no folder missing", last.get("rc") == 0 and last.get("missing_paths") == 0,
          f"state={j.get('state')} rc={last.get('rc')} files_new={(last.get('summary') or {}).get('files_new')}")

    st, d = h.call("GET", "/restic/snapshots?name=tiletest")
    snaps = d.get("snapshots", [])
    if not check("one snapshot", st == 200 and len(snaps) == 1, f"{st} {len(snaps)}"):
        return 1
    st, d = h.call("POST", "/restic/restore", {"name": "tiletest", "snapshot": snaps[0]["id"]})
    target = d.get("target", "")
    check("restore started into a new folder", st == 200 and target.startswith("/DATA/restic-restore/tiletest-"), target)
    r: dict = {}
    for _ in range(60):
        time.sleep(2)
        _, d = h.call("GET", "/restic/jobs")
        r = next((x for x in d.get("jobs", []) if x.get("name") == "tiletest"), {}).get("restore") or {}
        if r.get("target") == target:
            break
    check("restore rc 0", r.get("target") == target and r.get("rc") == 0, r.get("rc"))

    if not a.keep:
        st, d = h.call("POST", "/restic/jobs/delete", {"name": "tiletest"})
        check("job deleted (files kept aside)", st == 200 and "/jobs/.deleted/tiletest-" in d.get("kept_in", ""), st)

    bad = results.count(False)
    print(f"result: {len(results) - bad} OK, {bad} FAIL")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
