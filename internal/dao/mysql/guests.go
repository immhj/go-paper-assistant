package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

const guestColumns = `id, token_hash, status, deep_read_state, reservation_id, reserved_at, expires_at, upgraded_to_account_id, active_project_id, import_idempotency_key, imported_project_ids, imported_current_project_id, created_at, updated_at`

var timeNow = time.Now

func (s *Store) CreateGuestSession(ctx context.Context, tokenHash []byte) (domain.GuestSession, error) {
	if len(tokenHash) != 32 {
		return domain.GuestSession{}, errors.New("guest token hash must be 32 bytes")
	}
	now := timeNow().UTC()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO guest_sessions (token_hash, status, deep_read_state, expires_at)
		VALUES (?, 'active', 'available', ?)`, tokenHash, now.Add(domain.GuestLifetime))
	if err != nil {
		if isDuplicateOnKey(err, "uk_guest_sessions_token_hash") {
			return domain.GuestSession{}, dao.ErrDuplicate
		}
		return domain.GuestSession{}, fmt.Errorf("create guest session: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return domain.GuestSession{}, fmt.Errorf("guest session id: %w", err)
	}
	return s.getGuestByID(ctx, uint64(id))
}

func (s *Store) GetGuestSessionByTokenHash(ctx context.Context, tokenHash []byte) (domain.GuestSession, error) {
	return scanGuest(s.db.QueryRowContext(ctx, `SELECT `+guestColumns+` FROM guest_sessions WHERE token_hash=?`, tokenHash))
}

func (s *Store) getGuestByID(ctx context.Context, id uint64) (domain.GuestSession, error) {
	return scanGuest(s.db.QueryRowContext(ctx, `SELECT `+guestColumns+` FROM guest_sessions WHERE id=?`, id))
}

func (s *Store) ImportGuestSession(ctx context.Context, accountID uint64, tokenHash []byte, idempotencyKey string) (domain.GuestImportResult, error) {
	if len(tokenHash) != 32 || idempotencyKey == "" {
		return domain.GuestImportResult{}, errors.New("guest import credential and idempotency key are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.GuestImportResult{}, fmt.Errorf("begin guest import: %w", err)
	}
	defer tx.Rollback()
	guest, err := scanGuest(tx.QueryRowContext(ctx, `SELECT `+guestColumns+` FROM guest_sessions WHERE token_hash=? FOR UPDATE`, tokenHash))
	if err != nil {
		return domain.GuestImportResult{}, dao.ErrGuestUnavailable
	}
	if guest.Status == domain.GuestStatusUpgraded && guest.UpgradedToAccountID != nil && *guest.UpgradedToAccountID == accountID && guest.ImportIdempotencyKey != nil && *guest.ImportIdempotencyKey == idempotencyKey {
		result, replayErr := importedProjectResult(ctx, tx, accountID, guest.ImportedProjectIDs, guest.ImportedCurrentProjectID)
		result.Upgraded, result.AlreadyImported = true, true
		return result, replayErr
	}
	if err = validateGuestForTransfer(guest); err != nil {
		return domain.GuestImportResult{}, err
	}
	var locked uint64
	if err = tx.QueryRowContext(ctx, `SELECT id FROM accounts WHERE id=? AND status='active' FOR UPDATE`, accountID).Scan(&locked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.GuestImportResult{}, dao.ErrNotFound
		}
		return domain.GuestImportResult{}, err
	}
	result, err := transferGuestToAccount(ctx, tx, guest, accountID, idempotencyKey)
	if err != nil {
		return domain.GuestImportResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return domain.GuestImportResult{}, fmt.Errorf("commit guest import: %w", err)
	}
	return result, nil
}

func validateGuestForTransfer(guest domain.GuestSession) error {
	if guest.Status != domain.GuestStatusActive || !guest.ExpiresAt.After(timeNow().UTC()) {
		return dao.ErrGuestUnavailable
	}
	if guest.DeepReadState == domain.GuestDeepReadReserved {
		return dao.ErrGuestDeepReadInProgress
	}
	return nil
}

type importedProject struct {
	id      uint64
	no      uint32
	status  string
	updated time.Time
}

func transferGuestToAccount(ctx context.Context, tx *sql.Tx, guest domain.GuestSession, accountID uint64, idempotencyKey string) (domain.GuestImportResult, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,project_no,status,updated_at FROM projects WHERE owner_guest_id=? ORDER BY project_no ASC,id ASC FOR UPDATE`, guest.ID)
	if err != nil {
		return domain.GuestImportResult{}, err
	}
	projects := make([]importedProject, 0)
	for rows.Next() {
		var p importedProject
		if err = rows.Scan(&p.id, &p.no, &p.status, &p.updated); err != nil {
			rows.Close()
			return domain.GuestImportResult{}, err
		}
		projects = append(projects, p)
	}
	if err = rows.Close(); err != nil {
		return domain.GuestImportResult{}, err
	}
	if err = rows.Err(); err != nil {
		return domain.GuestImportResult{}, err
	}
	used := map[uint32]bool{}
	var max uint32
	for _, project := range projects {
		if project.no > max {
			max = project.no
		}
	}
	existing, err := tx.QueryContext(ctx, `SELECT project_no FROM projects WHERE owner_account_id=? FOR UPDATE`, accountID)
	if err != nil {
		return domain.GuestImportResult{}, err
	}
	for existing.Next() {
		var no uint32
		if err = existing.Scan(&no); err != nil {
			existing.Close()
			return domain.GuestImportResult{}, err
		}
		used[no] = true
		if no > max {
			max = no
		}
	}
	if err = existing.Close(); err != nil {
		return domain.GuestImportResult{}, err
	}
	ids := make([]uint64, 0, len(projects))
	nos := make([]uint32, 0, len(projects))
	var current *uint64
	var fallback *importedProject
	for i := range projects {
		p := &projects[i]
		if used[p.no] {
			for {
				max++
				if !used[max] {
					p.no = max
					break
				}
			}
		}
		used[p.no] = true
		if p.no > max {
			max = p.no
		}
		if _, err = tx.ExecContext(ctx, `UPDATE projects SET project_no=?,owner_account_id=?,owner_guest_id=NULL WHERE id=? AND owner_guest_id=?`, p.no, accountID, p.id, guest.ID); err != nil {
			return domain.GuestImportResult{}, fmt.Errorf("transfer guest project: %w", err)
		}
		ids = append(ids, p.id)
		nos = append(nos, p.no)
		if p.status == domain.ProjectStatusActive {
			if guest.ActiveProjectID != nil && *guest.ActiveProjectID == p.id {
				v := p.id
				current = &v
			}
			if fallback == nil || p.updated.After(fallback.updated) || (p.updated.Equal(fallback.updated) && p.id > fallback.id) {
				fallback = p
			}
		}
	}
	if current == nil && fallback != nil {
		v := fallback.id
		current = &v
	}
	if current != nil {
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET active_project_id=? WHERE id=?`, *current, accountID); err != nil {
			return domain.GuestImportResult{}, err
		}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return domain.GuestImportResult{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE guest_sessions SET status='upgraded',upgraded_to_account_id=?,active_project_id=NULL,import_idempotency_key=?,imported_project_ids=?,imported_current_project_id=? WHERE id=? AND status='active'`, accountID, nullableStringValue(idempotencyKey), encoded, nullableUint64(current), guest.ID)
	if err != nil {
		return domain.GuestImportResult{}, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return domain.GuestImportResult{}, dao.ErrResourceStateConflict
	}
	var currentNo *uint32
	if current != nil {
		for i, id := range ids {
			if id == *current {
				v := nos[i]
				currentNo = &v
				break
			}
		}
	}
	return domain.GuestImportResult{Upgraded: true, ImportedProjectNos: nos, CurrentProjectNo: currentNo}, nil
}

func importedProjectResult(ctx context.Context, tx *sql.Tx, accountID uint64, ids []uint64, current *uint64) (domain.GuestImportResult, error) {
	result := domain.GuestImportResult{ImportedProjectNos: make([]uint32, 0, len(ids))}
	for _, id := range ids {
		var no uint32
		if err := tx.QueryRowContext(ctx, `SELECT project_no FROM projects WHERE id=? AND owner_account_id=?`, id, accountID).Scan(&no); err != nil {
			return domain.GuestImportResult{}, dao.ErrGuestUnavailable
		}
		result.ImportedProjectNos = append(result.ImportedProjectNos, no)
		if current != nil && id == *current {
			v := no
			result.CurrentProjectNo = &v
		}
	}
	return result, nil
}
func nullableStringValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func nullableUint64(value *uint64) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *Store) ClearGuestSession(ctx context.Context, guestID uint64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin clear guest: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	guest, err := loadGuestForUpdate(ctx, tx, guestID)
	if err != nil {
		return err
	}
	if guest.Status == domain.GuestStatusCleared {
		return nil
	}
	if guest.Status != domain.GuestStatusActive || !guest.ExpiresAt.After(timeNow().UTC()) {
		return dao.ErrGuestUnavailable
	}
	if guest.DeepReadState == domain.GuestDeepReadReserved {
		return dao.ErrGuestTaskInProgress
	}
	if _, err = tx.ExecContext(ctx, `UPDATE guest_sessions SET status='cleared', active_project_id=NULL,
			reservation_id=NULL, reserved_at=NULL WHERE id=?`, guestID); err != nil {
		return fmt.Errorf("revoke cleared guest: %w", err)
	}
	if err = deleteGuestContent(ctx, tx, guestID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ReserveGuestDeepRead(ctx context.Context, guestID uint64, reservationID string) (domain.GuestSession, error) {
	if reservationID == "" {
		return domain.GuestSession{}, errors.New("reservation id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.GuestSession{}, fmt.Errorf("begin guest deep read reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	guest, err := loadGuestForUpdate(ctx, tx, guestID)
	if err != nil {
		return domain.GuestSession{}, err
	}
	switch {
	case guest.Status != domain.GuestStatusActive || !guest.ExpiresAt.After(timeNow().UTC()):
		return domain.GuestSession{}, dao.ErrGuestUnavailable
	case guest.DeepReadState == domain.GuestDeepReadReserved:
		return domain.GuestSession{}, dao.ErrGuestTaskInProgress
	case guest.DeepReadState == domain.GuestDeepReadConsumed:
		return domain.GuestSession{}, dao.ErrGuestDeepReadExhausted
	case guest.DeepReadState != domain.GuestDeepReadAvailable:
		return domain.GuestSession{}, dao.ErrReservationMismatch
	}
	if _, err = tx.ExecContext(ctx, `
		UPDATE guest_sessions
		SET deep_read_state='reserved', reservation_id=?, reserved_at=CURRENT_TIMESTAMP(3)
		WHERE id=? AND status='active' AND deep_read_state='available'`, reservationID, guestID); err != nil {
		if isDuplicateOnKey(err, "uk_guest_sessions_reservation") {
			return domain.GuestSession{}, dao.ErrDuplicate
		}
		return domain.GuestSession{}, fmt.Errorf("reserve guest deep read: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return domain.GuestSession{}, fmt.Errorf("commit guest deep read reservation: %w", err)
	}
	now := timeNow().UTC()
	guest.DeepReadState = domain.GuestDeepReadReserved
	guest.ReservationID = &reservationID
	guest.ReservedAt = &now
	guest.UpdatedAt = now
	return guest, nil
}

func (s *Store) FinishGuestDeepRead(ctx context.Context, guestID uint64, reservationID string, success bool) error {
	state := domain.GuestDeepReadAvailable
	reservationValue := "NULL"
	reservedAtValue := "NULL"
	if success {
		state = domain.GuestDeepReadConsumed
		reservationValue = "reservation_id"
		reservedAtValue = "reserved_at"
	}
	query := fmt.Sprintf(`UPDATE guest_sessions SET deep_read_state=?, reservation_id=%s, reserved_at=%s
		WHERE id=? AND status='active' AND deep_read_state='reserved' AND reservation_id=?`, reservationValue, reservedAtValue)
	result, err := s.db.ExecContext(ctx, query, state, guestID, reservationID)
	if err != nil {
		return fmt.Errorf("finish guest deep read: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 1 {
		return nil
	}
	guest, err := s.getGuestByID(ctx, guestID)
	if err != nil {
		return err
	}
	if success && guest.DeepReadState == domain.GuestDeepReadConsumed && guest.ReservationID != nil && *guest.ReservationID == reservationID {
		return nil
	}
	if !success && guest.DeepReadState == domain.GuestDeepReadAvailable {
		return nil
	}
	return dao.ErrReservationMismatch
}

func (s *Store) CleanupExpiredGuests(ctx context.Context, limit int) (domain.GuestMetrics, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.GuestMetrics{}, fmt.Errorf("begin guest cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var metrics domain.GuestMetrics
	staleBefore := timeNow().UTC().Add(-domain.GuestReservationCleanupGrace)
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM guest_sessions
		WHERE status='active' AND expires_at<=CURRENT_TIMESTAMP(3)
		  AND deep_read_state='reserved' AND reserved_at>?`, staleBefore).Scan(&metrics.SkippedReserved); err != nil {
		return domain.GuestMetrics{}, fmt.Errorf("count reserved expired guests: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM guest_sessions
		WHERE status='active' AND expires_at<=CURRENT_TIMESTAMP(3)
		  AND (deep_read_state<>'reserved' OR reserved_at IS NULL OR reserved_at<=?)
		ORDER BY expires_at, id LIMIT ? FOR UPDATE SKIP LOCKED`, staleBefore, limit)
	if err != nil {
		return domain.GuestMetrics{}, fmt.Errorf("select expired guests: %w", err)
	}
	ids := make([]uint64, 0, limit)
	for rows.Next() {
		var id uint64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return domain.GuestMetrics{}, fmt.Errorf("scan expired guest: %w", err)
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return domain.GuestMetrics{}, err
	}
	if err = rows.Err(); err != nil {
		return domain.GuestMetrics{}, err
	}
	metrics.Scanned = int64(len(ids))
	for _, id := range ids {
		var projects int64
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE owner_guest_id=?`, id).Scan(&projects); err != nil {
			return domain.GuestMetrics{}, fmt.Errorf("count expired guest projects: %w", err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE guest_sessions SET status='expired', active_project_id=NULL WHERE id=?`, id); err != nil {
			return domain.GuestMetrics{}, fmt.Errorf("expire guest: %w", err)
		}
		if err = deleteGuestContent(ctx, tx, id); err != nil {
			return domain.GuestMetrics{}, err
		}
		metrics.DeletedSessions++
		metrics.DeletedProjects += projects
	}
	if err = tx.Commit(); err != nil {
		return domain.GuestMetrics{}, fmt.Errorf("commit guest cleanup: %w", err)
	}
	return metrics, nil
}

func deleteGuestContent(ctx context.Context, tx *sql.Tx, guestID uint64) error {
	// projects owns the full Web subtree through final-schema foreign keys. Shared papers and
	// every QQ user/conversation are outside this cascade.
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE owner_guest_id=?`, guestID); err != nil {
		return fmt.Errorf("delete guest project tree: %w", err)
	}
	return nil
}

func (s *Store) GuestMetrics(ctx context.Context) (domain.GuestMetrics, error) {
	var metrics domain.GuestMetrics
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status='active'),0), COALESCE(SUM(status='upgraded'),0),
		COALESCE(SUM(status='cleared'),0), COALESCE(SUM(status='expired'),0),
		COALESCE(SUM(deep_read_state='available'),0), COALESCE(SUM(deep_read_state='reserved'),0),
		COALESCE(SUM(deep_read_state='consumed'),0) FROM guest_sessions`).Scan(
		&metrics.Active, &metrics.Upgraded, &metrics.Cleared, &metrics.Expired,
		&metrics.Available, &metrics.Reserved, &metrics.Consumed)
	if err != nil {
		return domain.GuestMetrics{}, fmt.Errorf("guest metrics: %w", err)
	}
	return metrics, nil
}

func loadGuestForUpdate(ctx context.Context, tx *sql.Tx, id uint64) (domain.GuestSession, error) {
	return scanGuest(tx.QueryRowContext(ctx, `SELECT `+guestColumns+` FROM guest_sessions WHERE id=? FOR UPDATE`, id))
}

func scanGuest(row interface{ Scan(...any) error }) (domain.GuestSession, error) {
	var guest domain.GuestSession
	var reservation, importKey sql.NullString
	var reservedAt sql.NullTime
	var upgraded, active, importedCurrent sql.NullInt64
	var importedIDs []byte
	if err := row.Scan(&guest.ID, &guest.TokenHash, &guest.Status, &guest.DeepReadState,
		&reservation, &reservedAt, &guest.ExpiresAt, &upgraded, &active, &importKey,
		&importedIDs, &importedCurrent, &guest.CreatedAt, &guest.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.GuestSession{}, dao.ErrNotFound
		}
		return domain.GuestSession{}, fmt.Errorf("scan guest session: %w", err)
	}
	guest.TokenHash = append([]byte(nil), guest.TokenHash...)
	if reservation.Valid {
		guest.ReservationID = &reservation.String
	}
	if reservedAt.Valid {
		guest.ReservedAt = &reservedAt.Time
	}
	guest.UpgradedToAccountID = uint64Pointer(upgraded)
	guest.ActiveProjectID = uint64Pointer(active)
	if importKey.Valid {
		guest.ImportIdempotencyKey = &importKey.String
	}
	if len(importedIDs) > 0 {
		if err := json.Unmarshal(importedIDs, &guest.ImportedProjectIDs); err != nil {
			return domain.GuestSession{}, fmt.Errorf("decode imported project ids: %w", err)
		}
	}
	guest.ImportedCurrentProjectID = uint64Pointer(importedCurrent)
	return guest, nil
}
