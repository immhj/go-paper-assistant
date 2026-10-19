package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
	"paperassistant/internal/platform/qq"
)

func strPtr(value string) *string { return &value }

func TestSearchSourceFollowsLiteralUserMention(t *testing.T) {
	ieeeSpec := domain.SearchSpec{Source: domain.PaperSourceIEEE}
	cases := []struct {
		name, mode, text string
		previous         domain.SearchSpec
		want             string
	}{
		{"new direction mentioning IEEE", "restart", "找 IEEE 期刊上的联邦学习论文", domain.SearchSpec{}, domain.PaperSourceIEEE},
		{"new direction without mention defaults to arXiv", "restart", "联邦学习", ieeeSpec, domain.PaperSourceArxiv},
		{"continue keeps IEEE", "continue", "再找5篇", ieeeSpec, domain.PaperSourceIEEE},
		{"refine keeps IEEE", "refine", "只要2023年以后", ieeeSpec, domain.PaperSourceIEEE},
		{"explicit arXiv switches back", "refine", "改成查 arXiv", ieeeSpec, domain.PaperSourceArxiv},
		{"both mentioned keeps previous", "continue", "IEEE 和 arXiv", ieeeSpec, domain.PaperSourceIEEE},
		{"negated IEEE does not select IEEE", "restart", "不要 IEEE 的论文，找联邦学习", domain.SearchSpec{}, domain.PaperSourceArxiv},
		{"negated IEEE leaves an IEEE direction", "continue", "再找5篇，不要IEEE", ieeeSpec, domain.PaperSourceArxiv},
	}
	for _, tc := range cases {
		if got := searchSource(tc.mode, tc.text, tc.previous); got != tc.want {
			t.Errorf("%s: searchSource = %q, want %q", tc.name, got, tc.want)
		}
	}
	if specSourceValue(domain.PaperSourceArxiv) != "" || specSourceValue(domain.PaperSourceIEEE) != "ieee" {
		t.Fatal("arXiv must be stored as the legacy empty source")
	}
}

func TestQualityListsEveryMatchedVenueRank(t *testing.T) {
	cas, factor := 1, 9.7
	candidate := domain.Candidate{CCFVenue: "TNNLS", Paper: domain.Paper{
		CCFRank: strPtr("B"), JCRQuartile: strPtr("Q1"), CASQuartile: &cas, ImpactFactor: &factor,
		VenueRaw: strPtr("IEEE Transactions on Neural Networks and Learning Systems"),
	}}
	tier, label, note := quality(candidate)
	if tier != "venue" {
		t.Fatalf("tier = %q", tier)
	}
	want := "CCF B（TNNLS） · JCR Q1 · 中科院 1区 · IF 9.7 · IEEE Transactions on Neural Networks and Learning Systems"
	if label != want {
		t.Fatalf("label = %q, want %q", label, want)
	}
	if note != "" {
		t.Fatalf("venue tier carries no caveat: %q", note)
	}

	// A JCR-only journal still lands in the venue tier.
	tier, label, note = quality(domain.Candidate{Paper: domain.Paper{JCRQuartile: strPtr("Q2"), VenueRaw: strPtr("IEEE Access")}})
	if tier != "venue" || label != "JCR Q2 · IEEE Access" || note != "" {
		t.Fatalf("JCR-only quality = %q %q %q", tier, label, note)
	}

	// No rank falls back to citation signals exactly as before.
	old := time.Now().AddDate(-2, 0, 0)
	tier, _, note = quality(domain.Candidate{CitationKnown: true, Paper: domain.Paper{PublicationDate: &old, CitationCount: 3, VenueRaw: strPtr("arXiv")}})
	if tier != "citation_velocity" || note != "" {
		t.Fatalf("unranked arXiv paper = %q %q", tier, note)
	}

	// An unmatched IEEE venue says so explicitly instead of staying silent.
	version := "ccfrank4dblp@86e59cc;impact_factor@1.1.4"
	tier, _, note = quality(domain.Candidate{CitationKnown: true, Paper: domain.Paper{Source: domain.PaperSourceIEEE, PublicationDate: &old, RankVersion: &version}})
	if tier != "citation_velocity" || !strings.Contains(note, "未匹配") {
		t.Fatalf("unmatched IEEE paper = %q %q", tier, note)
	}
}

func TestSearchSourceIgnoresModelWrittenQuery(t *testing.T) {
	// The planner's tool query mentions IEEE, but the user did not; the source must stay arXiv.
	source := &recordingSearchSource{}
	app := &Application{store: &searchTestStore{}, papers: source, model: unavailableModel{}}
	conversation := domain.Conversation{ID: 1, ConversationNo: 1, Title: "x"}
	recorder := &recordingDeliverer{}
	err := app.search(context.Background(), domain.User{}, conversation, qq.IncomingMessage{Content: "找扩散模型做分割的论文"}, "diffusion segmentation IEEE Transactions", searchToolPlan{Count: 3, Mode: "restart"}, recorder)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if source.request.Spec.EffectiveSource() != domain.PaperSourceArxiv {
		t.Fatalf("model-written query selected source %q", source.request.Spec.EffectiveSource())
	}
}

func TestSpentIEEEQuotaIsANoticeNotAFailure(t *testing.T) {
	source := &recordingSearchSource{err: domain.ErrSourceQuotaExhausted}
	app := &Application{store: &searchTestStore{}, papers: source, model: unavailableModel{}}
	recorder := &recordingDeliverer{}
	err := app.search(context.Background(), domain.User{}, domain.Conversation{ID: 1, Title: "x"}, qq.IncomingMessage{Content: "查 IEEE 的联邦学习论文"}, "联邦学习", searchToolPlan{Count: 3, Mode: "restart"}, recorder)
	if err != nil {
		t.Fatalf("quota exhaustion must not fail the task: %v", err)
	}
	if source.request.Spec.EffectiveSource() != domain.PaperSourceIEEE {
		t.Fatal("user mention of IEEE should select IEEE")
	}
	last := recorder.events[len(recorder.events)-1].Text
	if !strings.Contains(last, "已达上限") {
		t.Fatalf("missing quota notice: %q", last)
	}
	if strings.Contains(strings.ToLower(last), "openalex") {
		t.Fatalf("quota notice must not expose the upstream provider: %q", last)
	}
}

type recordingSearchSource struct {
	PaperSource
	request domain.PaperSearchRequest
	err     error
}

func (s *recordingSearchSource) Search(_ context.Context, request domain.PaperSearchRequest) ([]domain.Candidate, error) {
	s.request = request
	return nil, s.err
}

type searchTestStore struct{ dao.Store }

func (searchTestStore) ListRecentUserMessages(context.Context, uint64, int) ([]domain.Message, error) {
	return nil, nil
}

type unavailableModel struct{ ModelClient }

func (unavailableModel) Available() bool { return false }

func TestUploadMatchesPaperByDOIOrTitle(t *testing.T) {
	paper := domain.Paper{Source: domain.PaperSourceIEEE, Title: "A Graph-Neural-Network-Powered Solver Framework for Graph Optimization Problems", DOI: strPtr("10.1109/tnnls.2024.3397926")}
	body := strings.Repeat("filler text ", 50)
	if !uploadMatchesPaper(paper, "IEEE TRANSACTIONS ...\nDigital Object Identifier 10.1109/TNNLS.2024.3397926\n"+body) {
		t.Fatal("DOI on the first page should match")
	}
	if !uploadMatchesPaper(paper, "A Graph-Neural-Network-\nPowered Solver Framework for\nGraph Optimization Problems\nAbstract "+body) {
		t.Fatal("title broken across lines should match")
	}
	if uploadMatchesPaper(paper, "Deep Residual Learning for Image Recognition\nAbstract "+body) {
		t.Fatal("a different paper must not match")
	}
	// The DOI appearing only far into the reference list is not evidence of identity.
	if uploadMatchesPaper(paper, strings.Repeat("unrelated words ", 3000)+"10.1109/tnnls.2024.3397926") {
		t.Fatal("a DOI cited in the references must not match")
	}
	// A short letter that cites the selected paper: its references start after ~12k runes.
	citing := "Deep Residual Learning for Image Recognition\nDOI 10.1109/lsp.2024.1\n" + strings.Repeat("letter body ", 1000) +
		"\nReferences\n[1] A Graph-Neural-Network-Powered Solver Framework for Graph Optimization Problems, doi:10.1109/tnnls.2024.3397926"
	if uploadMatchesPaper(paper, citing) {
		t.Fatal("a short paper citing the selected one must not match")
	}
	// A DOI is matched only on its full length.
	if uploadMatchesPaper(paper, "Digital Object Identifier 10.1109/TNNLS.2024.33979261\n"+body) {
		t.Fatal("a longer DOI with the same prefix must not match")
	}
	short := domain.Paper{Source: domain.PaperSourceIEEE, Title: "Graphs"}
	if uploadMatchesPaper(short, "Graphs\n"+body) {
		t.Fatal("very short titles need an identifier match")
	}
}

func TestSlidingWindowLimiter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newSlidingWindowLimiter(2, time.Hour, func() time.Time { return now })
	if !limiter.Allow(1) || !limiter.Allow(1) || limiter.Allow(1) {
		t.Fatal("third upload inside the window must be refused")
	}
	if !limiter.Allow(2) {
		t.Fatal("limits are per account")
	}
	now = now.Add(time.Hour)
	if !limiter.Allow(1) {
		t.Fatal("window must slide")
	}
}

// fullTextTestStore serves one paper and optional private text.
type fullTextTestStore struct {
	dao.Store
	paper         domain.Paper
	private       map[uint64]string // accountID -> text for paper
	sharedUpdates int
	upserts       []string
	messages      []domain.Message
}

func (s *fullTextTestStore) GetPaper(_ context.Context, id uint64) (domain.Paper, error) {
	if id != s.paper.ID {
		return domain.Paper{}, dao.ErrNotFound
	}
	return s.paper, nil
}
func (s *fullTextTestStore) GetAccountFullText(_ context.Context, accountID, paperID uint64) (string, error) {
	if text, ok := s.private[accountID]; ok && paperID == s.paper.ID {
		return text, nil
	}
	return "", dao.ErrNotFound
}
func (s *fullTextTestStore) UpdatePaperFullText(context.Context, uint64, string, string) error {
	s.sharedUpdates++
	return nil
}
func (s *fullTextTestStore) UpsertAccountFullText(_ context.Context, accountID, paperID uint64, content string) error {
	s.upserts = append(s.upserts, content)
	return nil
}
func (s *fullTextTestStore) GetAccountFullTextStatus(_ context.Context, _, paperID uint64) (domain.PrivateFullTextStatus, error) {
	return domain.PrivateFullTextStatus{PaperID: paperID, Present: len(s.upserts) > 0}, nil
}

type fullTextTestSource struct {
	PaperSource
	pdfURL  string
	lookups int
}

func (s *fullTextTestSource) GetByID(context.Context, string, string) (domain.Candidate, error) {
	s.lookups++
	return domain.Candidate{OpenAccessPDFURL: s.pdfURL}, nil
}

type fullTextTestExtractor struct {
	fetchErr error
	fetches  int
	upload   string
}

func (e *fullTextTestExtractor) Fetch(context.Context, uint64, string) (string, string, error) {
	e.fetches++
	return "", "", e.fetchErr
}
func (e *fullTextTestExtractor) ExtractUploaded(_ context.Context, body io.Reader) (string, error) {
	_, _ = io.Copy(io.Discard, body)
	return e.upload, nil
}

func ieeePaper() domain.Paper {
	return domain.Paper{ID: 5, Source: domain.PaperSourceIEEE, ExternalID: "10.1109/access.2024.1", Title: "Graph Networks for Intrusion Detection Systems", DOI: strPtr("10.1109/access.2024.1"), FullTextStatus: "not_fetched"}
}

func accountScope(id uint64) context.Context {
	return withProjectAgentScope(context.Background(), domain.ProjectOwner{Type: domain.ProjectOwnerAccount, ID: id}, 1)
}

func TestFullTextPrefersOwnPrivateTextAndNeverSharesIt(t *testing.T) {
	store := &fullTextTestStore{paper: ieeePaper(), private: map[uint64]string{7: "private body"}}
	source := &fullTextTestSource{}
	app := &Application{store: store, papers: source, fulltext: &fullTextTestExtractor{}}

	resolved, err := app.fullText(accountScope(7), store.paper)
	if err != nil || !resolved.private || resolved.text != "private body" || resolved.path != "" {
		t.Fatalf("owner did not get private text: %+v %v", resolved, err)
	}
	if err = app.markSharedFullText(accountScope(7), store.paper, resolved); err != nil || store.sharedUpdates != 0 {
		t.Fatalf("private text leaked into the shared cache: updates=%d err=%v", store.sharedUpdates, err)
	}
	if source.lookups != 0 {
		t.Fatal("private text should short-circuit the source lookup")
	}

	// Another account, a guest and a QQ user cannot see account 7's text.
	guest := withProjectAgentScope(context.Background(), domain.ProjectOwner{Type: domain.ProjectOwnerGuest, ID: 7}, 1)
	for name, ctx := range map[string]context.Context{"other account": accountScope(8), "guest": guest, "qq": context.Background()} {
		_, err = app.fullText(ctx, store.paper)
		var unavailable *fullTextUnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("%s: expected unavailable full text, got %v", name, err)
		}
	}
}

func TestFailedIEEEOpenAccessDownloadAsksForUpload(t *testing.T) {
	store := &fullTextTestStore{paper: ieeePaper()}
	extractor := &fullTextTestExtractor{fetchErr: errors.New("download PDF status 418")}
	app := &Application{store: store, papers: &fullTextTestSource{pdfURL: "https://ieeexplore.ieee.org/stamp.pdf"}, fulltext: extractor}
	_, err := app.fullText(accountScope(7), store.paper)
	var unavailable *fullTextUnavailableError
	if !errors.As(err, &unavailable) || extractor.fetches != 1 {
		t.Fatalf("publisher refusal should fall back to upload, got %v", err)
	}
}

func TestIEEELookupFailureAsksForUpload(t *testing.T) {
	store := &fullTextTestStore{paper: ieeePaper()}
	app := &Application{store: store, papers: &failingLookupSource{}, fulltext: &fullTextTestExtractor{}}
	_, err := app.fullText(accountScope(7), store.paper)
	var unavailable *fullTextUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("lookup failure should become an upload prompt, got %v", err)
	}
}

type failingLookupSource struct{ PaperSource }

func (failingLookupSource) GetByID(context.Context, string, string) (domain.Candidate, error) {
	return domain.Candidate{}, domain.ErrSourceQuotaExhausted
}

func TestReadWithoutFullTextPromptsUploadInsteadOfFailing(t *testing.T) {
	store := &fullTextTestStore{paper: ieeePaper()}
	app := &Application{store: store, papers: &fullTextTestSource{}, fulltext: &fullTextTestExtractor{}}
	current, _ := json.Marshal(domain.SearchContext{Name: "x", SpecVersion: 1, NextOrdinal: 2, Ordinals: map[string]int{"5": 1}})
	conversation := domain.Conversation{ID: 3, ConversationNo: 1, CurrentContext: current}

	for name, ctx := range map[string]context.Context{"account": accountScope(7), "qq": context.Background()} {
		recorder := &recordingDeliverer{}
		if err := app.readPaper(ctx, conversation, qq.IncomingMessage{}, "1", domain.ProjectReadTypeDeep, recorder); err != nil {
			t.Fatalf("%s: missing full text must not fail the task: %v", name, err)
		}
		last := recorder.events[len(recorder.events)-1].Text
		if !strings.Contains(last, "https://doi.org/10.1109/access.2024.1") {
			t.Fatalf("%s: prompt lacks the IEEE link: %q", name, last)
		}
		wantHint := "上传全文 PDF"
		if name == "qq" {
			wantHint = "登录 Web 账号"
		}
		if !strings.Contains(last, wantHint) {
			t.Fatalf("%s: prompt lacks %q: %q", name, wantHint, last)
		}
	}
}

func TestUploadPrivateFullTextRequiresAccountAndMatchingPDF(t *testing.T) {
	store := &fullTextTestStore{paper: ieeePaper()}
	extractor := &fullTextTestExtractor{upload: "Graph Networks for Intrusion Detection Systems\nAbstract " + strings.Repeat("body ", 200)}
	app := New(store, &fullTextTestSource{}, nil, extractor, nil)

	if _, err := app.UploadPrivateFullText(context.Background(), WebActor{Type: WebActorGuest, ID: 1}, 5, strings.NewReader("%PDF-")); !errors.Is(err, ErrAccountRequired) {
		t.Fatalf("guest upload: %v", err)
	}
	status, err := app.UploadPrivateFullText(context.Background(), WebActor{Type: WebActorAccount, ID: 7}, 5, strings.NewReader("%PDF-"))
	if err != nil || !status.Present || len(store.upserts) != 1 {
		t.Fatalf("matching upload: status=%+v err=%v upserts=%d", status, err, len(store.upserts))
	}
	extractor.upload = "Deep Residual Learning for Image Recognition\nAbstract " + strings.Repeat("body ", 200)
	if _, err = app.UploadPrivateFullText(context.Background(), WebActor{Type: WebActorAccount, ID: 7}, 5, strings.NewReader("%PDF-")); !errors.Is(err, ErrFullTextMismatch) {
		t.Fatalf("mismatched upload: %v", err)
	}
	if len(store.upserts) != 1 {
		t.Fatal("a mismatched upload must not be stored")
	}
	if _, err = app.UploadPrivateFullText(context.Background(), WebActor{Type: WebActorAccount, ID: 7}, 99, strings.NewReader("%PDF-")); !errors.Is(err, dao.ErrPaperNotFound) {
		t.Fatalf("unknown paper: %v", err)
	}
}
