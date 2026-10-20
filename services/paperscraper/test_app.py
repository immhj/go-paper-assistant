"""Offline HTTP-layer tests for the sidecar: request validation, error-code mapping and the
arXiv result shaping. arXiv and IEEE are replaced by fakes; no network is used.
Run from services/paperscraper:  python -m unittest discover -v
"""
from __future__ import annotations

import http.client
import json
import threading
import unittest
from http.server import ThreadingHTTPServer
from unittest import mock

import app
from ieee import SourceQuotaExhausted, SourceRateLimited


class FakeFrame:
    def __init__(self, records):
        self.records = records

    def to_dict(self, orient):
        assert orient == "records"
        return list(self.records)


def arxiv_record(identifier, title="Graph Networks", abstract="We study graphs.", doi=None):
    return {
        "title": title, "authors": "A. Author", "date": "2024-05-01", "abstract": abstract,
        "journal": None, "doi": doi if doi is not None else f"10.48550/arXiv.{identifier}v2",
    }


SPEC = {"concepts": [{"concept": "图网络", "synonyms": ["graph neural network", "GNN"]}]}


class SidecarHTTPTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), app.Handler)
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def request(self, method, path, body=None, raw=None):
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_address[1], timeout=10)
        payload = raw if raw is not None else (json.dumps(body).encode() if body is not None else None)
        headers = {"Content-Type": "application/json"}
        if payload is not None:
            headers["Content-Length"] = str(len(payload))
        connection.request(method, path, body=payload, headers=headers)
        response = connection.getresponse()
        data = json.loads(response.read() or b"null")
        connection.close()
        return response.status, data

    def test_healthz_reports_sources(self):
        status, data = self.request("GET", "/healthz")
        self.assertEqual(status, 200)
        self.assertEqual(data["status"], "ok")
        self.assertEqual(data["sources"], ["arxiv", "ieee"])

    def test_unknown_paths_are_404(self):
        self.assertEqual(self.request("GET", "/nope")[0], 404)
        self.assertEqual(self.request("POST", "/v1/other", {"spec": SPEC})[0], 404)

    def test_search_rejects_empty_and_oversized_bodies(self):
        self.assertEqual(self.request("POST", "/v1/search", raw=b"")[0], 400)
        # The size check runs before the body is read, so only the declared length is sent.
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_address[1], timeout=10)
        connection.putrequest("POST", "/v1/search")
        connection.putheader("Content-Length", "1000001")
        connection.endheaders()
        response = connection.getresponse()
        self.assertEqual(response.status, 400)
        self.assertIn("invalid request size", json.loads(response.read())["error"])
        connection.close()

    def test_search_rejects_unsupported_source_and_empty_spec(self):
        status, data = self.request("POST", "/v1/search", {"spec": {**SPEC, "source": "acm"}})
        self.assertEqual(status, 400)
        self.assertIn("unsupported", data["error"])
        self.assertEqual(self.request("POST", "/v1/search", {"spec": {"concepts": []}})[0], 400)

    def test_arxiv_search_shapes_filters_and_truncates(self):
        records = [
            arxiv_record("2401.00001"),
            arxiv_record("2401.00002", doi="10.1000/not-arxiv"),  # no arXiv identity: dropped
            arxiv_record("2401.00003", title="A Survey of GNNs"),  # excluded term: dropped
            arxiv_record("2401.00004"),  # previously shown: dropped
            arxiv_record("2401.00005"),
            arxiv_record("2401.00006"),  # beyond the limit
        ]
        captured = {}

        def fake_api(**kwargs):
            captured.update(kwargs)
            return FakeFrame(records)

        payload = {
            "spec": {**SPEC, "exclude": ["survey"], "yearFrom": 2023},
            "limit": 2,
            "exclude": [{"source": "arxiv", "externalId": "2401.00004"}],
        }
        with mock.patch.object(app, "get_arxiv_papers_api", side_effect=fake_api):
            status, data = self.request("POST", "/v1/search", payload)
        self.assertEqual(status, 200)
        papers = data["papers"]
        self.assertEqual([paper["externalId"] for paper in papers], ["2401.00001", "2401.00005"])
        first = papers[0]
        self.assertEqual(first["source"], "arxiv")
        self.assertEqual(first["doi"], "10.48550/arXiv.2401.00001")
        self.assertEqual(first["openAccessPdfUrl"], "https://arxiv.org/pdf/2401.00001")
        self.assertEqual(first["venue"], "arXiv")
        self.assertIsNone(first["citationCount"])
        # Over-fetch: four times the limit plus the excluded identities, never below the limit.
        self.assertEqual(captured["max_results"], 1 + 2 * 4)
        self.assertIn("submittedDate:[20230101", captured["query"])

    def test_ieee_errors_map_to_distinct_codes(self):
        cases = [
            (SourceQuotaExhausted("daily budget spent"), 429, "SOURCE_QUOTA_EXHAUSTED"),
            (SourceRateLimited("slow down"), 503, "SOURCE_RATE_LIMITED"),
        ]
        for error, status_code, code in cases:
            with self.subTest(code=code), mock.patch.object(app.IEEE, "search", side_effect=error):
                status, data = self.request("POST", "/v1/search", {"spec": {**SPEC, "source": "ieee"}})
                self.assertEqual(status, status_code)
                self.assertEqual(data["code"], code)

    def test_unexpected_search_failure_is_bad_gateway(self):
        with mock.patch.object(app, "get_arxiv_papers_api", side_effect=RuntimeError("arXiv down")):
            status, _ = self.request("POST", "/v1/search", {"spec": SPEC})
        self.assertEqual(status, 502)

    def test_lookup_arxiv_strips_version_and_rejects_unknown_identities(self):
        status, data = self.request("GET", "/v1/papers/arxiv/2401.00001v3")
        self.assertEqual(status, 200)
        self.assertEqual(data["externalId"], "2401.00001")
        self.assertEqual(self.request("GET", "/v1/papers/arxiv/not-an-id")[0], 404)
        self.assertEqual(self.request("GET", "/v1/papers/acm/123")[0], 404)

    def test_lookup_ieee_quota_is_429(self):
        with mock.patch.object(app.IEEE, "lookup", side_effect=SourceQuotaExhausted("spent")):
            status, data = self.request("GET", "/v1/papers/ieee/10.1109%2Fexample")
        self.assertEqual(status, 429)
        self.assertEqual(data["code"], "SOURCE_QUOTA_EXHAUSTED")


if __name__ == "__main__":
    unittest.main()
