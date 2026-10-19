"""IEEE data source backed by OpenAlex, with Semantic Scholar abstract fill-in (ADR 0018).

Identity is ``source='ieee'`` plus the lower-case IEEE DOI (``10.1109/...``). Records without
such a DOI are skipped. Only the standard library is used so this module stays testable
without the paperscraper dependency stack.
"""

from __future__ import annotations

import json
import logging
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Callable

from spec_terms import excluded_identities, matches_exclusions, terms_from_spec
from venue_ranks import VenueRanks

LOG = logging.getLogger("paperscraper-sidecar.ieee")

OPENALEX_WORKS = "https://api.openalex.org/works"
SEMANTIC_SCHOLAR_BATCH = "https://api.semanticscholar.org/graph/v1/paper/batch"
# OpenAlex publisher ID of the Institute of Electrical and Electronics Engineers.
IEEE_PUBLISHER = "P4310319808"
IEEE_DOI = re.compile(r"^10\.1109/\S+$")
DOI_URL_PREFIXES = ("https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:")
TAG = re.compile(r"<[^>]+>")
SELECT_FIELDS = "id,doi,display_name,publication_date,primary_location,best_oa_location,abstract_inverted_index,cited_by_count"
MAX_PAGES = 3
PER_PAGE = 100

Fetch = Callable[[urllib.request.Request, float], Any]


class SourceQuotaExhausted(Exception):
    """The keyless or keyed OpenAlex daily budget is spent; retrying today will not help."""


class SourceRateLimited(Exception):
    """OpenAlex kept refusing with a per-second 429; transient, not a spent daily budget."""


def default_fetch(request: urllib.request.Request, timeout: float) -> Any:
    return urllib.request.urlopen(request, timeout=timeout)  # noqa: S310 - fixed HTTPS hosts


def normalize_doi(value: Any) -> str | None:
    doi = str(value or "").strip()
    lowered = doi.lower()
    for prefix in DOI_URL_PREFIXES:
        if lowered.startswith(prefix):
            doi = doi[len(prefix):]
            break
    doi = doi.strip().lower()
    return doi if IEEE_DOI.match(doi) else None


def reconstruct_abstract(index: Any) -> str | None:
    """OpenAlex ships abstracts as an inverted index: word -> list of positions."""
    if not isinstance(index, dict) or not index:
        return None
    positions: dict[int, str] = {}
    for word, places in index.items():
        for place in places or []:
            if isinstance(place, int) and place >= 0:
                positions[place] = str(word)
    if not positions:
        return None
    text = " ".join(positions[i] for i in sorted(positions))
    return text.strip() or None


def clean_title(value: Any) -> str:
    return " ".join(TAG.sub("", str(value or "")).split())


def quote_term(term: str) -> str:
    term = " ".join(term.replace('"', " ").split())
    return f'"{term}"' if " " in term or "-" in term else term


def build_search_query(spec: dict[str, Any]) -> str:
    """Translate the spec into OpenAlex boolean search: AND across concepts, OR across synonyms."""
    clauses = []
    for group in terms_from_spec(spec):
        terms = [quote_term(term) for term in ([group] if isinstance(group, str) else group) if term.strip()]
        terms = [term for term in terms if term]
        if not terms:
            continue
        clauses.append(terms[0] if len(terms) == 1 else "(" + " OR ".join(terms) + ")")
    if not clauses:
        raise ValueError("search spec has no concepts")
    return " AND ".join(clauses)


def build_filter(spec: dict[str, Any]) -> str:
    filters = [f"primary_location.source.host_organization:{IEEE_PUBLISHER}", "has_doi:true"]
    if spec.get("yearFrom"):
        filters.append(f"from_publication_date:{int(spec['yearFrom'])}-01-01")
    if spec.get("yearTo"):
        filters.append(f"to_publication_date:{int(spec['yearTo'])}-12-31")
    return ",".join(filters)


class IEEESource:
    def __init__(
        self,
        ranks: VenueRanks,
        openalex_api_key: str = "",
        semantic_scholar_api_key: str = "",
        timeout: float = 30.0,
        fetch: Fetch = default_fetch,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.ranks = ranks
        self.openalex_api_key = openalex_api_key.strip()
        self.semantic_scholar_api_key = semantic_scholar_api_key.strip()
        self.timeout = timeout
        self.fetch = fetch
        self.sleep = sleep

    # -- HTTP -----------------------------------------------------------------------------

    def _openalex(self, url: str, params: dict[str, Any]) -> dict[str, Any]:
        if self.openalex_api_key:
            params = {**params, "api_key": self.openalex_api_key}
        query = urllib.parse.urlencode(params)
        request = urllib.request.Request(url + ("?" + query if query else ""), headers={"User-Agent": "paper-assistant-sidecar/1", "Accept": "application/json"})
        for attempt in range(3):
            try:
                with self.fetch(request, self.timeout) as response:
                    return json.load(response)
            except urllib.error.HTTPError as exc:
                if exc.code != 429:
                    raise
                remaining = exc.headers.get("X-RateLimit-Remaining") if exc.headers else None
                try:
                    budget_spent = remaining is not None and float(remaining) <= 0
                except ValueError:
                    budget_spent = False
                if budget_spent:
                    raise SourceQuotaExhausted("OpenAlex daily budget exhausted") from exc
                if attempt == 2:
                    # Only the budget header proves the daily budget is spent.
                    raise SourceRateLimited("OpenAlex rate limited") from exc
                self.sleep(1.5 * (attempt + 1))
        raise SourceRateLimited("OpenAlex rate limited")

    def _fill_abstracts(self, records: list[dict[str, Any]]) -> None:
        missing = [record for record in records if not record.get("abstract")]
        if not missing:
            return
        body = json.dumps({"ids": [f"DOI:{record['doi']}" for record in missing]}).encode()
        headers = {"Content-Type": "application/json", "User-Agent": "paper-assistant-sidecar/1"}
        if self.semantic_scholar_api_key:
            headers["x-api-key"] = self.semantic_scholar_api_key
        request = urllib.request.Request(SEMANTIC_SCHOLAR_BATCH + "?fields=abstract", data=body, headers=headers, method="POST")
        try:
            # One batch call, no retry: a shared-pool 429 simply leaves abstracts unknown.
            with self.fetch(request, min(self.timeout, 15.0)) as response:
                items = json.load(response)
        except Exception as exc:  # noqa: BLE001 - abstract fill-in is optional
            LOG.warning("semantic scholar abstract fill-in skipped: %s", exc)
            return
        if not isinstance(items, list):
            return
        for record, item in zip(missing, items):
            abstract = (item or {}).get("abstract") if isinstance(item, dict) else None
            if isinstance(abstract, str) and abstract.strip():
                record["abstract"] = abstract.strip()

    # -- Mapping --------------------------------------------------------------------------

    def envelope(self, work: dict[str, Any]) -> dict[str, Any] | None:
        doi = normalize_doi(work.get("doi"))
        title = clean_title(work.get("display_name") or work.get("title"))
        if not doi or not title:
            return None
        location = work.get("primary_location") or {}
        source = location.get("source") or {}
        venue = clean_title(source.get("display_name")) or None
        venue_type = str(source.get("type") or "").strip().lower() or None
        issns = [str(value) for value in (source.get("issn") or []) if value]
        if not issns and source.get("issn_l"):
            issns = [str(source["issn_l"])]
        best = work.get("best_oa_location") or {}
        pdf_url = str(best.get("pdf_url") or "")
        cited = work.get("cited_by_count")
        record = {
            "source": "ieee",
            "externalId": doi,
            "doi": doi,
            "title": title,
            "abstract": reconstruct_abstract(work.get("abstract_inverted_index")),
            "authors": None,
            "publicationDate": work.get("publication_date"),
            "venue": venue,
            "venueType": venue_type,
            "issn": issns,
            "citationCount": int(cited) if isinstance(cited, int) and cited >= 0 else None,
            "tldr": None,
            "openAccessPdfUrl": pdf_url if pdf_url.lower().startswith("https://") else "",
            "landingUrl": f"https://doi.org/{doi}",
        }
        record.update(self.ranks.describe(doi, venue, venue_type, issns))
        return record

    # -- Public operations ----------------------------------------------------------------

    def search(self, payload: dict[str, Any], max_results: int) -> list[dict[str, Any]]:
        spec = payload.get("spec") or {}
        limit = max(1, min(int(payload.get("limit") or 5), max_results))
        excluded = excluded_identities(payload)
        excluded_terms = [str(x) for x in spec.get("exclude") or []]
        params = {"search": build_search_query(spec), "filter": build_filter(spec), "per_page": PER_PAGE, "select": SELECT_FIELDS}
        # Collect a reserve beyond the limit so records dropped after the abstract fill-in
        # (an excluded term in a newly filled abstract) can be replaced without another page.
        wanted = limit * 2 if excluded_terms else limit
        result: list[dict[str, Any]] = []
        seen: set[str] = set()
        for page in range(1, MAX_PAGES + 1):
            response = self._openalex(OPENALEX_WORKS, {**params, "page": page})
            works = response.get("results") or []
            for work in works:
                record = self.envelope(work)
                if record is None or record["doi"] in seen or ("ieee", record["doi"]) in excluded:
                    continue
                if not matches_exclusions(record, excluded_terms):
                    continue
                seen.add(record["doi"])
                result.append(record)
                if len(result) >= wanted:
                    break
            if len(result) >= limit or len(works) < PER_PAGE:
                break
        self._fill_abstracts(result)
        return [record for record in result if matches_exclusions(record, excluded_terms)][:limit]

    def lookup(self, external_id: str) -> dict[str, Any]:
        doi = normalize_doi(external_id)
        if not doi:
            raise KeyError("paper identity is not supported")
        # Singleton lookups by DOI are free under the OpenAlex budget.
        try:
            work = self._openalex(f"{OPENALEX_WORKS}/doi:{urllib.parse.quote(doi, safe='/')}", {"select": SELECT_FIELDS})
        except urllib.error.HTTPError as exc:
            if exc.code == 404:
                raise KeyError("paper not found") from exc
            raise
        record = self.envelope(work)
        if record is None:
            raise KeyError("paper identity is not supported")
        return record
