package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

const paperColumns = `id, source, external_id, title, abstract, publication_date, venue_raw, ccf_rank, citation_count, tldr, fulltext_path, fulltext_status, doi, issn, jcr_quartile, cas_quartile, impact_factor, rank_version`

// UpsertPaper keeps earlier enrichment when a later record lacks it. Venue ranks are the
// exception: a record carrying a rank data version is a fresh deterministic match, so it
// replaces every rank column together, including clearing ranks that no longer match.
func (s *Store) UpsertPaper(ctx context.Context, paper domain.Paper) (domain.Paper, error) {
	if paper.Source == "" || paper.ExternalID == "" {
		return domain.Paper{}, errors.New("paper source and external ID are required")
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO papers (source, external_id, title, abstract, publication_date, venue_raw, ccf_rank, citation_count, tldr, fulltext_path, fulltext_status,
			doi, issn, jcr_quartile, cas_quartile, impact_factor, rank_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			id=LAST_INSERT_ID(id), title=VALUES(title), abstract=COALESCE(VALUES(abstract),abstract),
			publication_date=COALESCE(VALUES(publication_date),publication_date),
			venue_raw=COALESCE(VALUES(venue_raw),venue_raw),
			citation_count=IF(VALUES(citation_count)>0,VALUES(citation_count),citation_count),
			tldr=COALESCE(VALUES(tldr),tldr),
			doi=COALESCE(VALUES(doi),doi), issn=COALESCE(VALUES(issn),issn),
			ccf_rank=IF(VALUES(rank_version) IS NULL, COALESCE(VALUES(ccf_rank),ccf_rank), VALUES(ccf_rank)),
			jcr_quartile=IF(VALUES(rank_version) IS NULL, jcr_quartile, VALUES(jcr_quartile)),
			cas_quartile=IF(VALUES(rank_version) IS NULL, cas_quartile, VALUES(cas_quartile)),
			impact_factor=IF(VALUES(rank_version) IS NULL, impact_factor, VALUES(impact_factor)),
			rank_version=COALESCE(VALUES(rank_version),rank_version)`,
		paper.Source, paper.ExternalID, paper.Title, nullableString(paper.Abstract), nullableTime(paper.PublicationDate),
		nullableString(paper.VenueRaw), nullableString(paper.CCFRank), paper.CitationCount, nullableString(paper.TLDR),
		nullableString(paper.FullTextPath), paper.FullTextStatus,
		nullableString(paper.DOI), nullableString(paper.ISSN), nullableString(paper.JCRQuartile), nullableInt(paper.CASQuartile),
		nullableFloat(paper.ImpactFactor), nullableString(paper.RankVersion))
	if err != nil {
		return domain.Paper{}, fmt.Errorf("upsert paper: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.Paper{}, fmt.Errorf("paper id: %w", err)
	}
	return s.GetPaper(ctx, uint64(id))
}

func (s *Store) GetPaper(ctx context.Context, id uint64) (domain.Paper, error) {
	return scanPaper(s.db.QueryRowContext(ctx, `SELECT `+paperColumns+` FROM papers WHERE id=?`, id))
}

func (s *Store) UpdatePaperFullText(ctx context.Context, id uint64, path, status string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE papers SET fulltext_path=?, fulltext_status=? WHERE id=?`, path, status, id)
	if err != nil {
		return fmt.Errorf("update paper full text: %w", err)
	}
	return requireAffected(result)
}

func scanPaper(row interface{ Scan(...any) error }) (domain.Paper, error) {
	var item domain.Paper
	var abstract, venue, rank, tldr, path, doi, issn, jcr, version sql.NullString
	var cas sql.NullInt64
	var factor sql.NullFloat64
	var date sql.NullTime
	if err := row.Scan(&item.ID, &item.Source, &item.ExternalID, &item.Title, &abstract, &date, &venue, &rank, &item.CitationCount, &tldr, &path, &item.FullTextStatus,
		&doi, &issn, &jcr, &cas, &factor, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Paper{}, dao.ErrNotFound
		}
		return domain.Paper{}, fmt.Errorf("scan paper: %w", err)
	}
	item.Abstract = stringPointer(abstract)
	if date.Valid {
		value := date.Time
		item.PublicationDate = &value
	}
	item.VenueRaw = stringPointer(venue)
	item.CCFRank = stringPointer(rank)
	item.TLDR = stringPointer(tldr)
	item.FullTextPath = stringPointer(path)
	item.DOI = stringPointer(doi)
	item.ISSN = stringPointer(issn)
	item.JCRQuartile = stringPointer(jcr)
	item.RankVersion = stringPointer(version)
	if cas.Valid {
		value := int(cas.Int64)
		item.CASQuartile = &value
	}
	if factor.Valid {
		value := factor.Float64
		item.ImpactFactor = &value
	}
	return item, nil
}

// isForeignKeyViolation reports MySQL error 1452 (referenced parent row missing).
func isForeignKeyViolation(err error) bool {
	var mysqlErr *mysqldriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1452
}

func stringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// UpsertAccountFullText stores or replaces one account's private full text for a paper
// (ADR 0019). The text never enters the shared papers.fulltext_path cache.
func (s *Store) UpsertAccountFullText(ctx context.Context, accountID, paperID uint64, content string) error {
	if accountID == 0 || paperID == 0 || content == "" {
		return errors.New("account, paper and content are required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO account_paper_fulltexts (account_id, paper_id, content, content_chars)
		VALUES (?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE content=VALUES(content), content_chars=VALUES(content_chars), updated_at=CURRENT_TIMESTAMP(3)`,
		accountID, paperID, content, len([]rune(content)))
	if err != nil {
		if isForeignKeyViolation(err) {
			return dao.ErrPaperNotFound
		}
		return fmt.Errorf("upsert account full text: %w", err)
	}
	return nil
}

func (s *Store) GetAccountFullText(ctx context.Context, accountID, paperID uint64) (string, error) {
	var content string
	err := s.db.QueryRowContext(ctx, `SELECT content FROM account_paper_fulltexts WHERE account_id=? AND paper_id=?`, accountID, paperID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return "", dao.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("get account full text: %w", err)
	}
	return content, nil
}

func (s *Store) GetAccountFullTextStatus(ctx context.Context, accountID, paperID uint64) (domain.PrivateFullTextStatus, error) {
	var status domain.PrivateFullTextStatus
	var updated time.Time
	err := s.db.QueryRowContext(ctx, `SELECT content_chars, updated_at FROM account_paper_fulltexts WHERE account_id=? AND paper_id=?`, accountID, paperID).Scan(&status.Chars, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.PrivateFullTextStatus{PaperID: paperID}, nil
	}
	if err != nil {
		return domain.PrivateFullTextStatus{}, fmt.Errorf("get account full text status: %w", err)
	}
	status.PaperID, status.Present, status.UpdatedAt = paperID, true, &updated
	return status, nil
}

func (s *Store) DeleteAccountFullText(ctx context.Context, accountID, paperID uint64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM account_paper_fulltexts WHERE account_id=? AND paper_id=?`, accountID, paperID)
	if err != nil {
		return fmt.Errorf("delete account full text: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return dao.ErrNotFound
	}
	return nil
}
