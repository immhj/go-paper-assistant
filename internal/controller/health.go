package controller

import (
	"net/http"

	"paperassistant/internal/service"
)

type Health struct{ admin *service.Admin }

func NewHealth(admin *service.Admin) *Health { return &Health{admin: admin} }
func (h *Health) Get(w http.ResponseWriter, r *http.Request) {
	if err := h.admin.Health(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
