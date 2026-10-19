"""Offline tests for the IEEE source and venue-rank matching.

Run from services/paperscraper:  python -m unittest test_ieee -v
"""

from __future__ import annotations

import io
import json
import shutil
import tempfile
import unittest
import urllib.error
import urllib.parse
from email.message import Message
from pathlib import Path

from ieee import IEEESource, SourceQuotaExhausted, SourceRateLimited, build_filter, build_search_query, normalize_doi, reconstruct_abstract
from venue_ranks import VenueRanks, ieee_doi_code

DATA_DIR = Path(__file__).resolve().parent / "data"
RANKS = VenueRanks(DATA_DIR)


def work(doi, title="A Paper", venue="IEEE Access", venue_type="journal", issn=("2169-3536",), abstract=True, cited=7, pdf=None):
    return {
        "id": "https://openalex.org/W1",
        "doi": f"https://doi.org/{doi}" if doi else None,
        "display_name": title,
        "publication_date": "2024-05-01",
        "primary_location": {"source": {"display_name": venue, "type": venue_type, "issn": list(issn), "issn_l": issn[0] if issn else None}},
        "best_oa_location": {"pdf_url": pdf} if pdf else None,
        "abstract_inverted_index": {"Graph": [0], "networks": [1], "work.": [2]} if abstract else None,
        "cited_by_count": cited,
    }


class FakeResponse(io.BytesIO):
    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()


class FakeHTTP:
    """Records requests and replays queued JSON bodies or HTTP errors."""

    def __init__(self, *responses):
        self.responses = list(responses)
        self.requests = []

    def __call__(self, request, timeout):
        self.requests.append(request)
        item = self.responses.pop(0)
        if isinstance(item, Exception):
            raise item
        return FakeResponse(json.dumps(item).encode())


def http_error(code, remaining=None):
    headers = Message()
    if remaining is not None:
        headers["X-RateLimit-Remaining"] = str(remaining)
    return urllib.error.HTTPError("https://api.openalex.org/works", code, "error", headers, io.BytesIO(b"{}"))


class RankDataTest(unittest.TestCase):
    def test_pinned_data_loads_and_verifies(self):
        self.assertTrue(RANKS.available)
        self.assertEqual(RANKS.version, "ccfrank4dblp@86e59cc;impact_factor@1.1.4")

    def test_tampered_data_disables_ranks_instead_of_guessing(self):
        with tempfile.TemporaryDirectory() as directory:
            for name in ("manifest.json", "ccf_rank_list.tsv", "impact_factor.sqlite3"):
                shutil.copy(DATA_DIR / name, Path(directory) / name)
            with (Path(directory) / "ccf_rank_list.tsv").open("a", encoding="utf-8") as handle:
                handle.write("A\tFAKE\tFake Venue\t/conf/fake/fake\n")
            tampered = VenueRanks(Path(directory))
            self.assertFalse(tampered.available)
            self.assertIsNone(tampered.describe("10.1109/cvpr52733.2024.1", "CVPR", "conference", [])["ccfRank"])


class CCFMatchTest(unittest.TestCase):
    def ccf(self, doi, venue=None, venue_type=None):
        match = RANKS.match_ccf(doi, venue, venue_type)
        return (match.rank, match.label) if match else None

    def test_doi_code_extraction(self):
        self.assertEqual(ieee_doi_code("10.1109/CVPR52733.2024.01167"), "cvpr")
        self.assertEqual(ieee_doi_code("10.1109/tnnls.2024.3397926"), "tnnls")
        self.assertIsNone(ieee_doi_code("10.1109/5.771073"))
        self.assertIsNone(ieee_doi_code("10.48550/arXiv.2401.00001"))

    def test_conference_and_journal_codes(self):
        self.assertEqual(self.ccf("10.1109/cvpr52733.2024.01167", "IEEE Computer Society Conference on Computer Vision and Pattern Recognition", "conference"), ("A", "CVPR"))
        self.assertEqual(self.ccf("10.1109/tpami.2023.3263585", "IEEE Transactions on Pattern Analysis and Machine Intelligence", "journal"), ("A", "TPAMI"))
        self.assertEqual(self.ccf("10.1109/tnnls.2024.3397926", None, "journal"), ("B", "TNNLS"))
        self.assertEqual(self.ccf("10.1109/sp46215.2023.10179300", None, "conference"), ("A", "S&P"))

    def test_aliases_for_codes_that_differ(self):
        self.assertEqual(self.ccf("10.1109/jiot.2023.3300000", None, "journal"), ("C", "IOT"))
        self.assertEqual(self.ccf("10.1109/tnet.2023.3300000", None, "journal"), ("A", "TON"))

    def test_kind_mismatch_is_rejected(self):
        # IEEE Micro uses the DOI code "mm", which is also the DBLP key of ACM MM.
        self.assertIsNone(self.ccf("10.1109/mm.2024.3360000", "IEEE Micro", "journal"))
        self.assertIsNone(self.ccf("10.1109/cvpr52733.2024.01167", None, "journal"))

    def test_unknown_venue_type_fails_closed_for_codes(self):
        self.assertIsNone(self.ccf("10.1109/cvpr52733.2024.01167", None, None))
        self.assertIsNone(self.ccf("10.1109/cc.2024.1", None, "book series"))

    def test_unknown_venue_is_unmatched(self):
        self.assertIsNone(self.ccf("10.1109/access.2024.3439352", "IEEE Access", "journal"))

    def test_exact_name_fallback(self):
        self.assertEqual(self.ccf("10.1109/5.771073", "IEEE Transactions on Computers", "journal"), ("A", "TC"))
        self.assertIsNone(self.ccf("10.1109/5.771073", "IEEE Transactions on Computer", "journal"))


class JournalMetricsTest(unittest.TestCase):
    def test_issn_lookup(self):
        metrics = RANKS.match_journal_metrics(["2162-237X", "2162-2388"])
        self.assertEqual(metrics["jcrQuartile"], "Q1")
        self.assertEqual(metrics["casQuartile"], 1)
        self.assertAlmostEqual(metrics["impactFactor"], 9.7, places=1)

    def test_unmatched_issn(self):
        self.assertEqual(RANKS.match_journal_metrics(["1063-6919"]), {"jcrQuartile": None, "casQuartile": None, "impactFactor": None})


class QueryTest(unittest.TestCase):
    def test_boolean_query_and_filters(self):
        spec = {"concepts": [{"concept": "图神经网络", "synonyms": ["graph neural network", "GNN"]}, {"concept": "入侵检测", "synonyms": ["intrusion detection"]}], "yearFrom": 2024}
        self.assertEqual(build_search_query(spec), '("graph neural network" OR GNN) AND "intrusion detection"')
        self.assertEqual(build_filter(spec), "primary_location.source.host_organization:P4310319808,has_doi:true,from_publication_date:2024-01-01")

    def test_helpers(self):
        self.assertEqual(normalize_doi("https://doi.org/10.1109/TNNLS.2024.1"), "10.1109/tnnls.2024.1")
        self.assertIsNone(normalize_doi("https://doi.org/10.1145/1234"))
        self.assertEqual(reconstruct_abstract({"b": [1], "a": [0, 2]}), "a b a")
        self.assertIsNone(reconstruct_abstract(None))


class SearchTest(unittest.TestCase):
    payload = {"spec": {"source": "ieee", "concepts": [{"concept": "x", "synonyms": ["graph"]}], "exclude": ["survey"]}, "limit": 3,
               "exclude": [{"source": "ieee", "externalId": "10.1109/access.2024.2"}]}

    def test_maps_filters_and_ranks(self):
        http = FakeHTTP({"results": [
            work("10.1109/ACCESS.2024.1", title="<i>Graph</i> nets", pdf="https://ieeexplore.ieee.org/stamp/1.pdf"),
            work("10.1109/access.2024.2"),  # already shown
            work("10.1145/3000000"),  # not IEEE
            work(None),  # no DOI
            work("10.1109/access.2024.3", title="A survey of graphs"),  # excluded term
            work("10.1109/tnnls.2024.4", venue="IEEE Transactions on Neural Networks and Learning Systems", issn=("2162-237X", "2162-2388")),
        ]})
        source = IEEESource(RANKS, fetch=http, sleep=lambda _: None)
        records = source.search(self.payload, 100)
        self.assertEqual([r["externalId"] for r in records], ["10.1109/access.2024.1", "10.1109/tnnls.2024.4"])
        first, second = records
        self.assertEqual(first["title"], "Graph nets")
        self.assertEqual(first["abstract"], "Graph networks work.")
        self.assertEqual(first["landingUrl"], "https://doi.org/10.1109/access.2024.1")
        self.assertEqual(first["openAccessPdfUrl"], "https://ieeexplore.ieee.org/stamp/1.pdf")
        self.assertEqual((first["jcrQuartile"], first["ccfRank"]), ("Q2", None))
        self.assertEqual((second["ccfRank"], second["ccfVenue"], second["jcrQuartile"], second["casQuartile"]), ("B", "TNNLS", "Q1", 1))
        self.assertEqual(second["rankVersion"], RANKS.version)
        query = urllib.parse.parse_qs(urllib.parse.urlparse(http.requests[0].full_url).query)
        self.assertNotIn("api_key", query)
        self.assertEqual(query["filter"][0].split(",")[0], "primary_location.source.host_organization:P4310319808")

    def test_semantic_scholar_fills_missing_abstracts_once(self):
        http = FakeHTTP({"results": [work("10.1109/access.2024.1", abstract=False), work("10.1109/access.2024.5", abstract=False)]},
                        [{"abstract": "Filled abstract."}, None])
        source = IEEESource(RANKS, openalex_api_key="oa-key", semantic_scholar_api_key="s2-key", fetch=http, sleep=lambda _: None)
        records = source.search(self.payload, 100)
        self.assertEqual([r["abstract"] for r in records], ["Filled abstract.", None])
        batch = http.requests[1]
        self.assertEqual(json.loads(batch.data), {"ids": ["DOI:10.1109/access.2024.1", "DOI:10.1109/access.2024.5"]})
        self.assertEqual(batch.get_header("X-api-key"), "s2-key")
        self.assertIn("api_key=oa-key", http.requests[0].full_url)

    def test_semantic_scholar_failure_leaves_abstract_unknown(self):
        http = FakeHTTP({"results": [work("10.1109/access.2024.1", abstract=False)]}, http_error(429))
        records = IEEESource(RANKS, fetch=http, sleep=lambda _: None).search(self.payload, 100)
        self.assertIsNone(records[0]["abstract"])
        self.assertEqual(len(http.requests), 2)

    def test_spent_budget_is_reported_without_retry(self):
        http = FakeHTTP(http_error(429, remaining=0))
        with self.assertRaises(SourceQuotaExhausted):
            IEEESource(RANKS, fetch=http, sleep=lambda _: None).search(self.payload, 100)
        self.assertEqual(len(http.requests), 1)

    def test_per_second_limit_is_retried(self):
        http = FakeHTTP(http_error(429, remaining=50), {"results": [work("10.1109/access.2024.1")]})
        records = IEEESource(RANKS, fetch=http, sleep=lambda _: None).search(self.payload, 100)
        self.assertEqual(len(records), 1)

    def test_persistent_rate_limit_is_not_reported_as_spent_budget(self):
        http = FakeHTTP(http_error(429), http_error(429), http_error(429))
        with self.assertRaises(SourceRateLimited):
            IEEESource(RANKS, fetch=http, sleep=lambda _: None).search(self.payload, 100)

    def test_records_dropped_after_abstract_fill_are_replaced(self):
        payload = {"spec": {"source": "ieee", "concepts": [{"concept": "x", "synonyms": ["graph"]}], "exclude": ["survey"]}, "limit": 1}
        http = FakeHTTP(
            {"results": [work("10.1109/access.2024.1", abstract=False), work("10.1109/access.2024.2")]},
            [{"abstract": "A survey of graphs."}],
        )
        records = IEEESource(RANKS, fetch=http, sleep=lambda _: None).search(payload, 100)
        self.assertEqual([r["externalId"] for r in records], ["10.1109/access.2024.2"])

    def test_lookup_by_doi(self):
        http = FakeHTTP(work("10.1109/access.2024.1", pdf="https://example.org/a.pdf"))
        record = IEEESource(RANKS, fetch=http, sleep=lambda _: None).lookup("10.1109/ACCESS.2024.1")
        self.assertEqual(record["openAccessPdfUrl"], "https://example.org/a.pdf")
        self.assertIn("/works/doi:10.1109/access.2024.1", http.requests[0].full_url)
        with self.assertRaises(KeyError):
            IEEESource(RANKS, fetch=FakeHTTP(), sleep=lambda _: None).lookup("10.1145/1")


if __name__ == "__main__":
    unittest.main()
