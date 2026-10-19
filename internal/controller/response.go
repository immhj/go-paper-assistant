package controller

import (
	"encoding/json"
	"net/http"
	"strconv"
)

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message, "status": status})
}

// writeStatusError is used by the platform API. Both fields carry the stable string so newer
// clients may use code while the existing status contract remains intact.
func writeStatusError(w http.ResponseWriter, httpStatus int, code, message string) {
	writeJSON(w, httpStatus, map[string]any{"error": message, "status": code, "code": code})
}

func queryInt(r *http.Request, key string, fallback int) int {
	value, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return fallback
	}
	return value
}

func pathUint64(r *http.Request, key string) (uint64, error) {
	return strconv.ParseUint(r.PathValue(key), 10, 64)
}
