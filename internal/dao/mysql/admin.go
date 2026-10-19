package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"paperassistant/internal/domain"
)

// traceScope restricts aggregation to the rows that carry an agent trace. The trace is written
// on the inbound user row, so one such row is exactly one turn of the router/executor pair.
const traceScope = `role='user' AND agent_trace IS NOT NULL AND created_at>=?`

// traceCost extracts one usage cost out of the trace. Rows without the key yield NULL, which
// SUM skips, so a partially traced day still reports the costs it does know about.
func traceCost(path string) string {
	return `SUM(CAST(agent_trace->>'$.` + path + `.costUsd' AS DOUBLE))`
}

func (s *Store) Overview(ctx context.Context, days int) (domain.Overview, error) {
	if days <= 0 || days > 365 {
		days = 7
	}
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -days+1)
	result := domain.Overview{Daily: make([]domain.DailyPoint, 0), Routes: make([]domain.LabelCount, 0), Tools: make([]domain.LabelCount, 0)}
	row := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users),
		(SELECT COUNT(*) FROM users WHERE last_active_at>=?),
		(SELECT COUNT(*) FROM messages WHERE role='user' AND created_at>=?),
		(SELECT COUNT(*) FROM papers),
		(SELECT COALESCE(SUM(cost_usd),0) FROM messages WHERE created_at>=?),
		(SELECT COUNT(*) FROM messages WHERE role='bot' AND delivery_status='failed' AND created_at>=?)`,
		from, from, from, from)
	if err := row.Scan(&result.Users, &result.ActiveUsers, &result.InboundMessages, &result.Papers, &result.CostUSD, &result.DeliveryFailures); err != nil {
		return domain.Overview{}, fmt.Errorf("overview totals: %w", err)
	}
	traceRow := s.db.QueryRowContext(ctx, `SELECT
		COUNT(*),
		COALESCE(SUM(JSON_LENGTH(agent_trace->'$.plan')>1),0),
		COALESCE(SUM(agent_trace->>'$.status'='failed'),0),
		COALESCE(SUM(agent_trace->>'$.tool'='search_papers'),0),
		COALESCE(`+traceCost("routerUsage")+`,0),
		COALESCE(`+traceCost("executorUsage")+`,0),
		COALESCE(`+traceCost("toolUsage")+`,0)
		FROM messages WHERE `+traceScope, from)
	if err := traceRow.Scan(&result.Turns, &result.MultiStepTurns, &result.FailedTurns, &result.Searches, &result.RouterCostUSD, &result.ExecutorCostUSD, &result.ToolCostUSD); err != nil {
		return domain.Overview{}, fmt.Errorf("overview trace totals: %w", err)
	}
	var err error
	if result.Routes, err = s.traceDistribution(ctx, "route", from); err != nil {
		return domain.Overview{}, err
	}
	if result.Tools, err = s.toolDistribution(ctx, from); err != nil {
		return domain.Overview{}, err
	}
	if result.Daily, err = s.overviewDaily(ctx, from); err != nil {
		return domain.Overview{}, err
	}
	return result, nil
}

// traceDistribution groups traced turns by one top-level trace field. The field name is not
// user input: only the literal below reaches it.
func (s *Store) traceDistribution(ctx context.Context, field string, from time.Time) ([]domain.LabelCount, error) {
	if field != "route" {
		return nil, fmt.Errorf("unsupported trace distribution field %q", field)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT agent_trace->>'$.`+field+`' AS label, COUNT(*),
		COALESCE(`+traceCost("routerUsage")+`,0)+COALESCE(`+traceCost("executorUsage")+`,0)+COALESCE(`+traceCost("toolUsage")+`,0)
		FROM messages WHERE `+traceScope+` AND agent_trace->>'$.`+field+`' IS NOT NULL
		GROUP BY label ORDER BY COUNT(*) DESC, label`, from)
	if err != nil {
		return nil, fmt.Errorf("overview %s distribution: %w", field, err)
	}
	defer rows.Close()
	items := make([]domain.LabelCount, 0)
	for rows.Next() {
		var item domain.LabelCount
		if err = rows.Scan(&item.Label, &item.Count, &item.CostUSD); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// toolDistribution counts executed tools per plan step (ADR 0021): a version-3 trace
// contributes one row per step that ran, with that step's own cost; older traces without a
// plan contribute their single tool with the whole turn's cost, as before.
func (s *Store) toolDistribution(ctx context.Context, from time.Time) ([]domain.LabelCount, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT label, COUNT(*), COALESCE(SUM(cost),0) FROM (
		SELECT step.tool AS label, step.cost AS cost
		FROM messages, JSON_TABLE(agent_trace, '$.plan[*]' COLUMNS(
			tool VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PATH '$.tool',
			status VARCHAR(16) PATH '$.status',
			cost DOUBLE PATH '$.usage.costUsd')) AS step
		WHERE `+traceScope+` AND step.status<>'skipped'
		UNION ALL
		SELECT agent_trace->>'$.tool' AS label,
			COALESCE(CAST(agent_trace->>'$.routerUsage.costUsd' AS DOUBLE),0)+COALESCE(CAST(agent_trace->>'$.executorUsage.costUsd' AS DOUBLE),0)+COALESCE(CAST(agent_trace->>'$.toolUsage.costUsd' AS DOUBLE),0) AS cost
		FROM messages WHERE `+traceScope+` AND agent_trace->'$.plan' IS NULL AND agent_trace->>'$.tool' IS NOT NULL
	) AS tools GROUP BY label ORDER BY COUNT(*) DESC, label`, from, from)
	if err != nil {
		return nil, fmt.Errorf("overview tool distribution: %w", err)
	}
	defer rows.Close()
	items := make([]domain.LabelCount, 0)
	for rows.Next() {
		var item domain.LabelCount
		if err = rows.Scan(&item.Label, &item.Count, &item.CostUSD); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *Store) overviewDaily(ctx context.Context, from time.Time) ([]domain.DailyPoint, error) {
	// The three split costs are intentionally not COALESCEd: a day with no traced turn returns
	// NULL and the chart draws a gap rather than a zero.
	// Grouping by the select alias keeps only_full_group_by satisfied; grouping by
	// DATE(created_at) while selecting DATE_FORMAT(...) does not count as functionally
	// dependent and is rejected.
	rows, err := s.db.QueryContext(ctx, `SELECT DATE_FORMAT(created_at,'%Y-%m-%d') AS day,
		COALESCE(SUM(role='user'),0),
		COALESCE(SUM(agent_trace->>'$.tool'='search_papers'),0),
		COALESCE(SUM(cost_usd),0),
		COALESCE(SUM(role='user' AND agent_trace IS NOT NULL),0),
		COALESCE(SUM(agent_trace->>'$.status'='failed'),0),
		`+traceCost("routerUsage")+`,
		`+traceCost("executorUsage")+`,
		`+traceCost("toolUsage")+`
		FROM messages WHERE created_at>=? GROUP BY day ORDER BY day`, from)
	if err != nil {
		return nil, fmt.Errorf("overview daily: %w", err)
	}
	defer rows.Close()
	items := make([]domain.DailyPoint, 0)
	for rows.Next() {
		var point domain.DailyPoint
		var router, executor, tool sql.NullFloat64
		if err = rows.Scan(&point.Date, &point.InboundMessages, &point.Searches, &point.CostUSD,
			&point.Turns, &point.FailedTurns, &router, &executor, &tool); err != nil {
			return nil, err
		}
		point.RouterCostUSD, point.ExecutorCostUSD, point.ToolCostUSD = nullFloat(router), nullFloat(executor), nullFloat(tool)
		items = append(items, point)
	}
	return items, rows.Err()
}

func nullFloat(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func (s *Store) ListAdminUsers(ctx context.Context, limit int) ([]domain.AdminUser, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.qq_openid, u.last_active_at,
		COUNT(DISTINCT c.id), COUNT(m.id), COALESCE(SUM(m.cost_usd),0)
		FROM users u
		LEFT JOIN conversations c ON c.user_id=u.id
		LEFT JOIN messages m ON m.conversation_id=c.id
		GROUP BY u.id ORDER BY u.last_active_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list admin users: %w", err)
	}
	defer rows.Close()
	items := make([]domain.AdminUser, 0)
	for rows.Next() {
		var item domain.AdminUser
		var openID string
		if err = rows.Scan(&item.ID, &openID, &item.LastActiveAt, &item.Conversations, &item.Messages, &item.CostUSD); err != nil {
			return nil, err
		}
		item.OpenIDMasked = maskOpenID(openID)
		items = append(items, item)
	}
	return items, rows.Err()
}
