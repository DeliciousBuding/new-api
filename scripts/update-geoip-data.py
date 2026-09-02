#!/usr/bin/env python3
"""Refresh GeoIP data files behind the known-answer gate.

Downloads the latest DB-IP Lite monthly city/ASN databases and the ip2region
community xdb into a staging directory, runs the Go known-answer tests
(pkg/geoip via GEOIP_TEST_DB_DIR) against them, and promotes the files into
the target directory only when the gate passes. Stdlib only.

Usage:
    python scripts/update-geoip-data.py --target /opt/geoip
    python scripts/update-geoip-data.py --target /opt/geoip --month 2026-08 \
        --xdb-rev 7e4c5b6761451c734db51e2b0652219ffc1aba36
"""
from __future__ import annotations

import argparse
import gzip
import os
import shutil
import subprocess
import sys
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

DBIP_URL = "https://download.db-ip.com/free/dbip-{kind}-lite-{month}.mmdb.gz"
XDB_URL = ("https://raw.githubusercontent.com/lionsoul2014/ip2region/"
           "{rev}/data/ip2region_v4.xdb")
FILES = ("dbip-city-lite.mmdb", "dbip-asn-lite.mmdb", "ip2region_v4.xdb")


def download(url: str, dest: Path) -> None:
    print(f"  get {url}")
    req = urllib.request.Request(
        url, headers={"User-Agent": "newapi-geoip-updater/1"})
    with urllib.request.urlopen(req, timeout=120) as resp, open(dest, "wb") as out:
        shutil.copyfileobj(resp, out)


def fetch_month(month: str, staging: Path) -> None:
    for kind, name in (("city", "dbip-city-lite.mmdb"),
                       ("asn", "dbip-asn-lite.mmdb")):
        gz = staging / (name + ".gz")
        download(DBIP_URL.format(kind=kind, month=month), gz)
        with gzip.open(gz, "rb") as src, open(staging / name, "wb") as dst:
            shutil.copyfileobj(src, dst)
        gz.unlink()


def run_gate(repo_root: Path, staging: Path) -> bool:
    env = dict(os.environ, GEOIP_TEST_DB_DIR=str(staging))
    cmd = ["go", "test", "./pkg/geoip/", "-count=1",
           "-run", "TestKnownAnswers|TestLookupWithRealDatabases"]
    print(f"  gate: {' '.join(cmd)}")
    return subprocess.run(cmd, cwd=repo_root, env=env).returncode == 0


def main() -> int:
    ap = argparse.ArgumentParser(
        description="Refresh GeoIP data files behind the known-answer gate.")
    ap.add_argument("--target", required=True,
                    help="directory holding the live data files")
    ap.add_argument("--month",
                    default=datetime.now(timezone.utc).strftime("%Y-%m"),
                    help="DB-IP Lite month snapshot (default: current month)")
    ap.add_argument("--xdb-rev", default="master",
                    help="ip2region commit SHA or ref (default: master)")
    ap.add_argument("--repo-root", default=".",
                    help="repo root for the Go gate (default: cwd)")
    ap.add_argument("--skip-kat", action="store_true",
                    help="promote without the gate (not recommended)")
    args = ap.parse_args()

    target = Path(args.target)
    repo_root = Path(args.repo_root).resolve()
    staging = target.with_name(target.name + ".staging")
    if staging.exists():
        shutil.rmtree(staging)
    staging.mkdir(parents=True)

    print(f"[1/3] downloading into {staging}")
    fetch_month(args.month, staging)
    download(XDB_URL.format(rev=args.xdb_rev), staging / "ip2region_v4.xdb")

    if args.skip_kat:
        print("[2/3] gate skipped (--skip-kat)")
        ok = True
    else:
        print("[2/3] running known-answer gate")
        ok = run_gate(repo_root, staging)
    if not ok:
        print("gate FAILED; staging left for inspection, target untouched",
              file=sys.stderr)
        return 1

    print(f"[3/3] promoting into {target}")
    target.mkdir(parents=True, exist_ok=True)
    for name in FILES:
        tmp = target / (name + ".new")
        shutil.copyfile(staging / name, tmp)
        os.replace(tmp, target / name)  # atomic per file; hot reload swaps
    shutil.rmtree(staging)
    print("done; readers hot-swap within GEOIP_RELOAD_INTERVAL")
    return 0


if __name__ == "__main__":
    sys.exit(main())
