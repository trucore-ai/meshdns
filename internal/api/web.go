package api

import (
	"net/http"

	webassets "github.com/trucore-ai/meshdns/internal/web"
)

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	html, err := webassets.HTML()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to load landing page")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(html)
}
