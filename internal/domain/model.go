package domain

import (
	"encoding/json"
	"errors"
	"time"
)

type User struct {
	ID                   uint64    `json:"id"`
	QQOpenID             string    `json:"qqOpenID"`
	ActiveConversationID *uint64   `json:"activeConversationId,omitempty"`
	LastActiveAt         time.Time `json:"lastActiveAt"`
	CreatedAt            time.Time `json:"createdAt"`
}

type Conversation struct {
	ID             uint64          `json:"id"`
	UserID         *uint64         `json:"userId,omitempty"`
	ProjectID      *uint64         `json:"projectId,omitempty"`
	ConversationNo uint32          `json:"conversationNo"`
	Title          string          `json:"title"`
	CurrentContext json.RawMessage `json:"currentContext,omitempty"`
	Status         string          `json:"status"`
	Selected       bool            `json:"selected"`
	CreatedAt      time.Time       `json:"createdAt"`
	UpdatedAt      time.Time       `json:"updatedAt"`
}

func (c Conversation) ValidateOwner() error {
	if (c.UserID == nil) == (c.ProjectID == nil) {
		return errors.New("conversation must belong to exactly one QQ user or research project")
	}
	return nil
}

type SearchContext struct {
	Name        string         `json:"name"`
	Spec        SearchSpec     `json:"spec"`
	SpecVersion int            `json:"specVersion"`
	NextOrdinal int            `json:"nextOrdinal"`
	Ordinals    map[string]int `json:"ordinals"`
}

type PaperIdentity struct {
	Source     string `json:"source"`
	ExternalID string `json:"externalId"`
}

type PaperSearchRequest struct {
	Spec    SearchSpec      `json:"spec"`
	Limit   int             `json:"limit"`
	Exclude []PaperIdentity `json:"exclude,omitempty"`
}

type SearchSpec struct {
	Concepts []SearchConcept `json:"concepts"`
	YearFrom *int            `json:"yearFrom,omitempty"`
	YearTo   *int            `json:"yearTo,omitempty"`
	Exclude  []string        `json:"exclude,omitempty"`
	// Source is the paper catalogue a search queries. Empty means arXiv so specs saved before
	// IEEE support keep their meaning (ADR 0018).
	Source string `json:"source,omitempty"`
}

const (
	PaperSourceArxiv = "arxiv"
	PaperSourceIEEE  = "ieee"
)

// ErrSourceQuotaExhausted means a paper catalogue's daily request budget is spent (OpenAlex
// for IEEE). It is a user-facing condition, not a system failure.
var ErrSourceQuotaExhausted = errors.New("paper source daily quota exhausted")

// EffectiveSource resolves the stored source, defaulting legacy specs to arXiv.
func (s SearchSpec) EffectiveSource() string {
	if s.Source == PaperSourceIEEE {
		return PaperSourceIEEE
	}
	return PaperSourceArxiv
}

type SearchConcept struct {
	Concept  string   `json:"concept"`
	Synonyms []string `json:"synonyms"`
}

type Paper struct {
	ID              uint64     `json:"id"`
	Source          string     `json:"source"`
	ExternalID      string     `json:"externalId"`
	Title           string     `json:"title"`
	Abstract        *string    `json:"abstract,omitempty"`
	PublicationDate *time.Time `json:"publicationDate,omitempty"`
	VenueRaw        *string    `json:"venue,omitempty"`
	CCFRank         *string    `json:"ccfRank,omitempty"`
	CitationCount   uint64     `json:"citationCount"`
	TLDR            *string    `json:"tldr,omitempty"`
	FullTextPath    *string    `json:"fulltextPath,omitempty"`
	FullTextStatus  string     `json:"fulltextStatus"`
	// DOI and ISSN are recorded for sources that carry them (IEEE). Venue ranks come from
	// deterministic matching against pinned data; RankVersion names that data (ADR 0020).
	DOI          *string  `json:"doi,omitempty"`
	ISSN         *string  `json:"issn,omitempty"`
	JCRQuartile  *string  `json:"jcrQuartile,omitempty"`
	CASQuartile  *int     `json:"casQuartile,omitempty"`
	ImpactFactor *float64 `json:"impactFactor,omitempty"`
	RankVersion  *string  `json:"rankVersion,omitempty"`
}

// LandingURL is the publisher landing page used for subscribed full text the user downloads.
func (p Paper) LandingURL() string {
	if p.DOI == nil || *p.DOI == "" {
		return ""
	}
	return "https://doi.org/" + *p.DOI
}

// HasVenueRank reports whether any venue rank system matched this paper.
func (p Paper) HasVenueRank() bool {
	return p.CCFRank != nil || p.JCRQuartile != nil || p.CASQuartile != nil
}

// PrivateFullTextStatus describes whether an account has a private full text for a paper.
// It never carries the text itself.
type PrivateFullTextStatus struct {
	PaperID   uint64     `json:"paperId"`
	Present   bool       `json:"present"`
	Chars     int        `json:"chars,omitempty"`
	UpdatedAt *time.Time `json:"updatedAt,omitempty"`
}

type Candidate struct {
	Paper
	DOI              string `json:"doi,omitempty"`
	ArxivID          string `json:"arxivId,omitempty"`
	OpenAccessPDFURL string `json:"openAccessPdfUrl,omitempty"`
	CitationKnown    bool   `json:"citationKnown"`
	// CCFVenue is the CCF list abbreviation (or full name) behind CCFRank.
	CCFVenue string `json:"ccfVenue,omitempty"`
}

type RelevanceJudgment struct {
	Index     int    `json:"index"`
	Level     string `json:"level"`
	Reason    string `json:"reason"`
	SummaryZH string `json:"summaryZh"`
}

type ModelUsage struct {
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	CachedTokens     int     `json:"cachedTokens"`
	CostUSD          float64 `json:"costUsd"`
}

func (u *ModelUsage) Add(value ModelUsage) {
	u.PromptTokens += value.PromptTokens
	u.CompletionTokens += value.CompletionTokens
	u.CachedTokens += value.CachedTokens
	u.CostUSD += value.CostUSD
}

type TaskStage struct {
	Code       string `json:"code"`
	DurationMS int64  `json:"durationMs"`
}

type AgentTrace struct {
	Version      int    `json:"version"`
	Route        string `json:"route"`
	Domain       string `json:"domain,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	RouterSource string `json:"routerSource"`
	// RouterUsage is the single planner call since trace version 3 (ADR 0021).
	RouterUsage ModelUsage `json:"routerUsage"`
	// Tool is the first step's tool, kept so version-1/2 readers still group by tool.
	Tool string `json:"tool,omitempty"`
	// ExecutorUsage exists only in version-1/2 traces, before the planner merged both agents.
	ExecutorUsage ModelUsage      `json:"executorUsage,omitzero"`
	Plan          []PlanStepTrace `json:"plan,omitempty"`
	ToolUsage     ModelUsage      `json:"toolUsage"`
	Status        string          `json:"status"`
	ErrorCode     string          `json:"errorCode,omitempty"`
	Error         string          `json:"error,omitempty"`
	DurationMS    int64           `json:"durationMs,omitempty"`
	Stages        []TaskStage     `json:"stages,omitempty"`
}

// PlanSelector lets a later step refer to papers the first (search) step actually displayed.
// Kind "top_relevance" takes the first Count papers of that list, which is relevance-ordered;
// kind "position" takes the listed 1-based positions. It is resolved in Go, never by the model.
type PlanSelector struct {
	Kind      string `json:"kind"`
	Count     int    `json:"count,omitempty"`
	Positions []int  `json:"positions,omitempty"`
}

// PlanStepTrace records one step of a turn's plan. Status is succeeded, stopped (the step ended
// in an expected state that needs the user, so the plan stopped), failed or skipped.
type PlanStepTrace struct {
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args,omitempty"`
	Select    *PlanSelector   `json:"select,omitempty"`
	Status    string          `json:"status"`
	Usage     ModelUsage      `json:"usage"`
	ErrorCode string          `json:"errorCode,omitempty"`
}

type ShownPaper struct {
	PaperID         uint64  `json:"paperId"`
	Ordinal         int     `json:"ordinal"`
	QualityTier     string  `json:"qualityTier"`
	QualityLabel    string  `json:"qualityLabel"`
	Relevance       *string `json:"relevance,omitempty"`
	RelevanceReason string  `json:"relevanceReason,omitempty"`
	SummaryZH       string  `json:"summaryZh,omitempty"`
	// TLDR is the direction-independent "what this paper is about" (ADR 0005 P6), shown only in
	// an expanded candidate card. Records written before this field existed simply lack it.
	TLDR             string `json:"tldr,omitempty"`
	OpenAccessPDFURL string `json:"openAccessPdfUrl,omitempty"`
	ArxivID          string `json:"arxivId,omitempty"`
	Source           string `json:"source,omitempty"`
	DOI              string `json:"doi,omitempty"`
	LandingURL       string `json:"landingUrl,omitempty"`
	Title            string `json:"title,omitempty"`
	AbstractMissing  bool   `json:"abstractMissing,omitempty"`
	// QualityNote explains caveats of third-party rank data shown in QualityLabel.
	QualityNote string `json:"qualityNote,omitempty"`
}

type Message struct {
	ID             uint64          `json:"id"`
	ConversationID uint64          `json:"conversationId"`
	Role           string          `json:"role"`
	QQMessageID    *string         `json:"qqMessageId,omitempty"`
	QQOutboundID   *string         `json:"qqOutboundId,omitempty"`
	MessageSeq     *uint32         `json:"messageSeq,omitempty"`
	Content        *string         `json:"content,omitempty"`
	ShownPapers    json.RawMessage `json:"shownPapers,omitempty"`
	AgentTrace     json.RawMessage `json:"agentTrace,omitempty"`
	DeliveryStatus string          `json:"deliveryStatus"`
	CostUSD        float64         `json:"costUsd"`
	CreatedAt      time.Time       `json:"createdAt"`
}

type Overview struct {
	Users            int64        `json:"users"`
	ActiveUsers      int64        `json:"activeUsers"`
	InboundMessages  int64        `json:"inboundMessages"`
	Searches         int64        `json:"searches"`
	Papers           int64        `json:"papers"`
	CostUSD          float64      `json:"costUsd"`
	RouterCostUSD    float64      `json:"routerCostUsd"`
	ExecutorCostUSD  float64      `json:"executorCostUsd"`
	ToolCostUSD      float64      `json:"toolCostUsd"`
	Turns            int64        `json:"turns"`
	MultiStepTurns   int64        `json:"multiStepTurns"`
	FailedTurns      int64        `json:"failedTurns"`
	DeliveryFailures int64        `json:"deliveryFailures"`
	Routes           []LabelCount `json:"routes"`
	Tools            []LabelCount `json:"tools"`
	Daily            []DailyPoint `json:"daily"`
}

// LabelCount is one bucket of a distribution, such as a router route or an executor tool.
type LabelCount struct {
	Label   string  `json:"label"`
	Count   int64   `json:"count"`
	CostUSD float64 `json:"costUsd"`
}

// DailyPoint carries one day of the overview charts. The three split costs are pointers on
// purpose: a day that predates agent tracing has no split at all, and reporting nil lets the
// chart draw a gap instead of claiming the router cost was zero that day.
type DailyPoint struct {
	Date            string   `json:"date"`
	InboundMessages int64    `json:"inboundMessages"`
	Searches        int64    `json:"searches"`
	CostUSD         float64  `json:"costUsd"`
	RouterCostUSD   *float64 `json:"routerCostUsd"`
	ExecutorCostUSD *float64 `json:"executorCostUsd"`
	ToolCostUSD     *float64 `json:"toolCostUsd"`
	Turns           int64    `json:"turns"`
	FailedTurns     int64    `json:"failedTurns"`
}

type AdminUser struct {
	ID            uint64    `json:"id"`
	OpenIDMasked  string    `json:"openId"`
	Conversations int64     `json:"conversations"`
	Messages      int64     `json:"messages"`
	CostUSD       float64   `json:"costUsd"`
	LastActiveAt  time.Time `json:"lastActiveAt"`
}
