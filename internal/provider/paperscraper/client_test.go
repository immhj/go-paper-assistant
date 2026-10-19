package paperscraper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"paperassistant/internal/config"
	"paperassistant/internal/domain"
)

func ptr[T any](value T) *T { return &value }

func TestIEEECandidateKeepsOnlyValidatedRanks(t *testing.T) {
	response := paperResponse{
		Source: "ieee", ExternalID: "10.1109/TNNLS.2024.3397926", Title: "A paper", Venue: "IEEE TNNLS",
		ISSN: []string{"2162-237x", "not-an-issn"}, CCFRank: ptr("B"), CCFVenue: ptr("TNNLS"),
		JCRQuartile: ptr("Q1"), CASQuartile: ptr(1), ImpactFactor: ptr(9.7), RankVersion: ptr("ccfrank4dblp@86e59cc;impact_factor@1.1.4"),
	}
	candidate, ok := response.candidate()
	if !ok {
		t.Fatal("valid IEEE record rejected")
	}
	if candidate.ExternalID != "10.1109/tnnls.2024.3397926" || candidate.Paper.DOI == nil || *candidate.Paper.DOI != candidate.ExternalID {
		t.Fatalf("DOI identity not normalized: %+v", candidate.Paper)
	}
	if candidate.ISSN == nil || *candidate.ISSN != "2162-237X" {
		t.Fatalf("ISSN = %v", candidate.ISSN)
	}
	if *candidate.CCFRank != "B" || candidate.CCFVenue != "TNNLS" || *candidate.JCRQuartile != "Q1" || *candidate.CASQuartile != 1 || *candidate.ImpactFactor != 9.7 {
		t.Fatalf("ranks not mapped: %+v", candidate)
	}

	response.CCFRank, response.JCRQuartile, response.CASQuartile = ptr("A+"), ptr("Q5"), ptr(7)
	candidate, _ = response.candidate()
	if candidate.CCFRank != nil || candidate.JCRQuartile != nil || candidate.CASQuartile != nil {
		t.Fatalf("out-of-domain ranks must be dropped: %+v", candidate.Paper)
	}

	response.RankVersion = nil
	response.CCFRank = ptr("A")
	candidate, _ = response.candidate()
	if candidate.CCFRank != nil || candidate.RankVersion != nil {
		t.Fatal("ranks without a data version are not trusted")
	}

	if _, ok = (paperResponse{Source: "ieee", ExternalID: "10.1145/3000000", Title: "x"}).candidate(); ok {
		t.Fatal("non-IEEE DOI accepted as an IEEE identity")
	}
}

func TestSpentQuotaIsReturnedWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"OpenAlex daily budget exhausted","code":"SOURCE_QUOTA_EXHAUSTED"}`))
	}))
	defer server.Close()
	client := New(config.PaperScraperConfig{BaseURL: server.URL, ResultLimit: 10, Timeout: 5 * time.Second})
	_, err := client.Search(context.Background(), domain.PaperSearchRequest{Spec: domain.SearchSpec{Source: domain.PaperSourceIEEE}, Limit: 3})
	if !errors.Is(err, domain.ErrSourceQuotaExhausted) {
		t.Fatalf("want quota sentinel, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("spent quota was retried %d times", calls.Load())
	}
}

func TestIEEELookupEscapesDOIPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"source":"ieee","externalId":"10.1109/access.2024.1","openAccessPdfUrl":"https://example.org/a.pdf"}`))
	}))
	defer server.Close()
	client := New(config.PaperScraperConfig{BaseURL: server.URL, ResultLimit: 10, Timeout: 5 * time.Second})
	candidate, err := client.GetByID(context.Background(), "ieee", "10.1109/access.2024.1")
	if err != nil || candidate.OpenAccessPDFURL != "https://example.org/a.pdf" {
		t.Fatalf("lookup: %+v %v", candidate, err)
	}
	if gotPath != "/v1/papers/ieee/10.1109%2Faccess.2024.1" {
		t.Fatalf("DOI slash must stay escaped in one path segment, got %q", gotPath)
	}
}
