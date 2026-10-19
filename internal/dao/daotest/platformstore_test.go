package daotest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"paperassistant/internal/dao"
	"paperassistant/internal/dao/daotest"
	"paperassistant/internal/domain"
)

func TestFakePlatformStoreCreateAccountUpgradesGuestAtomically(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	store := daotest.NewFakePlatformStore()
	store.SetNow(func() time.Time { return now })
	hash := make([]byte, 32)
	hash[0] = 1
	guest, err := store.CreateGuestSession(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	current := uint64(12)
	if err = store.SeedGuestProjects(guest.ID, []uint64{11, 12}, &current); err != nil {
		t.Fatal(err)
	}

	account, imported, err := store.CreateAccount(ctx, domain.CreateAccountParams{
		Username: "account-1", Email: "a@example.com", Nickname: "A",
		PasswordHash: "hash", EmailVerifiedAt: now,
	}, &guest.ID)
	if err != nil {
		t.Fatal(err)
	}
	if account.AuthVersion != 1 || account.ActiveProjectID == nil || *account.ActiveProjectID != current {
		t.Fatalf("unexpected account: %+v", account)
	}
	if !imported.Upgraded || len(imported.ImportedProjectNos) != 2 {
		t.Fatalf("unexpected import: %+v", imported)
	}
	storedGuest, err := store.GetGuestSessionByTokenHash(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if storedGuest.Status != domain.GuestStatusUpgraded || storedGuest.UpgradedToAccountID == nil || *storedGuest.UpgradedToAccountID != account.ID {
		t.Fatalf("guest was not revoked: %+v", storedGuest)
	}
}

func TestFakePlatformStoreReservationBlocksUpgradeAndTransitions(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	store := daotest.NewFakePlatformStore()
	store.SetNow(func() time.Time { return now })
	hash := make([]byte, 32)
	hash[0] = 2
	guest, err := store.CreateGuestSession(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReserveGuestDeepRead(ctx, guest.ID, "reservation-1"); err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateAccount(ctx, domain.CreateAccountParams{
		Username: "account-2", Email: "b@example.com", Nickname: "B",
		PasswordHash: "hash", EmailVerifiedAt: now,
	}, &guest.ID)
	if !errors.Is(err, dao.ErrGuestTaskInProgress) {
		t.Fatalf("got %v, want guest task conflict", err)
	}
	if err = store.FinishGuestDeepRead(ctx, guest.ID, "reservation-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReserveGuestDeepRead(ctx, guest.ID, "reservation-2"); err != nil {
		t.Fatal(err)
	}
	if err = store.FinishGuestDeepRead(ctx, guest.ID, "reservation-2", true); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReserveGuestDeepRead(ctx, guest.ID, "reservation-3"); !errors.Is(err, dao.ErrGuestDeepReadExhausted) {
		t.Fatalf("got %v, want exhausted", err)
	}
}

func TestFakePlatformStoreAuthVersionAndIdempotentImport(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 3, 20, 12, 0, 0, 0, time.UTC)
	store := daotest.NewFakePlatformStore()
	store.SetNow(func() time.Time { return now })
	account, _, err := store.CreateAccount(ctx, domain.CreateAccountParams{
		Username: "account-3", Email: "c@example.com", Nickname: "C",
		PasswordHash: "old", EmailVerifiedAt: now,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := store.ResetPasswordByEmail(ctx, account.Email, "new")
	if err != nil || version != 2 {
		t.Fatalf("reset version=%d err=%v", version, err)
	}
	if err = store.SetAccountStatus(ctx, account.ID, domain.AccountStatusDisabled); err != nil {
		t.Fatal(err)
	}
	updated, err := store.GetAccountByEmail(ctx, account.Email)
	if err != nil || updated.AuthVersion != 3 {
		t.Fatalf("updated account=%+v err=%v", updated, err)
	}

	hash := make([]byte, 32)
	hash[0] = 3
	guest, err := store.CreateGuestSession(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.SeedGuestProjects(guest.ID, []uint64{21}, nil); err != nil {
		t.Fatal(err)
	}
	first, err := store.ImportGuestSession(ctx, account.ID, hash, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ImportGuestSession(ctx, account.ID, hash, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.AlreadyImported || !second.AlreadyImported || len(second.ImportedProjectNos) != 1 {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}
