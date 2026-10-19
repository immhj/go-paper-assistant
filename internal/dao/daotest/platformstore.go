// Package daotest provides in-memory implementations of DAO seams for behavioral tests.
package daotest

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/domain"
)

// FakePlatformStore reproduces account uniqueness, guest terminal states, idempotent imports,
// and deep-read conditional transitions without depending on MySQL.
type FakePlatformStore struct {
	mu            sync.Mutex
	accounts      []domain.Account
	guests        map[uint64]domain.GuestSession
	guestByToken  map[string]uint64
	nextAccountID uint64
	nextGuestID   uint64
	now           func() time.Time
}

func NewFakePlatformStore() *FakePlatformStore {
	return &FakePlatformStore{
		guests:       make(map[uint64]domain.GuestSession),
		guestByToken: make(map[string]uint64),
		now:          time.Now,
	}
}

// SetNow makes expiry and reservation tests deterministic.
func (f *FakePlatformStore) SetNow(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// SeedGuestProjects records internal project IDs used to model an in-place import. Their
// numeric values also serve as stable project numbers in this focused platform fake.
func (f *FakePlatformStore) SeedGuestProjects(guestID uint64, ids []uint64, current *uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	guest, ok := f.guests[guestID]
	if !ok {
		return dao.ErrNotFound
	}
	guest.ImportedProjectIDs = append([]uint64(nil), ids...)
	guest.ActiveProjectID = cloneUint64(current)
	f.guests[guestID] = guest
	return nil
}

func (f *FakePlatformStore) CreateAccount(_ context.Context, params domain.CreateAccountParams, guestID *uint64) (domain.Account, domain.GuestImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if params.EmailVerifiedAt.IsZero() {
		return domain.Account{}, domain.GuestImportResult{}, errors.New("email verification time is required")
	}
	for _, account := range f.accounts {
		if account.Email == params.Email || account.Username == params.Username {
			return domain.Account{}, domain.GuestImportResult{}, dao.ErrDuplicate
		}
	}
	var guest domain.GuestSession
	if guestID != nil {
		var ok bool
		guest, ok = f.guests[*guestID]
		if !ok {
			return domain.Account{}, domain.GuestImportResult{}, dao.ErrNotFound
		}
		if err := f.validateTransfer(guest); err != nil {
			return domain.Account{}, domain.GuestImportResult{}, err
		}
	}
	now := f.now()
	verified := params.EmailVerifiedAt
	f.nextAccountID++
	account := domain.Account{
		ID: f.nextAccountID, Username: params.Username, Email: params.Email,
		Nickname: params.Nickname, PasswordHash: params.PasswordHash,
		Status: domain.AccountStatusActive, AuthVersion: 1, EmailVerifiedAt: &verified,
		CreatedAt: now, UpdatedAt: now,
	}
	result := domain.GuestImportResult{ImportedProjectNos: []uint32{}}
	if guestID != nil {
		result = f.transferGuest(&guest, account.ID, "")
		account.ActiveProjectID = cloneUint64(guest.ImportedCurrentProjectID)
		f.guests[*guestID] = guest
	}
	f.accounts = append(f.accounts, account)
	return account, result, nil
}

func (f *FakePlatformStore) GetAccountByEmail(_ context.Context, email string) (domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, account := range f.accounts {
		if account.Email == email {
			return account, nil
		}
	}
	return domain.Account{}, dao.ErrNotFound
}

func (f *FakePlatformStore) GetAccountByUsername(_ context.Context, username string) (domain.Account, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, account := range f.accounts {
		if account.Username == username {
			return account, nil
		}
	}
	return domain.Account{}, dao.ErrNotFound
}

func (f *FakePlatformStore) ResetPasswordByEmail(_ context.Context, email, passwordHash string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.accounts {
		if f.accounts[index].Email == email {
			f.accounts[index].PasswordHash = passwordHash
			f.accounts[index].AuthVersion++
			f.accounts[index].UpdatedAt = f.now()
			return f.accounts[index].AuthVersion, nil
		}
	}
	return 0, dao.ErrNotFound
}

func (f *FakePlatformStore) UpdateNickname(_ context.Context, username, nickname string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.accounts {
		if f.accounts[index].Username == username {
			f.accounts[index].Nickname = nickname
			f.accounts[index].UpdatedAt = f.now()
			return nil
		}
	}
	return dao.ErrNotFound
}

func (f *FakePlatformStore) ListAccounts(_ context.Context, limit int) ([]domain.AdminAccountView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 {
		limit = len(f.accounts)
	}
	views := make([]domain.AdminAccountView, 0, min(limit, len(f.accounts)))
	for index := len(f.accounts) - 1; index >= 0 && len(views) < limit; index-- {
		account := f.accounts[index]
		views = append(views, domain.AdminAccountView{
			ID: account.ID, Email: account.Email, Nickname: account.Nickname,
			EmailVerifiedAt: cloneTime(account.EmailVerifiedAt), QQBound: account.QQOpenID != nil,
			Status: account.Status, CreatedAt: account.CreatedAt,
		})
	}
	return views, nil
}

func (f *FakePlatformStore) SetAccountStatus(_ context.Context, id uint64, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for index := range f.accounts {
		if f.accounts[index].ID == id {
			f.accounts[index].Status = status
			f.accounts[index].AuthVersion++
			f.accounts[index].UpdatedAt = f.now()
			return nil
		}
	}
	return dao.ErrNotFound
}

func (f *FakePlatformStore) CreateGuestSession(_ context.Context, tokenHash []byte) (domain.GuestSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(tokenHash) != 32 {
		return domain.GuestSession{}, errors.New("guest token hash must be 32 bytes")
	}
	key := hex.EncodeToString(tokenHash)
	if _, exists := f.guestByToken[key]; exists {
		return domain.GuestSession{}, dao.ErrDuplicate
	}
	now := f.now()
	f.nextGuestID++
	guest := domain.GuestSession{
		ID: f.nextGuestID, TokenHash: append([]byte(nil), tokenHash...),
		Status: domain.GuestStatusActive, DeepReadState: domain.GuestDeepReadAvailable,
		ExpiresAt: now.Add(domain.GuestLifetime), CreatedAt: now, UpdatedAt: now,
	}
	f.guests[guest.ID] = guest
	f.guestByToken[key] = guest.ID
	return cloneGuest(guest), nil
}

func (f *FakePlatformStore) GetGuestSessionByTokenHash(_ context.Context, tokenHash []byte) (domain.GuestSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.guestByToken[hex.EncodeToString(tokenHash)]
	if !ok {
		return domain.GuestSession{}, dao.ErrNotFound
	}
	return cloneGuest(f.guests[id]), nil
}

func (f *FakePlatformStore) ImportGuestSession(_ context.Context, accountID uint64, tokenHash []byte, idempotencyKey string) (domain.GuestImportResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	guestID, tokenExists := f.guestByToken[hex.EncodeToString(tokenHash)]
	if idempotencyKey == "" {
		return domain.GuestImportResult{}, errors.New("guest import idempotency key is required")
	}
	guest, ok := f.guests[guestID]
	if !tokenExists || !ok {
		return domain.GuestImportResult{}, dao.ErrGuestUnavailable
	}
	if guest.Status == domain.GuestStatusUpgraded && guest.UpgradedToAccountID != nil &&
		*guest.UpgradedToAccountID == accountID && guest.ImportIdempotencyKey != nil &&
		*guest.ImportIdempotencyKey == idempotencyKey {
		return importResultFromIDs(guest.ImportedProjectIDs, guest.ImportedCurrentProjectID, true), nil
	}
	if err := f.validateTransfer(guest); err != nil {
		return domain.GuestImportResult{}, err
	}
	accountIndex := -1
	for index := range f.accounts {
		if f.accounts[index].ID == accountID {
			accountIndex = index
			break
		}
	}
	if accountIndex < 0 {
		return domain.GuestImportResult{}, dao.ErrNotFound
	}
	result := f.transferGuest(&guest, accountID, idempotencyKey)
	f.accounts[accountIndex].ActiveProjectID = cloneUint64(guest.ImportedCurrentProjectID)
	f.guests[guestID] = guest
	return result, nil
}

func (f *FakePlatformStore) validateTransfer(guest domain.GuestSession) error {
	if guest.Status != domain.GuestStatusActive || !guest.ExpiresAt.After(f.now()) {
		return dao.ErrGuestUnavailable
	}
	if guest.DeepReadState == domain.GuestDeepReadReserved {
		return dao.ErrGuestTaskInProgress
	}
	return nil
}

func (f *FakePlatformStore) transferGuest(guest *domain.GuestSession, accountID uint64, idempotencyKey string) domain.GuestImportResult {
	ids := append([]uint64(nil), guest.ImportedProjectIDs...)
	current := validCurrent(guest.ActiveProjectID, ids)
	if current == nil && len(ids) > 0 {
		value := ids[len(ids)-1]
		current = &value
	}
	guest.Status = domain.GuestStatusUpgraded
	guest.UpgradedToAccountID = &accountID
	guest.ActiveProjectID = nil
	guest.ImportedProjectIDs = ids
	guest.ImportedCurrentProjectID = cloneUint64(current)
	if idempotencyKey != "" {
		guest.ImportIdempotencyKey = &idempotencyKey
	}
	guest.UpdatedAt = f.now()
	return importResultFromIDs(ids, current, false)
}

func (f *FakePlatformStore) ClearGuestSession(_ context.Context, guestID uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	guest, ok := f.guests[guestID]
	if !ok {
		return dao.ErrNotFound
	}
	if guest.Status == domain.GuestStatusCleared {
		return nil
	}
	if err := f.validateTransfer(guest); err != nil {
		return err
	}
	guest.Status = domain.GuestStatusCleared
	guest.ActiveProjectID = nil
	guest.ImportedProjectIDs = nil
	guest.ReservationID = nil
	guest.ReservedAt = nil
	guest.UpdatedAt = f.now()
	f.guests[guestID] = guest
	return nil
}

func (f *FakePlatformStore) ReserveGuestDeepRead(_ context.Context, guestID uint64, reservationID string) (domain.GuestSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if reservationID == "" {
		return domain.GuestSession{}, errors.New("reservation id is required")
	}
	for id, other := range f.guests {
		if id != guestID && other.ReservationID != nil && *other.ReservationID == reservationID {
			return domain.GuestSession{}, dao.ErrDuplicate
		}
	}
	guest, ok := f.guests[guestID]
	if !ok {
		return domain.GuestSession{}, dao.ErrNotFound
	}
	if guest.Status != domain.GuestStatusActive || !guest.ExpiresAt.After(f.now()) {
		return domain.GuestSession{}, dao.ErrGuestUnavailable
	}
	switch guest.DeepReadState {
	case domain.GuestDeepReadReserved:
		return domain.GuestSession{}, dao.ErrGuestTaskInProgress
	case domain.GuestDeepReadConsumed:
		return domain.GuestSession{}, dao.ErrGuestDeepReadExhausted
	}
	now := f.now()
	guest.DeepReadState = domain.GuestDeepReadReserved
	guest.ReservationID = &reservationID
	guest.ReservedAt = &now
	guest.UpdatedAt = now
	f.guests[guestID] = guest
	return cloneGuest(guest), nil
}

func (f *FakePlatformStore) FinishGuestDeepRead(_ context.Context, guestID uint64, reservationID string, success bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	guest, ok := f.guests[guestID]
	if !ok {
		return dao.ErrNotFound
	}
	if success && guest.DeepReadState == domain.GuestDeepReadConsumed && guest.ReservationID != nil && *guest.ReservationID == reservationID {
		return nil
	}
	if !success && guest.DeepReadState == domain.GuestDeepReadAvailable {
		return nil
	}
	if guest.Status != domain.GuestStatusActive || guest.DeepReadState != domain.GuestDeepReadReserved ||
		guest.ReservationID == nil || *guest.ReservationID != reservationID {
		return dao.ErrReservationMismatch
	}
	if success {
		guest.DeepReadState = domain.GuestDeepReadConsumed
	} else {
		guest.DeepReadState = domain.GuestDeepReadAvailable
		guest.ReservationID = nil
		guest.ReservedAt = nil
	}
	guest.UpdatedAt = f.now()
	f.guests[guestID] = guest
	return nil
}

func (f *FakePlatformStore) CleanupExpiredGuests(_ context.Context, limit int) (domain.GuestMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	var metrics domain.GuestMetrics
	for id, guest := range f.guests {
		if guest.Status != domain.GuestStatusActive || guest.ExpiresAt.After(f.now()) {
			continue
		}
		if guest.DeepReadState == domain.GuestDeepReadReserved && guest.ReservedAt != nil &&
			guest.ReservedAt.After(f.now().Add(-domain.GuestReservationCleanupGrace)) {
			metrics.SkippedReserved++
			continue
		}
		if metrics.Scanned >= int64(limit) {
			continue
		}
		metrics.Scanned++
		metrics.DeletedSessions++
		metrics.DeletedProjects += int64(len(guest.ImportedProjectIDs))
		guest.Status = domain.GuestStatusExpired
		guest.ActiveProjectID = nil
		guest.ImportedProjectIDs = nil
		guest.UpdatedAt = f.now()
		f.guests[id] = guest
	}
	return metrics, nil
}

func (f *FakePlatformStore) GuestMetrics(_ context.Context) (domain.GuestMetrics, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var metrics domain.GuestMetrics
	for _, guest := range f.guests {
		switch guest.Status {
		case domain.GuestStatusActive:
			metrics.Active++
		case domain.GuestStatusUpgraded:
			metrics.Upgraded++
		case domain.GuestStatusCleared:
			metrics.Cleared++
		case domain.GuestStatusExpired:
			metrics.Expired++
		}
		switch guest.DeepReadState {
		case domain.GuestDeepReadAvailable:
			metrics.Available++
		case domain.GuestDeepReadReserved:
			metrics.Reserved++
		case domain.GuestDeepReadConsumed:
			metrics.Consumed++
		}
	}
	return metrics, nil
}

func validCurrent(current *uint64, ids []uint64) *uint64 {
	if current == nil {
		return nil
	}
	for _, id := range ids {
		if id == *current {
			value := id
			return &value
		}
	}
	return nil
}

func cloneGuest(guest domain.GuestSession) domain.GuestSession {
	guest.TokenHash = append([]byte(nil), guest.TokenHash...)
	guest.ImportedProjectIDs = append([]uint64(nil), guest.ImportedProjectIDs...)
	guest.ActiveProjectID = cloneUint64(guest.ActiveProjectID)
	guest.UpgradedToAccountID = cloneUint64(guest.UpgradedToAccountID)
	guest.ImportedCurrentProjectID = cloneUint64(guest.ImportedCurrentProjectID)
	guest.ReservationID = cloneString(guest.ReservationID)
	guest.ImportIdempotencyKey = cloneString(guest.ImportIdempotencyKey)
	guest.ReservedAt = cloneTime(guest.ReservedAt)
	return guest
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func importResultFromIDs(ids []uint64, current *uint64, replay bool) domain.GuestImportResult {
	result := domain.GuestImportResult{Upgraded: true, AlreadyImported: replay, ImportedProjectNos: make([]uint32, 0, len(ids))}
	for _, id := range ids {
		no := uint32(id)
		result.ImportedProjectNos = append(result.ImportedProjectNos, no)
		if current != nil && id == *current {
			v := no
			result.CurrentProjectNo = &v
		}
	}
	return result
}

func (f *FakePlatformStore) GetResearchSelection(_ context.Context, owner domain.ProjectOwner) (domain.ResearchSelection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var active *uint64
	if owner.IsAccount() {
		for _, account := range f.accounts {
			if account.ID == owner.ID {
				active = account.ActiveProjectID
				break
			}
		}
	} else if guest, ok := f.guests[owner.ID]; ok {
		active = guest.ActiveProjectID
	}
	if active == nil {
		return domain.ResearchSelection{}, nil
	}
	no := uint32(*active)
	return domain.ResearchSelection{ActiveProjectNo: &no}, nil
}
func (f *FakePlatformStore) HasProjects(_ context.Context, owner domain.ProjectOwner) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if owner.IsGuest() {
		guest, ok := f.guests[owner.ID]
		return ok && len(guest.ImportedProjectIDs) > 0, nil
	}
	for _, account := range f.accounts {
		if account.ID == owner.ID {
			return account.ActiveProjectID != nil, nil
		}
	}
	return false, nil
}
