package controller

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"paperassistant/internal/config"
	"paperassistant/internal/dao"
	"paperassistant/internal/service"
)

// adminRequestMarker is a header the browser cannot attach cross-origin without a preflight,
// which keeps state-changing admin endpoints from being triggered by a plain form post.
const adminRequestMarker = "paper-assistant-admin"

func rejectUnmarked(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Requested-With") != adminRequestMarker {
		writeError(w, http.StatusForbidden, "missing admin request marker")
		return true
	}
	return false
}

type Admin struct {
	service *service.Admin
	config  config.Config
}

func NewAdmin(value *service.Admin, cfg config.Config) *Admin {
	return &Admin{service: value, config: cfg}
}

func (a *Admin) Overview(w http.ResponseWriter, r *http.Request) {
	days := queryInt(r, "days", 7)
	value, err := a.service.Overview(r.Context(), days)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (a *Admin) Users(w http.ResponseWriter, r *http.Request) {
	value, err := a.service.Users(r.Context(), queryInt(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (a *Admin) Conversations(w http.ResponseWriter, r *http.Request) {
	userID, err := pathUint64(r, "userID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	value, err := a.service.Conversations(r.Context(), userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (a *Admin) Messages(w http.ResponseWriter, r *http.Request) {
	conversationID, err := pathUint64(r, "conversationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	userID, err := strconv.ParseUint(r.URL.Query().Get("userId"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "userId query parameter is required")
		return
	}
	value, err := a.service.Messages(r.Context(), userID, conversationID, queryInt(r, "limit", 100))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func (a *Admin) RestoreConversation(w http.ResponseWriter, r *http.Request) {
	if rejectUnmarked(w, r) {
		return
	}
	userID, err := pathUint64(r, "userID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return
	}
	conversationID, err := pathUint64(r, "conversationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid conversation id")
		return
	}
	if err = a.service.RestoreConversation(r.Context(), userID, conversationID); err != nil {
		if errors.Is(err, dao.ErrNotFound) {
			writeError(w, http.StatusNotFound, "conversation not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *Admin) RuntimeControls(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.runtimeControls())
}

// UpdateRuntimeControls toggles the global emergency stop. It is the only runtime state the
// admin surface may write; behaviour parameters stay in environment variables so that what a
// given deployment ran with remains auditable. See ADR 0011.
func (a *Admin) UpdateRuntimeControls(w http.ResponseWriter, r *http.Request) {
	if rejectUnmarked(w, r) {
		return
	}
	var payload struct {
		EmergencyStop *bool  `json:"emergencyStop"`
		Reason        string `json:"reason"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if payload.EmergencyStop == nil {
		writeError(w, http.StatusBadRequest, "emergencyStop is required")
		return
	}
	if *payload.EmergencyStop {
		reason := strings.TrimSpace(payload.Reason)
		if len([]rune(reason)) > 200 {
			reason = string([]rune(reason)[:200])
		}
		state := a.service.EngageEmergencyStop(reason)
		slog.Warn("emergency stop engaged", "engaged_at", state.EngagedAt, "reason", state.Reason)
	} else {
		a.service.ReleaseEmergencyStop()
		slog.Warn("emergency stop released")
	}
	writeJSON(w, http.StatusOK, a.runtimeControls())
}

func (a *Admin) runtimeControls() map[string]any {
	state := a.service.EmergencyStopState()
	if state == nil {
		return map[string]any{"emergencyStop": false}
	}
	return map[string]any{"emergencyStop": true, "engagedAt": state.EngagedAt, "reason": state.Reason}
}

func (a *Admin) RuntimeConfig(w http.ResponseWriter, _ *http.Request) {
	limits := service.Limits()
	writeJSON(w, http.StatusOK, map[string]any{
		"environment":             a.config.Environment,
		"qqEnabled":               a.config.QQ.Enabled,
		"qqApiBase":               a.config.QQ.APIBase,
		"qqIntents":               a.config.QQ.Intents,
		"paperScraperUrl":         a.config.PaperScraper.BaseURL,
		"paperScraperResultLimit": a.config.PaperScraper.ResultLimit,
		"deepSeekConfigured":      a.config.DeepSeek.APIKey != "",
		"deepSeekModel":           a.config.DeepSeek.Model,
		"fullTextDirectory":       a.config.FullText.Directory,
		"maxSearchPapers":         limits.MaxSearchPapers,
		"toolCallsPerTurn":        limits.ToolCallsPerTurn,
		"taskPersistence":         "memory",
		"emergencyStopPersisted":  false,
		"jwtConfigured":           a.config.Platform.JWTSecret != "",
		"redisConfigured":         a.config.Redis.Addr != "",
		"smtpMode":                a.config.SMTP.Mode,
		"smtpConfigured":          a.config.SMTP.Mode == "smtp" && a.config.SMTP.Host != "" && a.config.SMTP.From != "" && a.config.SMTP.AuthCode != "",
		"features": map[string]bool{
			"openRegistration":  a.config.Platform.OpenRegistrationEnabled,
			"guestAccess":       a.config.Platform.GuestAccessEnabled,
			"guestDeepRead":     a.config.Platform.GuestDeepReadEnabled,
			"guestImport":       a.config.Platform.GuestImportEnabled,
			"emailVerification": a.config.Platform.EmailVerificationRequired,
			"costBreaker":       a.config.Platform.CostBreakerEnabled,
		},
		"costThresholds": map[string]float64{
			"hourlyUSD": a.config.Platform.CostHourlyUSD,
			"dailyUSD":  a.config.Platform.CostDailyUSD,
		},
	})
}
