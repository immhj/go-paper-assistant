package auth

import (
	"context"

	"paperassistant/internal/domain"
)

type contextKey string

const (
	usernameKey contextKey = "account-username"
	accountKey  contextKey = "account"
	guestKey    contextKey = "guest"
	clientIPKey contextKey = "client-ip"
)

// WithUsername is retained for compatibility. New middleware stores the complete durable account.
func WithUsername(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, usernameKey, username)
}

func UsernameFromContext(ctx context.Context) (string, bool) {
	if account, ok := AccountFromContext(ctx); ok {
		return account.Username, true
	}
	username, ok := ctx.Value(usernameKey).(string)
	return username, ok
}

func WithAccount(ctx context.Context, account domain.Account) context.Context {
	ctx = context.WithValue(ctx, accountKey, account)
	return context.WithValue(ctx, usernameKey, account.Username)
}

func AccountFromContext(ctx context.Context) (domain.Account, bool) {
	account, ok := ctx.Value(accountKey).(domain.Account)
	return account, ok
}

func WithGuest(ctx context.Context, guest domain.GuestSession) context.Context {
	return context.WithValue(ctx, guestKey, guest)
}

func GuestFromContext(ctx context.Context) (domain.GuestSession, bool) {
	guest, ok := ctx.Value(guestKey).(domain.GuestSession)
	return guest, ok
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

func ClientIPFromContext(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey).(string)
	return ip
}
