package main

import "net/http"

func newLocalUIHandler(a *app) http.Handler {
	return newLocalUIHandlerWithResidentObserver(a, nil)
}

// newLocalUIHandlerWithResidentObserver keeps the normal test handler isolated
// from the fixed resident endpoint while allowing production to expose the
// same bounded observation on readiness and runtime-status routes.
func newLocalUIHandlerWithResidentObserver(a *app, observer ResidentEndpointObservationProvider) http.Handler {
	mux := http.NewServeMux()
	runtimeClientsHandler := newRuntimeClientsUIHandlerWithResidentObserver(
		a.runtime,
		a.clientRegistration,
		a.csrf,
		a.host,
		a.executable,
		clientRegistrationUIErrorCode,
		observer,
	)
	readinessHandler := newReadinessStatusHandler(a.configPath, a.clientRegistration, observer)
	mux.Handle(readinessStatusPath, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !mcpHTTPOriginAllowed(r, "http://"+a.host) {
			http.Error(w, "Request rejected.", http.StatusForbidden)
			return
		}
		readinessHandler.ServeHTTP(w, r)
	}))
	for _, route := range []string{
		runtimeClientsUIStatusPath,
		runtimeClientsUIPlanPath,
		runtimeClientsUIBackupPath,
		runtimeClientsUIApplyPath,
		runtimeClientsUIVerifyPath,
		runtimeClientsUIRestorePath,
	} {
		mux.Handle(route, runtimeClientsHandler)
	}
	mux.HandleFunc("/readiness", a.handleReadiness)
	mux.HandleFunc("/", a.handleRoot)
	mux.HandleFunc("/set-login-startup", a.handleLoginStartup)
	namedSecretHandler := newNamedSecretUIHandler(a.host, a.csrf, a.namedSecrets)
	mux.Handle(uiRouteSaveNamedSecret, namedSecretHandler)
	mux.Handle(uiRouteDeleteNamedSecret, namedSecretHandler)
	mux.Handle(uiRouteRetryNamedSecretCleanup, namedSecretHandler)
	mux.Handle(uiRouteSaveUserEnvironment, newUserEnvironmentUIHandler(a.host, a.csrf, a.userEnvironment))
	mux.Handle(uiRouteDeleteUserEnvironment, newUserEnvironmentUIHandler(a.host, a.csrf, a.userEnvironment))
	if a.ssh != nil && a.setupDrafts != nil {
		sshHandler := newSSHSetupUIHandler(a.host, a.csrf, a.ssh, a.setupDrafts)
		for _, route := range []string{
			"/ssh-settings",
			"/save-ssh-target",
			"/delete-ssh-target",
			"/save-ssh-operation",
			"/delete-ssh-operation",
			"/review-setup-draft",
			"/approve-setup-draft",
			"/reject-setup-draft",
		} {
			mux.Handle(route, sshHandler)
		}
	}
	mux.HandleFunc("/save", a.handleSave)
	mux.HandleFunc("/test", a.handleTest)
	mux.HandleFunc("/save-jenkins", a.handleJenkinsSave)
	mux.HandleFunc("/test-jenkins", a.handleJenkinsTest)
	mux.HandleFunc("/save-harbor", a.handleHarborSave)
	mux.HandleFunc("/test-harbor", a.handleHarborTest)
	mux.HandleFunc("/save-dashboard", a.handleDashboardSave)
	mux.HandleFunc("/test-dashboard", a.handleDashboardTest)
	mux.HandleFunc("/toggle-target", a.handleToggleTarget)
	mux.HandleFunc("/delete-target", a.handleDeleteTarget)
	mux.HandleFunc("/save-bundle", a.handleServiceBundleSave)
	mux.HandleFunc("/delete-bundle", a.handleServiceBundleDelete)
	mux.HandleFunc("/save-python-task", a.handlePythonTaskSave)
	mux.HandleFunc("/dashboard-diagnosis", a.handleDashboardDiagnosis)
	mux.HandleFunc("/dashboard-diagnosis-logs", a.handleDashboardDiagnosisLogs)
	mux.HandleFunc("/preview-git-remotes", a.handleGitRemotePreview)
	mux.HandleFunc("/preview-runbook", a.handleRunbookPreview)
	mux.HandleFunc("/preview-ssh-config", a.handleSSHConfigPreview)
	mux.HandleFunc("/preview-settings-json", a.handleSettingsImportPreview)
	mux.HandleFunc("/preview-json-remotes", a.handleJSONGitRemotePreview)
	return a.securityHeaders(mux)
}
