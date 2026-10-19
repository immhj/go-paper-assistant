package service

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

// privateFullTextUploadsPerHour is the per-account safety limit for full-text uploads.
const privateFullTextUploadsPerHour = 10

// The paper's own DOI is printed on its first page (IEEE footer) and its title at the very top.
// Both windows end well before the reference list of even a short letter, so a different paper
// that merely cites the selected one is not accepted.
const (
	uploadDOIWindowRunes   = 10_000
	uploadTitleWindowRunes = 3_000
)

var (
	// ErrAccountRequired means only an authenticated account may hold private full text.
	ErrAccountRequired = errors.New("private full text requires an account")
	// ErrFullTextMismatch means the uploaded PDF cannot be tied to the selected paper.
	ErrFullTextMismatch = errors.New("uploaded full text does not match the paper")
	// ErrUploadRateLimited means the account exceeded the hourly upload limit.
	ErrUploadRateLimited = errors.New("private full text upload rate limited")
)

// UploadPrivateFullText validates an account's own PDF for one paper and stores the extracted
// text as that account's private full text (ADR 0019). It does not start a read.
func (a *Application) UploadPrivateFullText(ctx context.Context, actor WebActor, paperID uint64, body io.Reader) (domain.PrivateFullTextStatus, error) {
	accountID, err := privateFullTextAccount(actor, paperID)
	if err != nil {
		return domain.PrivateFullTextStatus{}, err
	}
	if !a.uploadLimiter.Allow(accountID) {
		return domain.PrivateFullTextStatus{}, ErrUploadRateLimited
	}
	paper, err := a.store.GetPaper(ctx, paperID)
	if errors.Is(err, dao.ErrNotFound) {
		return domain.PrivateFullTextStatus{}, dao.ErrPaperNotFound
	}
	if err != nil {
		return domain.PrivateFullTextStatus{}, err
	}
	text, err := a.fulltext.ExtractUploaded(ctx, body)
	if err != nil {
		return domain.PrivateFullTextStatus{}, err
	}
	if !uploadMatchesPaper(paper, text) {
		return domain.PrivateFullTextStatus{}, ErrFullTextMismatch
	}
	if err = a.store.UpsertAccountFullText(ctx, accountID, paperID, text); err != nil {
		return domain.PrivateFullTextStatus{}, err
	}
	return a.store.GetAccountFullTextStatus(ctx, accountID, paperID)
}

func (a *Application) PrivateFullTextStatus(ctx context.Context, actor WebActor, paperID uint64) (domain.PrivateFullTextStatus, error) {
	accountID, err := privateFullTextAccount(actor, paperID)
	if err != nil {
		return domain.PrivateFullTextStatus{}, err
	}
	return a.store.GetAccountFullTextStatus(ctx, accountID, paperID)
}

func (a *Application) DeletePrivateFullText(ctx context.Context, actor WebActor, paperID uint64) error {
	accountID, err := privateFullTextAccount(actor, paperID)
	if err != nil {
		return err
	}
	return a.store.DeleteAccountFullText(ctx, accountID, paperID)
}

func privateFullTextAccount(actor WebActor, paperID uint64) (uint64, error) {
	actor, err := normalizeWebActor(actor)
	if err != nil || actor.Type != WebActorAccount {
		return 0, ErrAccountRequired
	}
	if paperID == 0 {
		return 0, ErrValidation
	}
	return actor.ID, nil
}

// uploadMatchesPaper ties an upload to the selected paper deterministically: its DOI (or arXiv
// ID) or its normalized title must appear near the start of the extracted text.
func uploadMatchesPaper(paper domain.Paper, text string) bool {
	runes := []rune(text)
	// Whitespace stays in place: it is the natural right boundary of a printed DOI.
	lowerHead := strings.ToLower(string(runes[:min(len(runes), uploadDOIWindowRunes)]))
	identifiers := make([]string, 0, 2)
	if paper.DOI != nil && *paper.DOI != "" {
		identifiers = append(identifiers, strings.ToLower(*paper.DOI))
	}
	if paper.Source == domain.PaperSourceArxiv && paper.ExternalID != "" {
		identifiers = append(identifiers, strings.ToLower(paper.ExternalID))
	}
	for _, identifier := range identifiers {
		if containsBounded(lowerHead, identifier) {
			return true
		}
	}
	title := compactLettersDigits(paper.Title)
	// Very short titles would match by accident; they must be confirmed by an identifier.
	titleHead := compactLettersDigits(string(runes[:min(len(runes), uploadTitleWindowRunes)]))
	return len([]rune(title)) >= 20 && strings.Contains(titleHead, title)
}

// containsBounded finds needle not followed by another identifier character, so the DOI
// 10.1109/x.123 does not match inside 10.1109/x.1234.
func containsBounded(haystack, needle string) bool {
	if needle == "" {
		return false
	}
	for offset := 0; ; {
		index := strings.Index(haystack[offset:], needle)
		if index < 0 {
			return false
		}
		end := offset + index + len(needle)
		if end == len(haystack) || !isIdentifierByte(haystack[end]) {
			return true
		}
		offset += index + 1
	}
}

func isIdentifierByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

// compactLettersDigits keeps only lower-cased letters and digits, so PDF line breaks, real
// hyphens and soft line-end hyphenation all compare equal to the metadata title.
func compactLettersDigits(value string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

// slidingWindowLimiter is an in-process per-key limiter; the service runs as a single
// instance (ADR 0006), so no shared store is needed for this safety limit.
type slidingWindowLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	now    func() time.Time
	hits   map[uint64][]time.Time
}

func newSlidingWindowLimiter(limit int, window time.Duration, now func() time.Time) *slidingWindowLimiter {
	return &slidingWindowLimiter{limit: limit, window: window, now: now, hits: map[uint64][]time.Time{}}
}

func (l *slidingWindowLimiter) Allow(key uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	kept := l.hits[key][:0]
	for _, at := range l.hits[key] {
		if now.Sub(at) < l.window {
			kept = append(kept, at)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
