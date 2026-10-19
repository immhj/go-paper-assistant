package controller

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"paperassistant/internal/auth"
	"paperassistant/internal/config"
	"paperassistant/internal/dao"
	"paperassistant/internal/provider/ratelimit"
	"paperassistant/internal/provider/verification"
	"paperassistant/internal/service"
)

// Platform serves account, guest-session, profile, and platform administration APIs.
type Platform struct {
	service    *service.Platform
	config     config.Config
	apiLimiter ratelimit.Store
}

func NewPlatform(value *service.Platform, cfg config.Config) *Platform {
	return &Platform{service: value, config: cfg}
}

// WithAPIRateLimit enables the per-client-IP budget of cfg.Platform.APIPerMinute requests per
// minute across all /api/ routes.
func (p *Platform) WithAPIRateLimit(store ratelimit.Store) *Platform {
	p.apiLimiter = store
	return p
}

// APIRateLimit applies the per-IP budget to /api/ paths only; static assets and /healthz are
// exempt. It fails open when the store is unavailable so a Redis outage does not take the API
// down with it.
func (p *Platform) APIRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p.apiLimiter == nil || !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r, p.config.TrustedProxyCIDRs)
		count, ttl, err := p.apiLimiter.Hit(r.Context(), ratelimit.Key("api:ip", ip), time.Minute)
		if err != nil {
			slog.Warn("API rate limiter unavailable; allowing request", "error", err)
			next.ServeHTTP(w, r)
			return
		}
		if count > int64(p.config.Platform.APIPerMinute) {
			setRetryAfter(w, ttl)
			writeStatusError(w, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁，请稍后重试")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	seconds := int(math.Ceil(wait.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
}

func (p *Platform) secureCookies() bool { return p.config.Environment == "production" }

func (p *Platform) RegisterCode(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Email    string `json:"email"`
		Nickname string `json:"nickname"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	result, err := p.service.RequestRegistrationCode(r.Context(), payload.Email, payload.Nickname, payload.Password, auth.ClientIPFromContext(r.Context()))
	if err != nil {
		writePlatformError(w, err, "发送注册验证码失败")
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (p *Platform) Register(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		RegistrationToken string `json:"registrationToken"`
		Code              string `json:"code"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	var guestID *uint64
	if guest, ok := auth.GuestFromContext(r.Context()); ok {
		guestID = &guest.ID
	}
	result, err := p.service.Register(r.Context(), payload.RegistrationToken, payload.Code, guestID)
	if err != nil {
		writePlatformError(w, err, "注册失败，请稍后重试")
		return
	}
	if guestID != nil {
		clearGuestCookie(w, p.secureCookies())
	}
	writeJSON(w, http.StatusCreated, result)
}

func (p *Platform) Login(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	result, err := p.service.Login(r.Context(), payload.Email, payload.Password, auth.ClientIPFromContext(r.Context()))
	if err != nil {
		writePlatformError(w, err, "登录失败，请稍后重试")
		return
	}
	if _, ok := auth.GuestFromContext(r.Context()); ok {
		if cookie, cookieErr := r.Cookie(guestCookieName); cookieErr == nil {
			result.GuestImportAvailable = p.service.GuestImportAvailable(r.Context(), cookie.Value)
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func (p *Platform) PasswordResetCode(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Email string `json:"email"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	result, err := p.service.RequestPasswordReset(r.Context(), payload.Email, auth.ClientIPFromContext(r.Context()))
	if err != nil {
		writePlatformError(w, err, "发送密码重置验证码失败")
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (p *Platform) PasswordReset(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ResetToken  string `json:"resetToken"`
		Code        string `json:"code"`
		NewPassword string `json:"newPassword"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	if err := p.service.ResetPassword(r.Context(), payload.ResetToken, payload.Code, payload.NewPassword); err != nil {
		writePlatformError(w, err, "密码重置失败")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Platform) Session(w http.ResponseWriter, r *http.Request) {
	if account, ok := auth.AccountFromContext(r.Context()); ok {
		writeJSON(w, http.StatusOK, p.service.AccountSession(r.Context(), account))
		return
	}
	if guest, ok := auth.GuestFromContext(r.Context()); ok {
		writeJSON(w, http.StatusOK, p.service.GuestSession(r.Context(), guest))
		return
	}
	writeStatusError(w, http.StatusUnauthorized, "ACTOR_REQUIRED", "无法建立会话")
}

func (p *Platform) ClearGuest(w http.ResponseWriter, r *http.Request) {
	if !p.sameOrigin(r) {
		writeStatusError(w, http.StatusForbidden, "ORIGIN_MISMATCH", "请求来源无效")
		return
	}
	cookie, err := r.Cookie(guestCookieName)
	if err != nil || cookie.Value == "" {
		clearGuestCookie(w, p.secureCookies())
		writeStatusError(w, http.StatusUnauthorized, "GUEST_SESSION_INVALID", "访客会话无效")
		return
	}
	if err = p.service.ClearGuest(r.Context(), cookie.Value); err != nil {
		clearGuestCookie(w, p.secureCookies())
		writePlatformError(w, err, "清除访客数据失败")
		return
	}
	clearGuestCookie(w, p.secureCookies())
	w.WriteHeader(http.StatusNoContent)
}

func (p *Platform) ImportGuest(w http.ResponseWriter, r *http.Request) {
	if !p.sameOrigin(r) {
		writeStatusError(w, http.StatusForbidden, "ORIGIN_MISMATCH", "请求来源无效")
		return
	}
	account, ok := auth.AccountFromContext(r.Context())
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "需要有效的账号身份凭证")
		return
	}
	var payload struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	if !payload.Confirm {
		writeStatusError(w, http.StatusBadRequest, "CONFIRMATION_REQUIRED", "必须明确确认导入")
		return
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempotencyKey == "" {
		writeStatusError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "缺少 Idempotency-Key")
		return
	}
	cookie, err := r.Cookie(guestCookieName)
	if err != nil || cookie.Value == "" {
		clearGuestCookie(w, p.secureCookies())
		writeStatusError(w, http.StatusUnauthorized, "GUEST_SESSION_INVALID", "访客会话无效")
		return
	}
	result, err := p.service.ImportGuest(r.Context(), account.ID, cookie.Value, idempotencyKey)
	if err != nil {
		if errors.Is(err, service.ErrGuestUnavailable) || errors.Is(err, dao.ErrGuestUnavailable) {
			clearGuestCookie(w, p.secureCookies())
		}
		writePlatformError(w, err, "导入访客历史失败")
		return
	}
	clearGuestCookie(w, p.secureCookies())
	writeJSON(w, http.StatusOK, result)
}

func (p *Platform) Me(w http.ResponseWriter, r *http.Request) {
	account, ok := auth.AccountFromContext(r.Context())
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "需要有效的账号身份凭证")
		return
	}
	writeJSON(w, http.StatusOK, account)
}

func (p *Platform) UpdateMe(w http.ResponseWriter, r *http.Request) {
	account, ok := auth.AccountFromContext(r.Context())
	if !ok {
		writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "需要有效的账号身份凭证")
		return
	}
	var payload struct {
		Nickname string `json:"nickname"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	updated, err := p.service.UpdateNickname(r.Context(), account.Username, payload.Nickname)
	if err != nil {
		writePlatformError(w, err, "更新资料失败")
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (p *Platform) ListAccounts(w http.ResponseWriter, r *http.Request) {
	accounts, err := p.service.ListAccounts(r.Context(), queryInt(r, "limit", 100))
	if err != nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "账号列表暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, accounts)
}

func (p *Platform) SetAccountStatus(w http.ResponseWriter, r *http.Request) {
	if rejectUnmarked(w, r) {
		return
	}
	accountID, err := pathUint64(r, "accountID")
	if err != nil {
		writeStatusError(w, http.StatusBadRequest, "VALIDATION_ERROR", "账号 ID 无效")
		return
	}
	var payload struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &payload) {
		return
	}
	if err = p.service.SetAccountStatus(r.Context(), accountID, payload.Status); err != nil {
		writePlatformError(w, err, "更新账号状态失败")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (p *Platform) GuestMetrics(w http.ResponseWriter, r *http.Request) {
	metrics, err := p.service.GuestMetrics(r.Context())
	if err != nil {
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "访客指标暂时不可用")
		return
	}
	writeJSON(w, http.StatusOK, metrics)
}

func writePlatformError(w http.ResponseWriter, err error, fallback string) {
	var throttled *service.ThrottledError
	switch {
	case errors.As(err, &throttled):
		setRetryAfter(w, throttled.RetryAfter)
		writeStatusError(w, http.StatusTooManyRequests, "RATE_LIMITED", "尝试次数过多，请稍后重试")
	case errors.Is(err, service.ErrValidation):
		writeStatusError(w, http.StatusBadRequest, "VALIDATION_ERROR", "请求参数无效")
	case errors.Is(err, service.ErrEmailTaken):
		writeStatusError(w, http.StatusConflict, "EMAIL_ALREADY_REGISTERED", "邮箱已被注册")
	case errors.Is(err, service.ErrInvalidCredentials):
		writeStatusError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "邮箱或密码错误")
	case errors.Is(err, service.ErrInvalidToken):
		writeStatusError(w, http.StatusUnauthorized, "INVALID_TOKEN", "身份凭证无效")
	case errors.Is(err, service.ErrRegistrationClosed):
		writeStatusError(w, http.StatusForbidden, "REGISTRATION_CLOSED", "注册暂未开放")
	case errors.Is(err, service.ErrEmailDelivery):
		w.Header().Set("Retry-After", "60")
		writeStatusError(w, http.StatusServiceUnavailable, "EMAIL_DELIVERY_UNAVAILABLE", "邮件服务暂时不可用")
	case errors.Is(err, service.ErrDependencyUnavailable), errors.Is(err, verification.ErrUnavailable):
		w.Header().Set("Retry-After", "60")
		writeStatusError(w, http.StatusServiceUnavailable, "DEPENDENCY_UNAVAILABLE", "依赖服务暂时不可用")
	case errors.Is(err, service.ErrVerificationCodeInvalid), errors.Is(err, verification.ErrClaimed), errors.Is(err, verification.ErrStateConflict):
		writeStatusError(w, http.StatusBadRequest, "VERIFICATION_CODE_INVALID", "验证码无效")
	case errors.Is(err, service.ErrVerificationExpired):
		writeStatusError(w, http.StatusGone, "VERIFICATION_EXPIRED", "验证码已过期")
	case errors.Is(err, service.ErrVerificationAttemptsExhausted):
		w.Header().Set("Retry-After", "60")
		writeStatusError(w, http.StatusTooManyRequests, "VERIFICATION_ATTEMPTS_EXHAUSTED", "验证码尝试次数已耗尽")
	case errors.Is(err, service.ErrVerificationRateLimited), errors.Is(err, verification.ErrEmailRateLimited), errors.Is(err, verification.ErrIPRateLimited), errors.Is(err, verification.ErrGlobalRateLimited):
		w.Header().Set("Retry-After", "60")
		writeStatusError(w, http.StatusTooManyRequests, "RATE_LIMITED", "请求过于频繁，请稍后重试")
	case errors.Is(err, service.ErrGuestAccessDisabled):
		writeStatusError(w, http.StatusForbidden, "GUEST_ACCESS_DISABLED", "访客访问暂未开放")
	case errors.Is(err, service.ErrGuestImportDisabled):
		writeStatusError(w, http.StatusForbidden, "GUEST_IMPORT_DISABLED", "访客历史导入暂未开放")
	case errors.Is(err, service.ErrGuestUnavailable), errors.Is(err, dao.ErrGuestUnavailable):
		writeStatusError(w, http.StatusUnauthorized, "GUEST_SESSION_INVALID", "访客会话无效")
	case errors.Is(err, dao.ErrGuestDeepReadInProgress):
		writeStatusError(w, http.StatusConflict, "GUEST_DEEP_READ_IN_PROGRESS", "访客精读任务仍在处理中")
	case errors.Is(err, dao.ErrGuestDeepReadExhausted):
		writeStatusError(w, http.StatusForbidden, "GUEST_DEEP_READ_EXHAUSTED", "访客精读资格已使用")
	case errors.Is(err, dao.ErrNotFound):
		writeStatusError(w, http.StatusNotFound, "NOT_FOUND", "资源不存在")
	default:
		writeStatusError(w, http.StatusInternalServerError, "INTERNAL_ERROR", fallback)
	}
}

func (p *Platform) sameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if isTrusted(remoteIP(r.RemoteAddr), p.config.TrustedProxyCIDRs) {
		forwarded := strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")
		if value := strings.ToLower(strings.TrimSpace(forwarded[len(forwarded)-1])); value == "http" || value == "https" {
			scheme = value
		}
	}
	return strings.EqualFold(parsed.Scheme, scheme) && strings.EqualFold(parsed.Host, r.Host)
}

// decodeJSON reads one size-limited JSON value and rejects trailing values.
func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			writeStatusError(w, http.StatusBadRequest, "INVALID_REQUEST", "请求包含未知字段")
		} else {
			writeStatusError(w, http.StatusBadRequest, "INVALID_JSON", "请求 JSON 无效")
		}
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeStatusError(w, http.StatusBadRequest, "INVALID_JSON", "请求 JSON 无效")
		return false
	}
	return true
}
