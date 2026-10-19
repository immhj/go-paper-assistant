package paperscraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"paperassistant/internal/config"
	"paperassistant/internal/domain"
)

// ErrSourceQuotaExhausted is re-exported so callers depend only on the domain sentinel.
var ErrSourceQuotaExhausted = domain.ErrSourceQuotaExhausted

const sourceQuotaExhaustedCode = "SOURCE_QUOTA_EXHAUSTED"

type Client struct {
	baseURL string
	limit   int
	http    *http.Client
}

func New(cfg config.PaperScraperConfig) *Client {
	return &Client{baseURL: cfg.BaseURL, limit: cfg.ResultLimit, http: &http.Client{Timeout: cfg.Timeout}}
}

func (c *Client) Search(ctx context.Context, request domain.PaperSearchRequest) ([]domain.Candidate, error) {
	limit := request.Limit
	if limit < 1 {
		limit = c.limit
	}
	if limit > c.limit {
		limit = c.limit
	}
	body, err := json.Marshal(map[string]any{"spec": request.Spec, "limit": limit, "exclude": request.Exclude})
	if err != nil {
		return nil, err
	}
	var response struct {
		Papers []paperResponse `json:"papers"`
	}
	attempts := 3
	if request.Spec.EffectiveSource() == domain.PaperSourceIEEE {
		// Every IEEE search spends OpenAlex daily budget (up to several paid pages), so a
		// failed search is reported once instead of being replayed (ADR 0018).
		attempts = 1
	}
	if err = c.requestJSONAttempts(ctx, http.MethodPost, c.baseURL+"/v1/search", body, &response, attempts); err != nil {
		return nil, err
	}
	excluded := make(map[string]struct{}, len(request.Exclude))
	for _, identity := range request.Exclude {
		source := strings.TrimSpace(strings.ToLower(identity.Source))
		externalID := strings.TrimSpace(identity.ExternalID)
		if source != "" && externalID != "" {
			excluded[source+"\x00"+externalID] = struct{}{}
		}
	}
	items := make([]domain.Candidate, 0, len(response.Papers))
	seen := make(map[string]struct{})
	for _, paper := range response.Papers {
		candidate, ok := paper.candidate()
		if !ok {
			continue
		}
		key := candidate.Source + "\x00" + candidate.ExternalID
		if _, skip := excluded[key]; skip {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		items = append(items, candidate)
		if len(items) >= limit {
			break
		}
	}
	return items, nil
}

func (c *Client) GetByID(ctx context.Context, source, externalID string) (domain.Candidate, error) {
	var response paperResponse
	endpoint := c.baseURL + "/v1/papers/" + url.PathEscape(source) + "/" + url.PathEscape(externalID)
	if err := c.requestJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return domain.Candidate{}, err
	}
	candidate, ok := response.candidate()
	if !ok {
		return domain.Candidate{}, errors.New("paperscraper lookup returned no stable identity")
	}
	return candidate, nil
}

func (c *Client) Health(ctx context.Context) error {
	var response struct {
		Status string `json:"status"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, c.baseURL+"/healthz", nil, &response); err != nil {
		return err
	}
	if response.Status != "ok" {
		return fmt.Errorf("paperscraper health status %q", response.Status)
	}
	return nil
}

func (c *Client) requestJSON(ctx context.Context, method, endpoint string, body []byte, target any) error {
	return c.requestJSONAttempts(ctx, method, endpoint, body, target, 3)
}

func (c *Client) requestJSONAttempts(ctx context.Context, method, endpoint string, body []byte, target any, attempts int) error {
	last := attempts - 1
	for attempt := 0; attempt < attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt == last {
				return fmt.Errorf("paperscraper request: %w", err)
			}
			continue
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusTooManyRequests && strings.Contains(string(data), sourceQuotaExhaustedCode) {
				// A spent daily budget does not recover within the retry window.
				return ErrSourceQuotaExhausted
			}
			if attempt == last {
				return fmt.Errorf("paperscraper status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
			}
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("paperscraper status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		if err = json.Unmarshal(data, target); err != nil {
			return fmt.Errorf("decode paperscraper response: %w", err)
		}
		return nil
	}
	return errors.New("paperscraper retries exhausted")
}

type paperResponse struct {
	Source           string  `json:"source"`
	ExternalID       string  `json:"externalId"`
	ArxivID          string  `json:"arxivId"`
	DOI              string  `json:"doi"`
	Title            string  `json:"title"`
	Abstract         *string `json:"abstract"`
	PublicationDate  string  `json:"publicationDate"`
	Venue            string  `json:"venue"`
	CitationCount    *uint64 `json:"citationCount"`
	TLDR             *string `json:"tldr"`
	OpenAccessPDFURL string  `json:"openAccessPdfUrl"`
	// IEEE-only fields (ADR 0018, 0020). Ranks are trusted only within their closed domains.
	ISSN         []string `json:"issn"`
	CCFRank      *string  `json:"ccfRank"`
	CCFVenue     *string  `json:"ccfVenue"`
	JCRQuartile  *string  `json:"jcrQuartile"`
	CASQuartile  *int     `json:"casQuartile"`
	ImpactFactor *float64 `json:"impactFactor"`
	RankVersion  *string  `json:"rankVersion"`
}

func (p paperResponse) candidate() (domain.Candidate, bool) {
	p.Source = strings.TrimSpace(strings.ToLower(p.Source))
	p.ExternalID = strings.TrimSpace(p.ExternalID)
	if p.Source == "" || p.ExternalID == "" {
		return domain.Candidate{}, false
	}
	candidate := domain.Candidate{Paper: domain.Paper{
		Source: p.Source, ExternalID: p.ExternalID, Title: strings.TrimSpace(p.Title), Abstract: p.Abstract,
		FullTextStatus: "not_fetched",
	}, DOI: p.DOI, ArxivID: p.ArxivID, OpenAccessPDFURL: p.OpenAccessPDFURL}
	if candidate.Title == "" {
		// Lookup responses need only identity and PDF URL; search responses need a title.
		candidate.Title = p.ExternalID
	}
	if p.PublicationDate != "" {
		if parsed, err := time.Parse("2006-01-02", p.PublicationDate); err == nil {
			candidate.PublicationDate = &parsed
		}
	}
	if p.Venue != "" {
		candidate.VenueRaw = &p.Venue
	}
	candidate.TLDR = p.TLDR
	if p.CitationCount != nil {
		candidate.CitationCount = *p.CitationCount
		candidate.CitationKnown = true
	}
	if candidate.Source == domain.PaperSourceIEEE {
		doi := strings.ToLower(candidate.ExternalID)
		if !strings.HasPrefix(doi, "10.1109/") {
			return domain.Candidate{}, false
		}
		candidate.ExternalID, candidate.DOI = doi, doi
		candidate.Paper.DOI = &doi
		applyVenueRanks(&candidate, p)
	}
	return candidate, true
}

// applyVenueRanks copies sidecar rank fields after validating their closed value sets, so a
// malformed sidecar response can never surface as an invented rank.
func applyVenueRanks(candidate *domain.Candidate, p paperResponse) {
	issns := make([]string, 0, len(p.ISSN))
	for _, value := range p.ISSN {
		if value = strings.ToUpper(strings.TrimSpace(value)); issnPattern.MatchString(value) {
			issns = append(issns, value)
		}
	}
	if len(issns) > 0 {
		joined := strings.Join(issns, ",")
		candidate.ISSN = &joined
	}
	if p.RankVersion == nil || strings.TrimSpace(*p.RankVersion) == "" {
		return
	}
	version := strings.TrimSpace(*p.RankVersion)
	candidate.RankVersion = &version
	if p.CCFRank != nil && (*p.CCFRank == "A" || *p.CCFRank == "B" || *p.CCFRank == "C") {
		rank := *p.CCFRank
		candidate.CCFRank = &rank
		if p.CCFVenue != nil {
			candidate.CCFVenue = strings.TrimSpace(*p.CCFVenue)
		}
	}
	if p.JCRQuartile != nil && jcrPattern.MatchString(*p.JCRQuartile) {
		quartile := *p.JCRQuartile
		candidate.JCRQuartile = &quartile
	}
	if p.CASQuartile != nil && *p.CASQuartile >= 1 && *p.CASQuartile <= 4 {
		quartile := *p.CASQuartile
		candidate.CASQuartile = &quartile
	}
	if p.ImpactFactor != nil && *p.ImpactFactor > 0 && *p.ImpactFactor < 10000 {
		factor := *p.ImpactFactor
		candidate.ImpactFactor = &factor
	}
}

var (
	issnPattern = regexp.MustCompile(`^\d{4}-\d{3}[\dX]$`)
	jcrPattern  = regexp.MustCompile(`^Q[1-4]$`)
)
