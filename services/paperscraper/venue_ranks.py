"""Deterministic venue-rank matching for IEEE papers (ADR 0020).

Two pinned, vendored datasets are used read-only:

* ``data/ccf_rank_list.tsv``: the CCF 2026 list from CCFrank4dblp.
* ``data/impact_factor.sqlite3``: JCR quartile, impact factor and CAS partition by ISSN.

Matching never guesses. A rank is reported only when exactly one rank is consistent with
the evidence; anything ambiguous or unmatched is reported as ``None``.
"""

from __future__ import annotations

import csv
import hashlib
import json
import logging
import re
import sqlite3
import threading
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable

LOG = logging.getLogger("paperscraper-sidecar.ranks")

# IEEE DOI venue codes that differ from both the CCF abbreviation and the DBLP key. Each
# target must exist in the pinned CCF list; missing targets are dropped at load time.
IEEE_DOI_ALIASES = {
    "jiot": "/journals/iotj/iotj",  # IEEE Internet of Things Journal
    "tnet": "/journals/ton/ton",  # IEEE/ACM Transactions on Networking
}
KNOWN_VENUE_TYPES = {"journal", "conference"}
IEEE_DOI_CODE = re.compile(r"^10\.1109/([a-z]+)")
YEAR = re.compile(r"\b(?:19|20)\d{2}\b")
NON_ALNUM = re.compile(r"[^a-z0-9]+")
QUARTILE = re.compile(r"^Q([1-4])$")


@dataclass(frozen=True)
class CCFVenue:
    rank: str
    abbr: str
    name: str
    dblp_key: str

    @property
    def venue_type(self) -> str | None:
        if self.dblp_key.startswith("/conf/"):
            return "conference"
        if self.dblp_key.startswith("/journals/"):
            return "journal"
        return None

    @property
    def label(self) -> str:
        return self.abbr or self.name


def normalize_name(value: str) -> str:
    value = YEAR.sub(" ", value.lower().replace("&", " and "))
    return " ".join(NON_ALNUM.sub(" ", value).split())


def normalize_code(value: str) -> str:
    return NON_ALNUM.sub("", value.lower())


def ieee_doi_code(doi: str | None) -> str | None:
    """Return the venue code IEEE embeds in its DOIs, e.g. ``cvpr`` or ``tnnls``."""
    match = IEEE_DOI_CODE.match(str(doi or "").strip().lower())
    return match.group(1) if match else None


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b""):
            digest.update(chunk)
    return digest.hexdigest()


class VenueRanks:
    def __init__(self, data_dir: Path) -> None:
        self.available = False
        self.version = ""
        self.ccf_version = ""
        self.impact_version = ""
        self._by_code: dict[str, list[CCFVenue]] = {}
        self._by_key: dict[str, CCFVenue] = {}
        self._by_name: dict[str, list[CCFVenue]] = {}
        self._aliases: dict[str, CCFVenue] = {}
        self._db: sqlite3.Connection | None = None
        self._lock = threading.Lock()
        try:
            self._load(data_dir)
            self.available = True
        except Exception:  # missing or tampered data disables ranks; it never invents them
            LOG.exception("venue rank data unavailable; IEEE results will show no venue ranks")

    def _load(self, data_dir: Path) -> None:
        manifest = json.loads((data_dir / "manifest.json").read_text(encoding="utf-8"))
        ccf_path = data_dir / manifest["ccf"]["file"]
        impact_path = data_dir / manifest["impactFactor"]["file"]
        for path, expected in ((ccf_path, manifest["ccf"]["sha256"]), (impact_path, manifest["impactFactor"]["sha256"])):
            actual = _sha256(path)
            if actual != expected:
                raise ValueError(f"{path.name} sha256 {actual} does not match manifest {expected}")

        with ccf_path.open(encoding="utf-8", newline="") as handle:
            for row in csv.DictReader(handle, delimiter="\t"):
                venue = CCFVenue(row["rank"].strip(), row["abbr"].strip(), row["name"].strip(), row["dblp_key"].strip())
                if venue.rank not in {"A", "B", "C"}:
                    continue
                codes = {normalize_code(venue.abbr)} if venue.abbr else set()
                if venue.dblp_key:
                    self._by_key[venue.dblp_key] = venue
                    codes.add(venue.dblp_key.rsplit("/", 1)[-1].lower())
                for code in codes - {""}:
                    self._by_code.setdefault(code, []).append(venue)
                self._by_name.setdefault(normalize_name(venue.name), []).append(venue)
        self._aliases = {code: self._by_key[key] for code, key in IEEE_DOI_ALIASES.items() if key in self._by_key}

        # Read-only URI: the vendored file is never written by the sidecar.
        self._db = sqlite3.connect(f"file:{impact_path.as_posix()}?mode=ro", uri=True, check_same_thread=False)
        self.ccf_version = manifest["ccf"]["version"]
        self.impact_version = manifest["impactFactor"]["version"]
        self.version = f"{self.ccf_version};{self.impact_version}"

    def match_ccf(self, doi: str | None, venue_name: str | None, venue_type: str | None) -> CCFVenue | None:
        if not self.available:
            return None
        venue_type = venue_type if venue_type in KNOWN_VENUE_TYPES else None
        found: list[CCFVenue] = []
        code = ieee_doi_code(doi)
        if code:
            found = [self._aliases[code]] if code in self._aliases else list(self._by_code.get(code, []))
            # Code evidence is only trusted when both venue kinds are known and agree (ADR 0020;
            # an unknown OpenAlex type fails closed), and an IEEE journal DOI
            # can only name an IEEE journal (e.g. IEEE Micro "mm" must not become ACM MM).
            found = [
                venue for venue in found
                if venue_type is not None
                and venue.venue_type == venue_type
                and (venue.venue_type != "journal" or "ieee" in venue.name.lower())
            ]
        if not found and venue_name:
            found = [
                venue for venue in self._by_name.get(normalize_name(venue_name), [])
                if venue_type is None or venue.venue_type is None or venue.venue_type == venue_type
            ]
        if len({(venue.rank, venue.label) for venue in found}) != 1:
            return None
        return found[0]

    def match_journal_metrics(self, issns: Iterable[str]) -> dict[str, Any]:
        result: dict[str, Any] = {"jcrQuartile": None, "casQuartile": None, "impactFactor": None}
        if not self.available or self._db is None:
            return result
        for issn in dict.fromkeys(str(value).strip().upper() for value in issns if str(value).strip()):
            with self._lock:
                rows = self._db.execute(
                    "SELECT factor, jcr, zky FROM factor WHERE upper(issn)=? OR upper(eissn)=? LIMIT 3", (issn, issn)
                ).fetchall()
            if len({tuple(row) for row in rows}) != 1:
                continue  # unmatched or ambiguous ISSN
            factor, jcr, zky = rows[0]
            jcr_match = QUARTILE.match(str(jcr or "").strip())
            cas_match = QUARTILE.match(str(zky or "").strip())
            result["jcrQuartile"] = f"Q{jcr_match.group(1)}" if jcr_match else None
            result["casQuartile"] = int(cas_match.group(1)) if cas_match else None
            result["impactFactor"] = round(float(factor), 3) if isinstance(factor, (int, float)) and factor > 0 else None
            return result
        return result

    def describe(self, doi: str | None, venue_name: str | None, venue_type: str | None, issns: Iterable[str]) -> dict[str, Any]:
        """Return the provider-neutral rank fields for one paper."""
        envelope: dict[str, Any] = {"ccfRank": None, "ccfVenue": None, "jcrQuartile": None, "casQuartile": None, "impactFactor": None, "rankVersion": None}
        if not self.available:
            return envelope
        ccf = self.match_ccf(doi, venue_name, venue_type)
        if ccf is not None:
            envelope["ccfRank"], envelope["ccfVenue"] = ccf.rank, ccf.label
        envelope.update(self.match_journal_metrics(issns))
        envelope["rankVersion"] = self.version
        return envelope
