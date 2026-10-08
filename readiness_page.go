package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type readinessPageData struct {
	Language string
	CSPNonce string
}

// handleReadiness serves the read-only, single-page readiness overview. Status
// is fetched from its separate same-origin GET endpoint by the page script.
func (a *app) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/readiness" {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}

	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		http.Error(w, "Could not render readiness.", http.StatusInternalServerError)
		return
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'nonce-"+nonce+"'"))
	if err := page.ExecuteTemplate(w, "readiness.html", readinessPageData{
		Language: string(uiLocaleForRequest(r)),
		CSPNonce: nonce,
	}); err != nil {
		// Template diagnostics are intentionally not returned to the local client.
		return
	}
}
