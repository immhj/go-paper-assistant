package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"paperassistant/internal/config"
)

type Store struct{ db *sql.DB }

func Open(ctx context.Context, cfg config.MySQLConfig) (*Store, error) {
	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(30 * time.Minute)
	deadline := time.Now().Add(cfg.ConnectTimeout)
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err == nil {
			store := &Store{db: db}
			if migrationErr := store.ensureMigrations(ctx); migrationErr != nil {
				db.Close()
				return nil, migrationErr
			}
			return store, nil
		}
		if time.Now().After(deadline) || errors.Is(ctx.Err(), context.Canceled) {
			db.Close()
			return nil, fmt.Errorf("wait for mysql: %w", err)
		}
		time.Sleep(time.Second)
	}
}

// ensureMigrations only retains additive, non-destructive historical message upgrades. The
// research-project migration is intentionally manual because it deletes old Web conversations
// and MySQL DDL cannot be transactionally rolled back.
func (s *Store) ensureMigrations(ctx context.Context) error {
	messageColumns := []struct{ name, ddl string }{
		{"agent_trace", `ALTER TABLE messages ADD COLUMN agent_trace JSON NULL COMMENT 'Versioned Router/Executor execution summary'`},
		{"delivery_status", `ALTER TABLE messages ADD COLUMN delivery_status ENUM('received','reserved','sent','failed') NOT NULL DEFAULT 'received' COMMENT 'Message delivery state'`},
		{"qq_outbound_id", `ALTER TABLE messages ADD COLUMN qq_outbound_id VARCHAR(128) NULL COMMENT 'QQ bot outbound message or stream ID'`},
	}
	for _, migration := range messageColumns {
		added, err := s.ensureColumn(ctx, "messages", migration.name, migration.ddl)
		if err != nil {
			return err
		}
		if added && migration.name == "delivery_status" {
			if _, err = s.db.ExecContext(ctx, `UPDATE messages SET delivery_status='sent' WHERE role='bot'`); err != nil {
				return fmt.Errorf("backfill delivery status: %w", err)
			}
		}
	}
	if err := s.ensureIndex(ctx, "messages", "idx_messages_outbound", `CREATE INDEX idx_messages_outbound ON messages (qq_outbound_id)`); err != nil {
		return err
	}
	if err := s.validateResearchProjectSchema(ctx); err != nil {
		return err
	}
	return s.ensureIEEEAndPrivateFullTextSchema(ctx)
}

// ensureIEEEAndPrivateFullTextSchema applies docs/migrations/0010 additively: nullable paper
// columns and a new table, with no data rewrite (ADR 0018, 0019, 0020).
func (s *Store) ensureIEEEAndPrivateFullTextSchema(ctx context.Context) error {
	columns := []struct{ name, ddl string }{
		{"doi", `ALTER TABLE papers ADD COLUMN doi VARCHAR(191) NULL COMMENT '小写 DOI；IEEE 来源时与 external_id 相同'`},
		{"issn", `ALTER TABLE papers ADD COLUMN issn VARCHAR(255) NULL COMMENT '逗号分隔的 ISSN/eISSN'`},
		{"jcr_quartile", `ALTER TABLE papers ADD COLUMN jcr_quartile ENUM('Q1','Q2','Q3','Q4') NULL COMMENT 'impact_factor 数据按 ISSN 精确匹配'`},
		{"cas_quartile", `ALTER TABLE papers ADD COLUMN cas_quartile TINYINT UNSIGNED NULL COMMENT '中科院分区 1-4；第三方整理，无版本与学科'`},
		{"impact_factor", `ALTER TABLE papers ADD COLUMN impact_factor DECIMAL(9,3) NULL`},
		{"rank_version", `ALTER TABLE papers ADD COLUMN rank_version VARCHAR(128) NULL COMMENT '发表场所等级所依据的固定数据版本'`},
	}
	for _, migration := range columns {
		if _, err := s.ensureColumn(ctx, "papers", migration.name, migration.ddl); err != nil {
			return err
		}
	}
	if err := s.ensureIndex(ctx, "papers", "idx_papers_doi", `CREATE INDEX idx_papers_doi ON papers (doi)`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, accountPaperFullTextsDDL); err != nil {
		return fmt.Errorf("create account_paper_fulltexts: %w", err)
	}
	return nil
}

const accountPaperFullTextsDDL = `CREATE TABLE IF NOT EXISTS account_paper_fulltexts (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  account_id    BIGINT UNSIGNED NOT NULL,
  paper_id      BIGINT UNSIGNED NOT NULL,
  content       MEDIUMTEXT      NOT NULL COMMENT '账号上传 PDF 提取并校验的私有全文；不保存 PDF',
  content_chars INT UNSIGNED    NOT NULL,
  created_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at    DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_account_paper_fulltexts (account_id, paper_id),
  KEY idx_account_paper_fulltexts_paper (paper_id),
  CONSTRAINT fk_account_paper_fulltexts_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
  CONSTRAINT fk_account_paper_fulltexts_paper FOREIGN KEY (paper_id) REFERENCES papers(id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='账号私有全文，只供上传账号精读'`

func (s *Store) validateResearchProjectSchema(ctx context.Context) error {
	required := map[string][]string{
		"accounts":             {"active_project_id"},
		"guest_sessions":       {"active_project_id", "imported_project_ids", "imported_current_project_id"},
		"projects":             {"owner_account_id", "owner_guest_id", "project_no", "active_conversation_id"},
		"conversations":        {"user_id", "project_id", "conversation_no"},
		"project_papers":       {"project_id", "paper_id", "status"},
		"project_read_results": {"project_paper_id", "message_id", "source_conversation_id", "read_type"},
	}
	missing := make([]string, 0)
	for table, columns := range required {
		for _, column := range columns {
			exists, err := s.columnExists(ctx, table, column)
			if err != nil {
				return err
			}
			if !exists {
				missing = append(missing, table+"."+column)
			}
		}
	}
	requiredConstraints := map[string][]string{
		"projects":             {"ck_projects_single_owner", "uk_projects_account_no", "uk_projects_guest_no", "fk_projects_account", "fk_projects_guest"},
		"conversations":        {"ck_conversations_single_owner", "uk_conversations_project_no", "fk_conversations_project"},
		"project_papers":       {"uk_project_papers_identity", "fk_project_papers_project", "fk_project_papers_paper"},
		"project_read_results": {"uk_project_read_results_message", "fk_project_read_results_project_paper", "fk_project_read_results_message", "fk_project_read_results_conversation"},
	}
	for table, names := range requiredConstraints {
		for _, name := range names {
			var exists bool
			err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.table_constraints WHERE constraint_schema=DATABASE() AND table_name=? AND constraint_name=?)`, table, name).Scan(&exists)
			if err != nil {
				return err
			}
			if !exists {
				missing = append(missing, table+"."+name)
			}
		}
	}
	requiredIndexes := map[string][]string{
		"projects":             {"idx_projects_account", "idx_projects_guest"},
		"conversations":        {"idx_conversations_project"},
		"project_papers":       {"idx_project_papers_list", "idx_project_papers_paper"},
		"project_read_results": {"idx_project_read_results_history", "idx_project_read_results_conversation"},
	}
	for table, names := range requiredIndexes {
		for _, name := range names {
			var exists bool
			err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?)`, table, name).Scan(&exists)
			if err != nil {
				return err
			}
			if !exists {
				missing = append(missing, table+"."+name)
			}
		}
	}
	forbidden := map[string][]string{"accounts": {"active_conversation_id"}, "guest_sessions": {"active_conversation_id", "imported_conversation_ids", "imported_current_conversation_id"}, "conversations": {"owner_account_id", "owner_guest_id"}}
	legacy := make([]string, 0)
	for table, columns := range forbidden {
		for _, column := range columns {
			exists, err := s.columnExists(ctx, table, column)
			if err != nil {
				return err
			}
			if exists {
				legacy = append(legacy, table+"."+column)
			}
		}
	}
	if len(missing) > 0 || len(legacy) > 0 {
		return fmt.Errorf("research-project schema is not installed (missing=%v legacy=%v); create and verify a backup, then execute docs/migrations/0009_research_projects.sql during a write outage", missing, legacy)
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, ddl string) (bool, error) {
	exists, err := s.columnExists(ctx, table, column)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if _, err = s.db.ExecContext(ctx, ddl); err != nil {
		return false, fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return true, nil
}
func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?)`, table, column).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check %s.%s: %w", table, column, err)
	}
	return exists, nil
}
func (s *Store) ensureIndex(ctx context.Context, table, index, ddl string) error {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name=? AND index_name=?)`, table, index).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err = s.db.ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("create %s.%s: %w", table, index, err)
	}
	return nil
}
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
func (s *Store) Close() error                   { return s.db.Close() }
