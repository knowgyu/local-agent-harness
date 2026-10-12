package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type userGuidePageData struct {
	Language string
	CSPNonce string
}

// handleUserGuide serves the static, read-only in-app guide.
func (a *app) handleUserGuide(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/guide" {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}

	var nonceBytes [32]byte
	if _, err := rand.Read(nonceBytes[:]); err != nil {
		http.Error(w, "Could not render guide.", http.StatusInternalServerError)
		return
	}
	nonce := hex.EncodeToString(nonceBytes[:])
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'nonce-"+nonce+"'"))
	if err := page.ExecuteTemplate(w, "guide.html", userGuidePageData{
		Language: string(uiLocaleForRequest(r)),
		CSPNonce: nonce,
	}); err != nil {
		// Template diagnostics are intentionally not returned to the local client.
		return
	}
}
