"""Regenerate the pinned venue-rank data used by the IEEE source (ADR 0020).

Inputs are downloaded by a maintainer, never at container build or runtime:

  * CCFrank4dblp ``data/dataGen.js`` at a pinned commit (MIT)
  * the ``impact_factor`` wheel at a pinned version (MIT)

Usage:
  python tools/build_rank_data.py --ccf-js dataGen.js --ccf-commit <sha> \
      --impact-factor-wheel impact_factor-1.1.4-py3-none-any.whl

Outputs ``data/ccf_rank_list.tsv``, ``data/impact_factor.sqlite3`` and
``data/manifest.json`` (provenance plus SHA-256 checked at sidecar start-up).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import zipfile
from pathlib import Path

DATA_DIR = Path(__file__).resolve().parent.parent / "data"
LINE = re.compile(r"^(?P<rank>\S+)\s*\t(?P<abbr>[^\t]*)\t(?P<name>.*?)\s+(?P<prefix>/\S+)\s+(?P<key>/\S+)\s*$")
WHEEL_DB = "impact_factor/data/impact_factor.sqlite3"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def parse_ccf(source: str) -> list[tuple[str, str, str, str]]:
    start = source.index("const ccfRankList")
    end = source.index(";", start)
    parts = re.findall(r'"((?:[^"\\]|\\.)*)"', source[start:end])
    text = "".join(parts).replace("\\t", "\t").replace("\\n", "\n")
    rows: list[tuple[str, str, str, str]] = []
    for line in text.split("\n"):
        if not line.strip():
            continue
        fields = line.split("\t")
        if len(fields) == 5:
            rank, abbr, name, _prefix, key = (field.strip() for field in fields)
        else:
            # One upstream row separates the DBLP paths with spaces instead of tabs.
            match = LINE.match(line)
            if not match:
                raise ValueError(f"unparseable CCF row: {line!r}")
            rank, abbr, name, key = (match.group(g).strip() for g in ("rank", "abbr", "name", "key"))
        if rank not in {"A", "B", "C"}:
            continue  # e.g. preprint rows without a CCF rank
        # A few newly listed venues have no DBLP path yet; they stay matchable by exact name.
        if not name or (key and not key.startswith(("/conf/", "/journals/"))):
            raise ValueError(f"unexpected CCF row: {line!r}")
        rows.append((rank, abbr, name, key))
    return rows


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--ccf-js", required=True, type=Path)
    parser.add_argument("--ccf-commit", required=True)
    parser.add_argument("--impact-factor-wheel", required=True, type=Path)
    args = parser.parse_args()

    ccf_source = args.ccf_js.read_bytes()
    rows = parse_ccf(ccf_source.decode("utf-8"))
    tsv = "rank\tabbr\tname\tdblp_key\n" + "".join("\t".join(row) + "\n" for row in rows)
    tsv_bytes = tsv.encode("utf-8")

    wheel_bytes = args.impact_factor_wheel.read_bytes()
    with zipfile.ZipFile(args.impact_factor_wheel) as wheel:
        version = json.loads(wheel.read("impact_factor/version.json"))["version"]
        sqlite_bytes = wheel.read(WHEEL_DB)

    DATA_DIR.mkdir(parents=True, exist_ok=True)
    (DATA_DIR / "ccf_rank_list.tsv").write_bytes(tsv_bytes)
    (DATA_DIR / "impact_factor.sqlite3").write_bytes(sqlite_bytes)
    manifest = {
        "ccf": {
            "file": "ccf_rank_list.tsv",
            "sha256": sha256(tsv_bytes),
            "rows": len(rows),
            "upstream": "https://github.com/WenyanLiu/CCFrank4dblp",
            "license": "MIT",
            "commit": args.ccf_commit,
            "upstreamFile": "data/dataGen.js",
            "upstreamSha256": sha256(ccf_source),
            "version": f"ccfrank4dblp@{args.ccf_commit[:7]}",
        },
        "impactFactor": {
            "file": "impact_factor.sqlite3",
            "sha256": sha256(sqlite_bytes),
            "upstream": "https://github.com/suqingdong/impact_factor",
            "license": "MIT",
            "package": f"impact_factor=={version}",
            "wheelSha256": sha256(wheel_bytes),
            "version": f"impact_factor@{version}",
            "note": "Third-party compilation. CAS partition has no edition, year, subject or TOP flag.",
        },
    }
    (DATA_DIR / "manifest.json").write_text(json.dumps(manifest, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
    print(f"wrote {len(rows)} CCF rows and impact_factor {version} to {DATA_DIR}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
