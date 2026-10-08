//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const manualWindowsDashboardFailureBrowserQAGate = "LAH_MANUAL_WINDOWS_DASHBOARD_FAILURE_BROWSER_QA"

func TestManualWindowsDashboardDiagnosisFailureBrowserQA(t *testing.T) {
	if os.Getenv(manualWindowsDashboardFailureBrowserQAGate) != "1" {
		t.Skipf("set %s=1 to start the loopback browser QA harness", manualWindowsDashboardFailureBrowserQAGate)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := serviceBundleConfig()
	second := cfg.ServiceBundles[0].Environments[0]
	second.Name = "qa-green"
	second.DashboardDeployment = "api-green"
	cfg.ServiceBundles[0].Environments = append(cfg.ServiceBundles[0].Environments, second)
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host := listener.Addr().String()
	baseURL := "http://" + host
	completionBytes := make([]byte, 32)
	if _, err := rand.Read(completionBytes); err != nil {
		listener.Close()
		t.Fatal("generate a browser QA completion signal")
	}
	completionToken := hex.EncodeToString(completionBytes)
	clear(completionBytes)

	a := &app{configPath: configPath, host: host, csrf: "synthetic-browser-qa-csrf"}
	productHandler := newLocalUIHandler(a)
	var rootGets atomic.Int32
	var blockedUnexpectedRequests atomic.Int32
	completed := make(chan struct{}, 1)
	var completionCount atomic.Int32

	mux := http.NewServeMux()
	mux.HandleFunc("/__test__/complete", func(w http.ResponseWriter, r *http.Request) {
		origins := r.Header.Values("Origin")
		if r.Method != http.MethodPost || r.Host != host || len(origins) != 1 || origins[0] != baseURL || r.URL.RawQuery != "" {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}
		mediaType, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if parseErr != nil || !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
			http.Error(w, "Request rejected.", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1024)
		if err := r.ParseForm(); err != nil || len(r.PostForm) != 1 || len(r.PostForm["qa_signal"]) != 1 || r.PostForm.Get("qa_signal") != completionToken {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}
		if completionCount.Add(1) != 1 {
			http.Error(w, "QA completion was already received.", http.StatusConflict)
			return
		}
		select {
		case completed <- struct{}{}:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "QA completion was already received.", http.StatusConflict)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			blockedUnexpectedRequests.Add(1)
			http.Error(w, "Only the local settings page is enabled in this browser QA harness.", http.StatusConflict)
			return
		}
		rootGets.Add(1)
		productHandler.ServeHTTP(w, r)
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
	})

	t.Logf("Open the synthetic Dashboard failure QA page at %s", baseURL)
	t.Logf("After checking both locales, scope reset, and stale-response behavior, signal completion with %s", completionToken)
	select {
	case <-completed:
	case err := <-serveErrors:
		t.Fatalf("manual browser QA server stopped before completion: %v", err)
	case <-time.After(15 * time.Minute):
		t.Fatal("timed out waiting for browser QA completion")
	}
	if rootGets.Load() != 1 || completionCount.Load() != 1 || blockedUnexpectedRequests.Load() != 0 {
		t.Fatalf("browser QA request counts: root GET=%d completion=%d unexpected app requests=%d, want 1/1/0",
			rootGets.Load(), completionCount.Load(), blockedUnexpectedRequests.Load())
	}
}
