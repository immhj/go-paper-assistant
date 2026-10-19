-- Run only when upgrading a database created by the former Semantic Scholar schema.
-- Fresh installations already use (source, external_id) from docs/schema.sql.

ALTER TABLE papers
  ADD COLUMN source VARCHAR(32) NULL AFTER id,
  ADD COLUMN external_id VARCHAR(191) NULL AFTER source;

-- Preserve legacy rows instead of guessing an arXiv ID from the title.
UPDATE papers
SET source = 'semantic_scholar', external_id = CAST(s2_corpus_id AS CHAR)
WHERE s2_corpus_id IS NOT NULL;

-- The following query must return 0 before continuing. The old provider rejected
-- identity-less candidates, so a clean database should satisfy it. Stop and repair
-- manually rather than deleting rows referenced by messages.shown_papers.
SELECT COUNT(*) AS unmigratable_papers
FROM papers
WHERE source IS NULL OR external_id IS NULL;

ALTER TABLE papers
  MODIFY COLUMN source VARCHAR(32) NOT NULL,
  MODIFY COLUMN external_id VARCHAR(191) NOT NULL,
  DROP INDEX uk_papers_corpus,
  DROP COLUMN s2_corpus_id,
  ADD UNIQUE KEY uk_papers_identity (source, external_id);
