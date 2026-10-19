-- 0010: IEEE data source, venue ranks and account-private full text (ADR 0018, 0019, 0020).
--
-- Purely additive: nullable paper columns and one new table, no data rewrite. The application
-- applies the same DDL idempotently at start-up (internal/dao/mysql/store.go
-- ensureIEEEAndPrivateFullTextSchema); this file documents it for manual installs.

ALTER TABLE papers
  ADD COLUMN doi           VARCHAR(191) NULL COMMENT '小写 DOI；IEEE 来源时与 external_id 相同',
  ADD COLUMN issn          VARCHAR(255) NULL COMMENT '逗号分隔的 ISSN/eISSN',
  ADD COLUMN jcr_quartile  ENUM('Q1','Q2','Q3','Q4') NULL COMMENT 'impact_factor 数据按 ISSN 精确匹配',
  ADD COLUMN cas_quartile  TINYINT UNSIGNED NULL COMMENT '中科院分区 1-4；第三方整理，无版本与学科',
  ADD COLUMN impact_factor DECIMAL(9,3) NULL,
  ADD COLUMN rank_version  VARCHAR(128) NULL COMMENT '发表场所等级所依据的固定数据版本';

CREATE INDEX idx_papers_doi ON papers (doi);

CREATE TABLE IF NOT EXISTS account_paper_fulltexts (
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
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='账号私有全文，只供上传账号精读';
