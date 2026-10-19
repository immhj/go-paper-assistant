package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"paperassistant/internal/auth"
	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/service"
)

type papersTestStore struct {
	dao.Store
	stored []string
}

func (s *papersTestStore) GetPaper(_ context.Context, id uint64) (domain.Paper, error) {
	if id != 5 {
		return domain.Paper{}, dao.ErrNotFound
	}
	doi := "10.1109/access.2024.1"
	return domain.Paper{ID: 5, Source: "ieee", ExternalID: doi, DOI: &doi, Title: "Graph Networks for Intrusion Detection Systems"}, nil
}
func (s *papersTestStore) UpsertAccountFullText(_ context.Context, _, _ uint64, content string) error {
	s.stored = append(s.stored, content)
	return nil
}
func (s *papersTestStore) GetAccountFullTextStatus(_ context.Context, _, paperID uint64) (domain.PrivateFullTextStatus, error) {
	return domain.PrivateFullTextStatus{PaperID: paperID, Present: len(s.stored) > 0}, nil
}

type papersTestExtractor struct{ text string }

func (e papersTestExtractor) Fetch(context.Context, uint64, string) (string, string, error) {
	return "", "", nil
}
func (e papersTestExtractor) ExtractUploaded(_ context.Context, body io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, body)
	return e.text, nil
}

func uploadRequest(actor func(context.Context) context.Context, origin, contentType string) *http.Request {
	req := httptest.NewRequest(http.MethodPut, "http://app.local/api/papers/5/private-fulltext", strings.NewReader("%PDF-1.7"))
	req.Host = "app.local"
	req.SetPathValue("paperId", "5")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	req.Header.Set("Content-Type", contentType)
	if actor != nil {
		req = req.WithContext(actor(req.Context()))
	}
	return req
}

func TestPrivateFullTextUploadEndpoint(t *testing.T) {
	store := &papersTestStore{}
	text := "Graph Networks for Intrusion Detection Systems\nAbstract " + strings.Repeat("body ", 300)
	app := service.New(store, nil, nil, papersTestExtractor{text: text}, nil)
	papers := NewPapers(app, &Platform{}, 1<<20)
	account := func(ctx context.Context) context.Context { return auth.WithAccount(ctx, domain.Account{ID: 7}) }
	guest := func(ctx context.Context) context.Context { return auth.WithGuest(ctx, domain.GuestSession{ID: 3}) }

	cases := []struct {
		name        string
		req         *http.Request
		wantStatus  int
		wantCode    string
		wantStoreed int
	}{
		{"cross-origin upload is refused", uploadRequest(account, "http://evil.example", "application/pdf"), http.StatusForbidden, "ORIGIN_MISMATCH", 0},
		{"guest cannot upload", uploadRequest(guest, "http://app.local", "application/pdf"), http.StatusForbidden, "ACCOUNT_REQUIRED", 0},
		{"non-PDF content type is refused", uploadRequest(account, "http://app.local", "text/html"), http.StatusUnsupportedMediaType, "NOT_PDF", 0},
		{"account upload succeeds", uploadRequest(account, "http://app.local", "application/pdf"), http.StatusOK, "", 1},
	}
	for _, tc := range cases {
		recorder := httptest.NewRecorder()
		papers.UploadPrivateFullText(recorder, tc.req)
		if recorder.Code != tc.wantStatus || (tc.wantCode != "" && !strings.Contains(recorder.Body.String(), tc.wantCode)) {
			t.Fatalf("%s: status=%d body=%s", tc.name, recorder.Code, recorder.Body.String())
		}
		if len(store.stored) != tc.wantStoreed {
			t.Fatalf("%s: stored %d texts, want %d", tc.name, len(store.stored), tc.wantStoreed)
		}
	}
}
