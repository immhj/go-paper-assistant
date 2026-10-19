package controller

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"paperassistant/internal/auth"
	"paperassistant/internal/service"
)

const (
	guestCookieName   = "guest_session"
	guestCookieMaxAge = 30 * 24 * 60 * 60
)

// OptionalAuth resolves an account when Authorization is present. When it is absent, it
// resolves the guest cookie and creates a guest when needed. A malformed or invalid
// Authorization header is never downgraded to a guest identity.
func (p *Platform) OptionalAuth(next http.Handler) http.Handler {
	return p.actorAuth(true, next)
}

// ExistingAuth applies the same strict Authorization semantics as OptionalAuth, but only
// resolves an existing guest cookie. It is used by login and registration so those endpoints
// do not create otherwise-unused guest sessions.
func (p *Platform) ExistingAuth(next http.Handler) http.Handler {
	return p.actorAuth(false, next)
}

func (p *Platform) actorAuth(createGuest bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithClientIP(r.Context(), clientIP(r, p.config.TrustedProxyCIDRs))
		headers := r.Header.Values("Authorization")
		if len(headers) > 0 {
			if len(headers) != 1 {
				writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "身份凭证无效")
				return
			}
			token, ok := strictBearer(headers[0])
			if !ok {
				writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "身份凭证无效")
				return
			}
			account, err := p.service.Authenticate(ctx, token)
			if err != nil {
				if errors.Is(err, service.ErrInvalidToken) {
					writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "身份凭证无效")
				} else {
					writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "身份服务暂时不可用")
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(auth.WithAccount(ctx, account)))
			return
		}

		if cookie, err := r.Cookie(guestCookieName); err == nil && cookie.Value != "" {
			guest, resolveErr := p.service.ResolveGuest(ctx, cookie.Value)
			if resolveErr == nil {
				next.ServeHTTP(w, r.WithContext(auth.WithGuest(ctx, guest)))
				return
			}
			clearGuestCookie(w, p.secureCookies())
			if !errors.Is(resolveErr, service.ErrGuestUnavailable) {
				writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "访客会话服务暂时不可用")
				return
			}
		}
		if !createGuest {
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		token, guest, err := p.service.CreateGuest(ctx)
		if err != nil {
			if errors.Is(err, service.ErrGuestAccessDisabled) {
				writeStatusError(w, http.StatusForbidden, "GUEST_ACCESS_DISABLED", "访客访问暂未开放")
			} else {
				writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "访客会话服务暂时不可用")
			}
			return
		}
		setGuestCookie(w, token, p.secureCookies())
		next.ServeHTTP(w, r.WithContext(auth.WithGuest(ctx, guest)))
	})
}

// RequireAccount performs both JWT verification and the durable account status/version check.
func (p *Platform) RequireAccount(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithClientIP(r.Context(), clientIP(r, p.config.TrustedProxyCIDRs))
		headers := r.Header.Values("Authorization")
		if len(headers) != 1 {
			writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "需要有效的账号身份凭证")
			return
		}
		token, ok := strictBearer(headers[0])
		if !ok {
			writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "身份凭证无效")
			return
		}
		account, err := p.service.Authenticate(ctx, token)
		if err != nil {
			if errors.Is(err, service.ErrInvalidToken) {
				writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "身份凭证无效")
			} else {
				writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "身份服务暂时不可用")
			}
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithAccount(ctx, account)))
	})
}

func strictBearer(header string) (string, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func setGuestCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: guestCookieName, Value: token, Path: "/", MaxAge: guestCookieMaxAge,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func clearGuestCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: guestCookieName, Value: "", Path: "/", MaxAge: -1,
		Expires: time.Unix(1, 0).UTC(), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

// ClientIP returns the client address, honouring X-Forwarded-For only from trusted proxies.
func ClientIP(r *http.Request, trusted []netip.Prefix) string { return clientIP(r, trusted) }

func clientIP(r *http.Request, trusted []netip.Prefix) string {
	remote := remoteIP(r.RemoteAddr)
	if !isTrusted(remote, trusted) {
		return remote.String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			continue
		}
		candidate = candidate.Unmap()
		if !isTrusted(candidate, trusted) {
			return candidate.String()
		}
	}
	return remote.String()
}

func remoteIP(value string) netip.Addr {
	if addrPort, err := netip.ParseAddrPort(value); err == nil {
		return addrPort.Addr().Unmap()
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		if addr, parseErr := netip.ParseAddr(host); parseErr == nil {
			return addr.Unmap()
		}
	}
	addr, _ := netip.ParseAddr(strings.Trim(value, "[]"))
	return addr.Unmap()
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	if !addr.IsValid() {
		return false
	}
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
