"""Small HTTP adapter around jannisborn/paperscraper plus the IEEE source.

The Go application owns product semantics. This sidecar owns Python/library details
and returns a stable provider-neutral envelope. Sources: arXiv via paperscraper
(ADR 0009) and IEEE via OpenAlex with deterministic venue ranks (ADR 0018, 0020).
"""

from __future__ import annotations

import json
import logging
import math
import os
import re
import threading
from datetime import date
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any
from urllib.parse import unquote, urlparse

from pathlib import Path

import paperscraper
from paperscraper.arxiv.arxiv import get_arxiv_papers_api
from paperscraper.arxiv.utils import get_query_from_keywords

from ieee import IEEESource, SourceQuotaExhausted, SourceRateLimited
from spec_terms import excluded_identities as spec_excluded_identities
from spec_terms import matches_exclusions, terms_from_spec
from venue_ranks import VenueRanks

LOG = logging.getLogger("paperscraper-sidecar")
logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO"))

HOST = os.getenv("PAPERSCRAPER_HOST", "0.0.0.0")
PORT = int(os.getenv("PAPERSCRAPER_PORT", "8090"))
MAX_RESULTS = max(1, min(int(os.getenv("PAPERSCRAPER_MAX_RESULTS", "100")), 100))
MAX_SCAN_RESULTS = max(MAX_RESULTS, min(int(os.getenv("PAPERSCRAPER_MAX_SCAN_RESULTS", "500")), 1000))
ARXIV_DELAY_SECONDS = max(3.0, float(os.getenv("PAPERSCRAPER_ARXIV_DELAY_SECONDS", "3")))
ARXIV_RETRIES = max(0, min(int(os.getenv("PAPERSCRAPER_ARXIV_RETRIES", "3")), 10))
ENABLE_CITATIONS = os.getenv("PAPERSCRAPER_CITATIONS", "false").lower() == "true"
ARXIV_ID = re.compile(
    r"^(?:[a-z-]+(?:\.[A-Z]{2})?/\d{7}|\d{4}\.\d{4,5})(?:v\d+)?$", re.I
)
QUERY_LOCK = threading.Lock()
RANK_DATA_DIR = Path(os.getenv("PAPERSCRAPER_RANK_DATA_DIR", str(Path(__file__).resolve().parent / "data")))
RANKS = VenueRanks(RANK_DATA_DIR)
IEEE = IEEESource(
    RANKS,
    openalex_api_key=os.getenv("OPENALEX_API_KEY", ""),
    semantic_scholar_api_key=os.getenv("SEMANTIC_SCHOLAR_API_KEY", ""),
    timeout=float(os.getenv("PAPERSCRAPER_IEEE_TIMEOUT_SECONDS", "30")),
)


def clean(value: Any) -> Any:
    if value is None:
        return None
    if isinstance(value, float) and math.isnan(value):
        return None
    if hasattr(value, "item"):
        return clean(value.item())
    return value


def arxiv_id_from_doi(value: Any) -> str | None:
    doi = str(clean(value) or "")
    prefix = "10.48550/arXiv."
    if doi.lower().startswith(prefix.lower()):
        result = re.sub(r"v\d+$", "", doi[len(prefix) :], flags=re.I)
        return result if ARXIV_ID.match(result) else None
    return None


def citation_count(doi: str | None) -> int | None:
    if not ENABLE_CITATIONS or not doi:
        return None
    try:
        from paperscraper.citations import get_citations_by_doi

        return int(get_citations_by_doi(doi))
    except Exception:  # upstream sources are optional; unknown is not zero
        LOG.exception("citation enrichment failed", extra={"doi": doi})
        return None


def search(payload: dict[str, Any]) -> list[dict[str, Any]]:
    spec = payload.get("spec") or {}
    source = str(spec.get("source") or "arxiv").strip().lower()
    if source == "ieee":
        return IEEE.search(payload, MAX_RESULTS)
    if source != "arxiv":
        raise ValueError(f"unsupported search source {source!r}")
    limit = max(1, min(int(payload.get("limit") or 5), MAX_RESULTS))
    excluded_identities = spec_excluded_identities(payload)
    keywords = terms_from_spec(spec)
    year_from, year_to = spec.get("yearFrom"), spec.get("yearTo")
    start = f"{int(year_from)}-01-01" if year_from else "None"
    end = f"{int(year_to)}-12-31" if year_to else "None"
    query = get_query_from_keywords(keywords, start_date=start, end_date=end)
    # Scan beyond the output limit when previous results are excluded. This is
    # a bounded provider pagination budget, independent from the requested count.
    fetch_limit = min(max(limit * 4, len(excluded_identities) + limit * 4), MAX_SCAN_RESULTS)

    # arXiv asks clients to serialize calls and wait between them. paperscraper's
    # arxiv.Client handles page delays; the lock prevents concurrent HTTP handlers.
    with QUERY_LOCK:
        frame = get_arxiv_papers_api(
            query=query,
            fields=["title", "authors", "date", "abstract", "journal", "doi"],
            max_results=fetch_limit,
            client_options={
                "page_size": min(fetch_limit, 100),
                "delay_seconds": ARXIV_DELAY_SECONDS,
                "num_retries": ARXIV_RETRIES,
            },
            verbose=False,
        )

    result: list[dict[str, Any]] = []
    excluded = [str(x) for x in spec.get("exclude") or []]
    for raw in frame.to_dict(orient="records"):
        record = {key: clean(value) for key, value in raw.items()}
        identifier = arxiv_id_from_doi(record.get("doi"))
        title = str(record.get("title") or "").strip()
        if not identifier or not title or ("arxiv", identifier) in excluded_identities or not matches_exclusions(record, excluded):
            continue
        doi = f"10.48550/arXiv.{identifier}"
        result.append(
            {
                "source": "arxiv",
                "externalId": identifier,
                "arxivId": identifier,
                "doi": doi,
                "title": title,
                "abstract": str(record.get("abstract") or "").strip() or None,
                "authors": str(record.get("authors") or "").strip() or None,
                "publicationDate": record.get("date"),
                "venue": str(record.get("journal") or "").strip() or "arXiv",
                "citationCount": citation_count(doi),
                "tldr": None,
                "openAccessPdfUrl": f"https://arxiv.org/pdf/{identifier}",
            }
        )
        if len(result) >= limit:
            break
    return result


def lookup(source: str, external_id: str) -> dict[str, Any]:
    if source == "ieee":
        return IEEE.lookup(external_id)
    if source != "arxiv" or not ARXIV_ID.match(external_id):
        raise KeyError("paper identity is not supported")
    identifier = re.sub(r"v\d+$", "", external_id, flags=re.I)
    return {
        "source": "arxiv",
        "externalId": identifier,
        "arxivId": identifier,
        "openAccessPdfUrl": f"https://arxiv.org/pdf/{identifier}",
    }


class Handler(BaseHTTPRequestHandler):
    server_version = "paperscraper-sidecar/1"

    def log_message(self, fmt: str, *args: Any) -> None:
        LOG.info("%s %s", self.address_string(), fmt % args)

    def send_json(self, status: int, value: Any) -> None:
        body = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path == "/healthz":
            self.send_json(HTTPStatus.OK, {
                "status": "ok", "paperscraperVersion": paperscraper.__version__, "sources": ["arxiv", "ieee"],
                "rankData": RANKS.available, "rankVersion": RANKS.version or None,
            })
            return
        parts = parsed.path.strip("/").split("/")
        if len(parts) == 4 and parts[:2] == ["v1", "papers"]:
            try:
                self.send_json(HTTPStatus.OK, lookup(unquote(parts[2]), unquote(parts[3])))
            except KeyError as exc:
                self.send_json(HTTPStatus.NOT_FOUND, {"error": str(exc)})
            except SourceQuotaExhausted as exc:
                self.send_json(HTTPStatus.TOO_MANY_REQUESTS, {"error": str(exc), "code": "SOURCE_QUOTA_EXHAUSTED"})
            except Exception as exc:
                LOG.exception("lookup failed")
                self.send_json(HTTPStatus.BAD_GATEWAY, {"error": str(exc)})
            return
        self.send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802
        if urlparse(self.path).path != "/v1/search":
            self.send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            length = int(self.headers.get("Content-Length", "0"))
            if length <= 0 or length > 1_000_000:
                raise ValueError("invalid request size")
            payload = json.loads(self.rfile.read(length))
            self.send_json(HTTPStatus.OK, {"papers": search(payload)})
        except SourceQuotaExhausted as exc:
            # Distinct code so the Go client does not burn retries on a spent daily budget.
            self.send_json(HTTPStatus.TOO_MANY_REQUESTS, {"error": str(exc), "code": "SOURCE_QUOTA_EXHAUSTED"})
        except SourceRateLimited as exc:
            self.send_json(HTTPStatus.SERVICE_UNAVAILABLE, {"error": str(exc), "code": "SOURCE_RATE_LIMITED"})
        except ValueError as exc:
            self.send_json(HTTPStatus.BAD_REQUEST, {"error": str(exc)})
        except Exception as exc:
            LOG.exception("search failed")
            self.send_json(HTTPStatus.BAD_GATEWAY, {"error": str(exc)})


if __name__ == "__main__":
    LOG.info("starting paperscraper sidecar on %s:%d", HOST, PORT)
    ThreadingHTTPServer((HOST, PORT), Handler).serve_forever()
