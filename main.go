package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	legacyConfigVersion = 1
	configVersion       = 9
	maxConfigSize       = 1 << 20
	maxFormSize         = 12 << 10
	maxGitConfigSize    = 64 << 10
	maxGitImportRequest = maxGitConfigSize + 8<<10
	maxAPIBytes         = 1 << 20
	maxSecretSize       = 4096
)

const localUIReferrerPolicy = "same-origin"

//go:embed ui.html ui_fragments/*.html
var uiFiles embed.FS

var page = mustParseUIPage(uiFiles)

type appLaunchMode uint8

const (
	appLaunchUI appLaunchMode = iota
	appLaunchMCP
	appLaunchResident
	appLaunchResidentBackground
)

func parseAppLaunchMode(args []string) (appLaunchMode, error) {
	switch {
	case len(args) == 0:
		return appLaunchUI, nil
	case len(args) == 1 && args[0] == "--mcp":
		return appLaunchMCP, nil
	case len(args) == 1 && args[0] == "--resident":
		return appLaunchResident, nil
	case len(args) == 2 && args[0] == "--resident" && args[1] == "--background":
		return appLaunchResidentBackground, nil
	default:
		return appLaunchUI, errors.New("invalid command-line arguments")
	}
}

var (
	githubTokenPattern     = regexp.MustCompile(`(?i)\b(?:gh[pousr]_[A-Za-z0-9_]{16,}|github_pat_[A-Za-z0-9_]{16,})\b`)
	credentialHeader       = regexp.MustCompile(`(?i)\bauthorization\b(\s*[:=]\s*)(?:bearer|basic)\s+[^\s,;]+`)
	credentialPattern      = regexp.MustCompile(`(?i)(["']?)([a-z0-9_-]*(?:token|password|secret|private[_-]?key|authorization|api[_-]?key))(["']?)(\s*[:=]\s*)("(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,"';}]+)`)
	privateKeyPEM          = regexp.MustCompile(`(?i)-----BEGIN [A-Z0-9 _-]*PRIVATE KEY(?: BLOCK)?-----`)
	secretRefPattern       = regexp.MustCompile(`^cred:[a-f0-9]{32}$`)
	githubIDPattern        = regexp.MustCompile(`^github:[a-f0-9]{32}$`)
	jenkinsIDPattern       = regexp.MustCompile(`^jenkins:[a-f0-9]{32}$`)
	harborIDPattern        = regexp.MustCompile(`^harbor:[a-f0-9]{32}$`)
	dashboardIDPattern     = regexp.MustCompile(`^dashboard:[a-f0-9]{32}$`)
	serviceBundleIDPattern = regexp.MustCompile(`^service:[a-f0-9]{32}$`)
)

type target struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Origin     string `json:"origin"`
	Repository string `json:"repository"`
	SecretRef  string `json:"secret_ref"`
	Disabled   bool   `json:"disabled,omitempty"`
}

type config struct {
	ConnectionTests  map[string]connectionTest `json:"connection_tests,omitempty"`
	Version          int                       `json:"version"`
	NamedSecrets     []namedSecretMetadata     `json:"named_secrets,omitempty"`
	GitHubTargets    []target                  `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget           `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget            `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget         `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle           `json:"service_bundles,omitempty"`
	PythonTasks      []pythonTask              `json:"python_tasks,omitempty"`
	SSHTargets       []sshTargetDefinition     `json:"ssh_targets,omitempty"`
	Target           *target                   `json:"-"`
	Jenkins          *jenkinsTarget            `json:"-"`
}

type connectionTest struct {
	Result      string `json:"result"`
	CompletedAt string `json:"completed_at,omitempty"`
}

type connectionFailureKind string

const (
	connectionFailureAddress        connectionFailureKind = "address"
	connectionFailureTimeout        connectionFailureKind = "timeout"
	connectionFailureAuthentication connectionFailureKind = "authentication"
	connectionFailureAccess         connectionFailureKind = "access"
	connectionFailureEndpoint       connectionFailureKind = "endpoint"
	connectionFailureRateLimit      connectionFailureKind = "rate_limit"
	connectionFailureOther          connectionFailureKind = "other"
)

type connectionDiagnosticError struct {
	kind connectionFailureKind
	err  error
}

func (e *connectionDiagnosticError) Error() string { return e.err.Error() }
func (e *connectionDiagnosticError) Unwrap() error { return e.err }

func withConnectionDiagnostic(kind connectionFailureKind, err error) error {
	if err == nil {
		return nil
	}
	return &connectionDiagnosticError{kind: kind, err: err}
}

func connectionTransportFailure(err error, addressMessage string) error {
	kind := connectionFailureAddress
	var networkError net.Error
	contextTimedOut := errors.Is(err, context.DeadlineExceeded)
	networkTimedOut := errors.As(err, &networkError) && networkError.Timeout()
	if contextTimedOut || networkTimedOut {
		kind = connectionFailureTimeout
	}
	return withConnectionDiagnostic(kind, errors.New(addressMessage))
}

func connectionFailureFor(err error) connectionFailureKind {
	if err == nil {
		return ""
	}
	var diagnostic *connectionDiagnosticError
	if errors.As(err, &diagnostic) {
		return diagnostic.kind
	}
	return connectionFailureOther
}

type connectionTestAttempt struct {
	attempted    bool
	succeeded    bool
	historySaved bool
	completedAt  string
	failureKind  connectionFailureKind
}

func connectionTestFor(cfg config, targetID string) *connectionTest {
	test, ok := cfg.ConnectionTests[targetID]
	if !ok || !validConnectionTest(test) {
		return nil
	}
	return &test
}

func catalogConnectionTestFor(cfg config, targetID string) *connectionTest {
	if test := connectionTestFor(cfg, targetID); test != nil {
		return test
	}
	return &connectionTest{Result: "not_tested"}
}

func validConnectionTest(test connectionTest) bool {
	if test.Result != "success" && test.Result != "failure" || !strings.HasSuffix(test.CompletedAt, "Z") {
		return false
	}
	completedAt, err := time.Parse(time.RFC3339Nano, test.CompletedAt)
	return err == nil && completedAt.Location() == time.UTC
}

func clearConnectionTest(cfg *config, targetID string) {
	delete(cfg.ConnectionTests, targetID)
	if len(cfg.ConnectionTests) == 0 {
		cfg.ConnectionTests = nil
	}
}

type legacyConfig struct {
	Version int            `json:"version"`
	Target  *target        `json:"target,omitempty"`
	Jenkins *jenkinsTarget `json:"jenkins,omitempty"`
}

type targetChoice struct {
	ID       string
	Name     string
	Disabled bool
	Selected bool
}

type githubTargetInput struct {
	Target string `json:"target"`
}

type githubPullRequestInput struct {
	Target string `json:"target"`
	Number int    `json:"number"`
}

type registeredTarget struct {
	ConnectionTest *connectionTest `json:"connection_test,omitempty"`
	Type           string          `json:"type"`
	Name           string          `json:"name"`
	Actions        []string        `json:"actions"`
}

type registeredTargetsResult struct {
	Targets []registeredTarget `json:"targets"`
}

type registeredServiceTarget struct {
	ConnectionTest *connectionTest `json:"connection_test,omitempty"`
	Name           string          `json:"name"`
	Actions        []string        `json:"actions"`
}

type registeredServiceEnvironment struct {
	Name      string                   `json:"name"`
	Jenkins   *registeredServiceTarget `json:"jenkins,omitempty"`
	Harbor    *registeredServiceTarget `json:"harbor,omitempty"`
	Dashboard *registeredServiceTarget `json:"dashboard,omitempty"`
}

type registeredServiceBundle struct {
	Name         string                         `json:"name"`
	Repositories []registeredServiceTarget      `json:"repositories"`
	Environments []registeredServiceEnvironment `json:"environments"`
}

type registeredServiceBundlesResult struct {
	Bundles []registeredServiceBundle `json:"bundles"`
}

type repository struct {
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Description   string `json:"description,omitempty"`
}

type githubPullRequest struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Draft     bool      `json:"draft"`
	Merged    bool      `json:"merged"`
	BaseRef   string    `json:"base_ref"`
	HeadRef   string    `json:"head_ref"`
	HeadSHA   string    `json:"head_sha"`
	UpdatedAt time.Time `json:"updated_at"`
}

type secretStore interface {
	Save(ref string, value []byte) error
	Load(ref string) ([]byte, error)
	Delete(ref string) error
}

type app struct {
	configPath            string
	secrets               secretStore
	namedSecrets          NamedSecretController
	userEnvironment       UserEnvironmentController
	ssh                   *sshOperationController
	setupDrafts           SetupDraftController
	clientRegistration    MCPClientRegistrationController
	runtime               MCPRuntimeController
	executable            string
	client                *http.Client
	host                  string
	csrf                  string
	openApprovalBrowser   func(string) error
	jenkinsApprovalResult func(context.Context) error
	pythonTaskCommand     pythonTaskCommandFactory
	pythonTaskStarter     pythonTaskProcessStarter
	pythonTaskSanitizer   pythonTaskOutputSanitizer
	loginStartup          loginStartupControl

	statusMu sync.RWMutex
	status   string
	isError  bool
}

type pageData struct {
	Language                    string
	GitHubTest                  *connectionTest
	JenkinsTest                 *connectionTest
	HarborTest                  *connectionTest
	DashboardTest               *connectionTest
	NamedSecretsJSON            string
	NamedSecretCleanupJSON      string
	UserEnvironmentJSON         string
	SecretsEnvironmentAvailable bool
	GitHubChoices               []targetChoice
	GitHubID                    string
	GitHubSecretID              string
	Name                        string
	Origin                      string
	Repository                  string
	SecretSaved                 bool
	JenkinsName                 string
	JenkinsChoices              []targetChoice
	JenkinsID                   string
	JenkinsSecretID             string
	GitHubDisabled              bool
	JenkinsDisabled             bool
	JenkinsURL                  string
	JenkinsUser                 string
	JenkinsJob                  string
	JenkinsEnv                  string
	JenkinsSecret               bool
	JenkinsRun                  bool
	HarborChoices               []targetChoice
	HarborID                    string
	HarborSecretID              string
	HarborName                  string
	HarborURL                   string
	HarborUser                  string
	HarborProject               string
	HarborRepository            string
	HarborSecret                bool
	HarborDisabled              bool
	HarborErrorField            string
	HarborFieldError            string
	HarborFieldErrorEN          string
	HarborFieldErrorKO          string
	DashboardChoices            []targetChoice
	DashboardID                 string
	DashboardSecretID           string
	DashboardName               string
	DashboardURL                string
	DashboardSecret             bool
	DashboardDisabled           bool
	DashboardErrorField         string
	DashboardFieldError         string
	DashboardFieldErrorEN       string
	DashboardFieldErrorKO       string
	ServiceBundleChoices        []serviceBundleChoice
	ServiceBundleID             string
	ServiceBundleName           string
	PythonTaskChoices           []pythonTaskChoice
	PythonTaskID                string
	PythonTaskName              string
	PythonInterpreterPath       string
	PythonScriptPath            string
	PythonTaskSecretEnvName     string
	PythonTaskSecretSaved       bool
	PythonTaskDisabled          bool
	ServiceBundleGitHubChoices  []targetChoice
	ServiceBundleEnvironments   []serviceEnvironmentPage
	DashboardDiagnosisChoices   []dashboardDiagnosisPageChoice
	DisabledDashboardMappings   bool
	LoginStartupAvailable       bool
	LoginStartupState           string
	StorageReady                bool
	CSRF                        string
	CSPNonce                    string
	Status                      string
	StatusEN                    string
	StatusKO                    string
	StatusIsError               bool
}

type harborSaveDraft struct {
	TargetID   string
	Name       string
	BaseURL    string
	Username   string
	Project    string
	Repository string
	ErrorField string
}

type dashboardSaveDraft struct {
	TargetID   string
	Name       string
	BaseURL    string
	ErrorField string
}

type savePageOverride struct {
	Status        string
	Harbor        *harborSaveDraft
	Dashboard     *dashboardSaveDraft
	StatusIsError bool
}

type saveFieldError struct {
	field   string
	message string
}

func (e *saveFieldError) Error() string { return e.message }

type serviceBundleChoice struct {
	ID   string
	Name string
}

type serviceEnvironmentPage struct {
	Name                string
	JenkinsID           string
	HarborID            string
	DashboardID         string
	DashboardNamespace  string
	DashboardDeployment string
	JenkinsChoices      []targetChoice
	HarborChoices       []targetChoice
	DashboardChoices    []targetChoice
}

func main() {
	mode, err := parseAppLaunchMode(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "Usage: local-agent-harness [--mcp|--resident [--background]]")
		os.Exit(2)
	}
	if mode == appLaunchMCP {
		if err := runMCP(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if mode == appLaunchResident || mode == appLaunchResidentBackground {
		if mode == appLaunchResidentBackground {
			hideBackgroundConsole()
		}
		if err := runResidentMCPEntrypoint(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := runUI(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runMCP() error {
	path, err := configFilePath()
	if err != nil {
		return errors.New("Could not locate the local settings file.")
	}
	if err := migrateConfig(path); err != nil {
		return errors.New("Could not migrate local settings. Previous settings remain in place.")
	}
	if !credentialStoreAvailable() {
		return errors.New("MCP mode requires Windows Credential Manager; secret storage is unavailable on this platform.")
	}
	a := &app{configPath: path, secrets: systemSecretStore{}, client: newGitHubClient()}
	if err := a.initializeRuntimeControllers(); err != nil {
		return errors.New("Could not initialize local registration and SSH settings.")
	}
	server := a.mcpServer()
	return server.Run(context.Background(), &mcp.StdioTransport{MaxLineLength: 1 << 20})
}

func runUI() error {
	path, err := configFilePath()
	if err != nil {
		return errors.New("Could not locate the local settings folder.")
	}
	if err := migrateConfig(path); err != nil {
		return errors.New("Could not migrate local settings. Previous settings remain in place.")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return errors.New("Could not start the local settings server.")
	}
	defer listener.Close()
	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(csrfBytes); err != nil {
		return errors.New("Could not initialize the local settings server.")
	}
	addr := listener.Addr().(*net.TCPAddr)
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(addr.Port))
	baseURL := "http://" + host
	secrets := systemSecretStore{}
	a := &app{
		configPath:      path,
		secrets:         secrets,
		namedSecrets:    newNamedSecretController(path, secrets),
		userEnvironment: newDefaultUserEnvironmentController(),
		client:          newGitHubClient(),
		host:            host,
		csrf:            hex.EncodeToString(csrfBytes),
		loginStartup:    newSystemLoginStartupController(),
	}
	if err := a.initializeRuntimeControllers(); err != nil {
		return errors.New("Could not initialize local registration and SSH settings.")
	}
	server := &http.Server{
		Handler:           newLocalUIHandlerWithResidentObserver(a, newResidentEndpointObserver()),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	defer server.Close()
	fmt.Fprintln(os.Stderr, "Local settings: "+baseURL)
	if err := openBrowser(baseURL); err != nil {
		fmt.Fprintln(os.Stderr, "Open this address in your browser: "+baseURL)
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.New("The local settings server stopped unexpectedly.")
	}
	return nil
}

func (a *app) initializeRuntimeControllers() error {
	if a == nil || a.configPath == "" {
		return errors.New("runtime settings are unavailable")
	}
	var queue setupDraftQueueStore
	if a.setupDrafts == nil {
		var err error
		queue, err = newSetupDraftQueueStore()
		if err != nil {
			return errors.New("runtime settings are unavailable")
		}
	}
	registration := a.clientRegistration
	if registration == nil {
		var err error
		registration, err = newProductionClientRegistrationController()
		if err != nil {
			return errors.New("runtime settings are unavailable")
		}
	}
	executable := a.executable
	if executable == "" {
		var err error
		executable, err = os.Executable()
		if err != nil {
			return errors.New("runtime settings are unavailable")
		}
		executable, err = filepath.Abs(executable)
		if err != nil {
			return errors.New("runtime settings are unavailable")
		}
	}
	return a.initializeRuntimeControllersWith(queue, registration, executable)
}

// initializeRuntimeControllersWith keeps test construction on temp stores and
// injected adapters while sharing the production app wiring.
func (a *app) initializeRuntimeControllersWith(
	queue setupDraftQueueStore,
	registration MCPClientRegistrationController,
	executable string,
) error {
	if a == nil || a.configPath == "" || !filepath.IsAbs(executable) {
		return errors.New("runtime settings are unavailable")
	}
	if a.ssh == nil {
		a.ssh = newSSHOperationController(
			newSSHConfigStore(a.configPath),
			newOpenSSHCommandRunner(),
			&loopbackSSHOperationApprover{open: openBrowser},
		)
	}
	if a.setupDrafts == nil {
		if queue == nil {
			return errors.New("runtime settings are unavailable")
		}
		a.setupDrafts = newSetupDraftControllerWithQueue(newSetupDraftConfigStore(a.configPath), queue)
	}
	if a.clientRegistration == nil {
		if registration == nil {
			return errors.New("runtime settings are unavailable")
		}
		a.clientRegistration = registration
	}
	if a.executable == "" {
		a.executable = executable
	}
	return nil
}

func (a *app) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'none'"))
		w.Header().Set("Referrer-Policy", localUIReferrerPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != a.host {
			http.Error(w, localizeUIMessage(uiLocaleForRequest(r), "Not found"), http.StatusNotFound)
			return
		}
		if r.Method == http.MethodPost {
			origins := r.Header.Values("Origin")
			if len(origins) > 0 && (len(origins) != 1 || origins[0] != "http://"+a.host) {
				http.Error(w, localizeUIMessage(uiLocaleForRequest(r), "Request origin rejected"), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func contentSecurityPolicy(scriptSource string) string {
	return "default-src 'none'; script-src " + scriptSource + "; style-src 'self' 'unsafe-inline'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"
}

func (a *app) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	a.renderRootPage(w, r, nil, http.StatusOK)
}

func (a *app) renderRootPage(w http.ResponseWriter, r *http.Request, override *savePageOverride, statusCode int) {
	locale := uiLocaleForRequest(r)
	nonceBytes := make([]byte, 32)
	if _, err := rand.Read(nonceBytes); err != nil {
		http.Error(w, localizeUIMessage(locale, "Could not render settings."), http.StatusInternalServerError)
		return
	}
	nonce := hex.EncodeToString(nonceBytes)
	w.Header().Set("Content-Security-Policy", contentSecurityPolicy("'nonce-"+nonce+"'"))
	data := pageData{
		Language: string(locale), StorageReady: credentialStoreAvailable(), CSRF: a.csrf, CSPNonce: nonce,
		NamedSecretsJSON: "[]", NamedSecretCleanupJSON: `{"status":"unavailable","pending_ids":[]}`, UserEnvironmentJSON: "[]",
	}
	if a.loginStartup != nil {
		data.LoginStartupState = string(a.loginStartup.State())
		data.LoginStartupAvailable = data.LoginStartupState != string(loginStartupUnavailable)
	}
	cfg, err := readConfig(a.configPath)
	if err != nil {
		data.Status, data.StatusIsError = "Could not read local settings. Check the settings file and restart the app.", true
	} else {
		secretViews := namedSecretViews(cfg)
		if dataJSON, jsonErr := json.Marshal(secretViews); jsonErr == nil {
			data.NamedSecretsJSON = string(dataJSON)
		}
		if dataJSON, jsonErr := json.Marshal(currentNamedSecretCleanupUIState(r.Context(), a.namedSecrets, secretViews)); jsonErr == nil {
			data.NamedSecretCleanupJSON = string(dataJSON)
		}
		if data.StorageReady && a.namedSecrets != nil && a.userEnvironment != nil {
			if entries, listErr := a.userEnvironment.ListUserEnvironment(r.Context()); listErr == nil {
				if dataJSON, jsonErr := json.Marshal(entries); jsonErr == nil {
					data.UserEnvironmentJSON = string(dataJSON)
					data.SecretsEnvironmentAvailable = true
				}
			}
		}
		for _, t := range cfg.GitHubTargets {
			data.GitHubChoices = append(data.GitHubChoices, targetChoice{ID: t.ID, Name: t.Name, Disabled: t.Disabled})
		}
		for _, t := range cfg.JenkinsTargets {
			data.JenkinsChoices = append(data.JenkinsChoices, targetChoice{ID: t.ID, Name: t.Name, Disabled: t.Disabled})
		}
		for _, t := range cfg.HarborTargets {
			data.HarborChoices = append(data.HarborChoices, targetChoice{ID: t.ID, Name: t.Name, Disabled: t.Disabled})
		}
		githubID := r.URL.Query().Get("github_id")
		if githubID == "" && len(cfg.GitHubTargets) != 1 {
			githubID = "new"
		}
		data.GitHubID = githubID
		if t := selectGitHubTarget(cfg, githubID); t != nil {
			data.GitHubID = t.ID
			data.Name, data.Origin, data.Repository = t.Name, t.Origin, t.Repository
			data.SecretSaved, data.GitHubDisabled = secretRefPattern.MatchString(t.SecretRef), t.Disabled
			data.GitHubSecretID = namedSecretIDForReference(cfg, t.SecretRef)
			data.GitHubTest = connectionTestFor(cfg, t.ID)
		}
		jenkinsID := r.URL.Query().Get("jenkins_id")
		if jenkinsID == "" && len(cfg.JenkinsTargets) != 1 {
			jenkinsID = "new"
		}
		data.JenkinsID = jenkinsID
		if t := selectJenkinsTarget(cfg, jenkinsID); t != nil {
			data.JenkinsID = t.ID
			data.JenkinsName, data.JenkinsURL = t.Name, t.BaseURL
			data.JenkinsUser, data.JenkinsJob, data.JenkinsEnv = t.Username, t.JobPath, t.Environment
			data.JenkinsSecret, data.JenkinsDisabled = secretRefPattern.MatchString(t.SecretRef), t.Disabled
			data.JenkinsSecretID = namedSecretIDForReference(cfg, t.SecretRef)
			data.JenkinsTest = connectionTestFor(cfg, t.ID)
			data.JenkinsRun = jenkinsTriggerApproved(*t) && !t.Disabled
		}
	}
	harborID := r.URL.Query().Get("harbor_id")
	if harborID == "" && len(cfg.HarborTargets) != 1 {
		harborID = "new"
	}
	data.HarborID = harborID
	if t := selectHarborTarget(cfg, harborID); t != nil {
		data.HarborID = t.ID
		data.HarborName, data.HarborURL = t.Name, t.BaseURL
		data.HarborUser, data.HarborProject = t.Username, t.Project
		data.HarborRepository = t.Repository
		data.HarborSecret = secretRefPattern.MatchString(t.SecretRef)
		data.HarborSecretID = namedSecretIDForReference(cfg, t.SecretRef)
		data.HarborDisabled = t.Disabled
		data.HarborTest = connectionTestFor(cfg, t.ID)
	}
	if field := r.URL.Query().Get("harbor_error"); field != "" {
		data.HarborErrorField = harborErrorFieldID(field)
		if data.HarborErrorField != "" {
			data.HarborFieldError = "Correct the highlighted Harbor field before saving."
		}
	}
	for _, t := range cfg.DashboardTargets {
		data.DashboardChoices = append(data.DashboardChoices, targetChoice{ID: t.ID, Name: t.Name, Disabled: t.Disabled})
	}
	dashboardID := r.URL.Query().Get("dashboard_id")
	if dashboardID == "" && len(cfg.DashboardTargets) != 1 {
		dashboardID = "new"
	}
	data.DashboardID = dashboardID
	if t := selectDashboardTarget(cfg, dashboardID); t != nil {
		data.DashboardID, data.DashboardName, data.DashboardURL = t.ID, t.Name, t.BaseURL
		data.DashboardSecret, data.DashboardDisabled = secretRefPattern.MatchString(t.SecretRef), t.Disabled
		data.DashboardSecretID = namedSecretIDForReference(cfg, t.SecretRef)
		data.DashboardTest = connectionTestFor(cfg, t.ID)
	}
	if field := r.URL.Query().Get("dashboard_error"); field != "" {
		data.DashboardErrorField = dashboardErrorFieldID(field)
		if data.DashboardErrorField != "" {
			data.DashboardFieldError = "Correct the highlighted Dashboard field before saving."
		}
	}
	for _, task := range cfg.PythonTasks {
		data.PythonTaskChoices = append(data.PythonTaskChoices, pythonTaskChoice{ID: task.ID, Name: task.Name, Disabled: task.Disabled})
	}
	pythonTaskID := r.URL.Query().Get("python_task_id")
	if pythonTaskID == "" {
		pythonTaskID = "new"
	}
	data.PythonTaskID = pythonTaskID
	for _, task := range cfg.PythonTasks {
		if task.ID == pythonTaskID {
			data.PythonTaskID = task.ID
			data.PythonTaskName = task.Name
			data.PythonInterpreterPath = task.InterpreterPath
			data.PythonScriptPath = task.ScriptPath
			data.PythonTaskSecretEnvName = task.SecretEnvName
			data.PythonTaskSecretSaved = task.SecretRef != ""
			data.PythonTaskDisabled = task.Disabled
			break
		}
	}
	if override != nil {
		if draft := override.Harbor; draft != nil {
			data.HarborID = targetSelectionID(draft.TargetID)
			if data.HarborID != "new" && selectHarborTarget(cfg, data.HarborID) == nil {
				data.HarborID = "new"
			}
			data.HarborSecret = false
			data.HarborDisabled = false
			data.HarborTest = nil
			if target := selectHarborTarget(cfg, data.HarborID); target != nil {
				data.HarborSecret = secretRefPattern.MatchString(target.SecretRef)
				data.HarborDisabled = target.Disabled
				data.HarborTest = connectionTestFor(cfg, target.ID)
			}
			data.HarborName = draft.Name
			data.HarborURL = draft.BaseURL
			data.HarborUser = draft.Username
			data.HarborProject = draft.Project
			data.HarborRepository = draft.Repository
			data.HarborErrorField = harborErrorFieldID(draft.ErrorField)
			if data.HarborErrorField != "" {
				data.HarborFieldError = "Correct the highlighted Harbor field before saving."
			}
		}
		if draft := override.Dashboard; draft != nil {
			data.DashboardID = targetSelectionID(draft.TargetID)
			if data.DashboardID != "new" && selectDashboardTarget(cfg, data.DashboardID) == nil {
				data.DashboardID = "new"
			}
			data.DashboardSecret = false
			data.DashboardDisabled = false
			data.DashboardTest = nil
			if target := selectDashboardTarget(cfg, data.DashboardID); target != nil {
				data.DashboardSecret = secretRefPattern.MatchString(target.SecretRef)
				data.DashboardDisabled = target.Disabled
				data.DashboardTest = connectionTestFor(cfg, target.ID)
			}
			data.DashboardName = draft.Name
			data.DashboardURL = draft.BaseURL
			data.DashboardErrorField = dashboardErrorFieldID(draft.ErrorField)
			if data.DashboardErrorField != "" {
				data.DashboardFieldError = "Correct the highlighted Dashboard field before saving."
			}
		}
	}
	a.populateServiceBundlePage(&data, cfg, r.URL.Query().Get("bundle_id"))
	a.statusMu.RLock()
	if a.status != "" {
		data.Status, data.StatusIsError = a.status, a.isError
	}
	a.statusMu.RUnlock()
	if override != nil && override.Status != "" {
		data.Status = override.Status
		data.StatusIsError = override.StatusIsError
	}
	data.StatusEN, data.StatusKO = data.Status, localizeUIMessage(uiLocaleKorean, data.Status)
	data.HarborFieldErrorEN = data.HarborFieldError
	data.HarborFieldErrorKO = localizeUIMessage(uiLocaleKorean, data.HarborFieldError)
	data.DashboardFieldErrorEN = data.DashboardFieldError
	data.DashboardFieldErrorKO = localizeUIMessage(uiLocaleKorean, data.DashboardFieldError)
	if locale == uiLocaleKorean {
		data.Status = data.StatusKO
		data.HarborFieldError = data.HarborFieldErrorKO
		data.DashboardFieldError = data.DashboardFieldErrorKO
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", localUIReferrerPolicy)
	var body bytes.Buffer
	if err := page.ExecuteTemplate(&body, "ui.html", data); err != nil {
		http.Error(w, localizeUIMessage(locale, "Could not render settings."), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(statusCode)
	_, _ = body.WriteTo(w)
}

func targetSelectionID(targetID string) string {
	if targetID == "" || targetID == "new" {
		return "new"
	}
	return targetID
}

func harborErrorFieldID(field string) string {
	return map[string]string{
		"name":       "harbor-name",
		"url":        "harbor-url",
		"username":   "harbor-username",
		"project":    "harbor-project",
		"repository": "harbor-repository",
		"secret":     "harbor-secret",
	}[field]
}

func dashboardErrorFieldID(field string) string {
	return map[string]string{
		"name":  "dashboard-name",
		"url":   "dashboard-url",
		"token": "dashboard-token",
	}[field]
}

func selectGitHubTarget(cfg config, id string) *target {
	if id == "" && len(cfg.GitHubTargets) > 0 {
		return &cfg.GitHubTargets[0]
	}
	for i := range cfg.GitHubTargets {
		if cfg.GitHubTargets[i].ID == id {
			return &cfg.GitHubTargets[i]
		}
	}
	return nil
}

func selectJenkinsTarget(cfg config, id string) *jenkinsTarget {
	if id == "" && len(cfg.JenkinsTargets) > 0 {
		return &cfg.JenkinsTargets[0]
	}
	for i := range cfg.JenkinsTargets {
		if cfg.JenkinsTargets[i].ID == id {
			return &cfg.JenkinsTargets[i]
		}
	}
	return nil
}

func selectHarborTarget(cfg config, id string) *harborTarget {
	if id == "" && len(cfg.HarborTargets) > 0 {
		return &cfg.HarborTargets[0]
	}
	for i := range cfg.HarborTargets {
		if cfg.HarborTargets[i].ID == id {
			return &cfg.HarborTargets[i]
		}
	}
	return nil
}

func (a *app) handleSave(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	namedSecretID, namedSecretErr := targetNamedSecretIDFromPost(r)
	if namedSecretErr != nil {
		a.setStatus(errTargetNamedSecret.Error(), true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	targetID := strings.TrimSpace(r.FormValue("target_id"))
	if targetID == "new" {
		targetID = ""
	}
	name := strings.TrimSpace(r.FormValue("name"))
	origin, err := validateOrigin(strings.TrimSpace(r.FormValue("origin")))
	if err != nil {
		a.setStatus("Enter an HTTPS origin such as https://github.example.invalid. Paths, query strings, and credentials are not accepted.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	repo := strings.TrimSpace(r.FormValue("repository"))
	if !validRepository(repo) {
		a.setStatus("Enter one repository as owner/name using letters, numbers, dots, underscores, or hyphens.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		a.setStatus("Enter a target name up to 80 characters.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	secret := r.FormValue("token")
	if len(secret) > maxSecretSize || strings.ContainsAny(secret, "\r\n\x00") {
		a.setStatus("The personal access token is invalid or too long.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	var savedID string
	err = withConfigLock(func() error {
		var saveErr error
		savedID, saveErr = a.saveTargetWithNamedSecret(targetID, name, origin, repo, secret, namedSecretID)
		return saveErr
	})
	if err != nil {
		a.setStatus(err.Error(), true)
	} else {
		a.setStatus("GitHub target saved. Test the connection before using MCP.", false)
	}
	if savedID == "" {
		savedID = targetID
	}
	http.Redirect(w, r, "/?github_id="+url.QueryEscape(savedID), http.StatusSeeOther)
}

func (a *app) saveTarget(targetID, name, origin, repo, secret string) (string, error) {
	return a.saveTargetWithNamedSecret(targetID, name, origin, repo, secret, "")
}

func (a *app) saveTargetWithNamedSecret(targetID, name, origin, repo, secret, namedSecretID string) (string, error) {
	current, err := readConfig(a.configPath)
	if err != nil {
		return "", errors.New("Could not read current settings. They were not changed.")
	}
	index := -1
	if targetID != "" {
		index = findGitHubTargetIndex(current.GitHubTargets, targetID)
		if index < 0 {
			return "", errors.New("The selected GitHub target is not registered.")
		}
	}
	for i, existing := range current.GitHubTargets {
		if existing.Name == name && i != index {
			return "", errors.New("A GitHub target with that name is already registered.")
		}
	}
	namedRef, selectedNamedSecret, err := resolveTargetNamedSecret(current, namedSecretID, secret)
	if err != nil {
		return "", err
	}
	if secret == "" && !selectedNamedSecret && index < 0 {
		return "", errors.New("Enter a personal access token to register a new target.")
	}
	var old target
	updated := target{Name: name, Origin: origin, Repository: repo}
	if index >= 0 {
		old = current.GitHubTargets[index]
		if err := validateTarget(old); err != nil {
			return "", errors.New("The saved target is invalid. It was not changed.")
		}
		if secret == "" && !selectedNamedSecret && old.Origin != origin {
			return "", errors.New("Changing HTTPS origin requires entering a token for the new host. The saved credential was not sent.")
		}
		updated.ID, updated.SecretRef, updated.Disabled = old.ID, old.SecretRef, old.Disabled
	} else {
		updated.ID, err = newTargetID("github")
		if err != nil {
			return "", errors.New("Could not create target ID. Settings were not changed.")
		}
	}
	if _, err := validateOrigin(origin); err != nil || !validRepository(repo) || name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		return "", errors.New("The GitHub target is invalid. Settings were not changed.")
	}
	newRef := ""
	createdCredential := false
	if secret != "" {
		refBytes := make([]byte, 16)
		if _, err := rand.Read(refBytes); err != nil {
			return "", errors.New("Could not create credential reference. Settings were not changed.")
		}
		newRef = "cred:" + hex.EncodeToString(refBytes)
		if err := a.secrets.Save(newRef, []byte(secret)); err != nil {
			return "", errors.New(secretStoreError())
		}
		updated.SecretRef = newRef
		createdCredential = true
	} else if selectedNamedSecret {
		newRef = namedRef
		updated.SecretRef = namedRef
	}
	if err := validateTarget(updated); err != nil {
		if createdCredential {
			_ = a.secrets.Delete(newRef)
		}
		return "", errors.New("The target is invalid. Settings were not changed.")
	}
	next := current
	next.Version = configVersion
	if index < 0 {
		next.GitHubTargets = append(next.GitHubTargets, updated)
	} else {
		next.GitHubTargets[index] = updated
	}
	clearConnectionTest(&next, updated.ID)
	if err := writeConfig(a.configPath, next); err != nil {
		if createdCredential && a.secrets.Delete(newRef) != nil {
			return updated.ID, errors.New("Could not save settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager. Remove the Local Agent Harness credential entry before retrying.")
		}
		return "", errors.New("Could not save settings. Previous settings remain in place.")
	}
	if old.SecretRef != "" && old.SecretRef != newRef && !configReferencesSecret(next, old.SecretRef) {
		if err := a.secrets.Delete(old.SecretRef); err != nil {
			return updated.ID, errors.New("GitHub target saved, but the previous unused credential could not be removed.")
		}
	}
	return updated.ID, nil
}

func findGitHubTargetIndex(targets []target, id string) int {
	for i := range targets {
		if targets[i].ID == id {
			return i
		}
	}
	return -1
}

func configReferencesSecret(cfg config, ref string) bool {
	return configHasCredentialConsumer(cfg, ref) || configHasNamedSecretReference(cfg, ref)
}

func configHasCredentialConsumer(cfg config, ref string) bool {
	if ref == "" {
		return false
	}
	for _, t := range cfg.GitHubTargets {
		if t.SecretRef == ref {
			return true
		}
	}
	for _, t := range cfg.JenkinsTargets {
		if t.SecretRef == ref {
			return true
		}
	}
	for _, t := range cfg.HarborTargets {
		if t.SecretRef == ref {
			return true
		}
	}
	for _, t := range cfg.DashboardTargets {
		if t.SecretRef == ref {
			return true
		}
	}
	for _, task := range cfg.PythonTasks {
		if task.SecretRef == ref {
			return true
		}
	}
	return false
}

func (a *app) handleTest(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	attempt := a.testConnection(r.Context(), "github", strings.TrimSpace(r.FormValue("target_id")), strings.TrimSpace(r.FormValue("target")))
	message, failed := connectionTestMessage(attempt)
	a.setStatus(message, failed)
	http.Redirect(w, r, "/?github_id="+url.QueryEscape(strings.TrimSpace(r.FormValue("target_id"))), http.StatusSeeOther)
}

func (a *app) handleJenkinsSave(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	namedSecretID, namedSecretErr := targetNamedSecretIDFromPost(r)
	if namedSecretErr != nil {
		a.setStatus(errTargetNamedSecret.Error(), true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	targetID := strings.TrimSpace(r.FormValue("target_id"))
	if targetID == "new" {
		targetID = ""
	}
	name := strings.TrimSpace(r.FormValue("jenkins_name"))
	baseURL, err := validateJenkinsBaseURL(strings.TrimSpace(r.FormValue("jenkins_url")))
	username := strings.TrimSpace(r.FormValue("jenkins_username"))
	jobPath := strings.TrimSpace(r.FormValue("jenkins_job"))
	environment := strings.TrimSpace(r.FormValue("jenkins_environment"))
	secret := r.FormValue("jenkins_token")
	if err != nil || !validJenkinsUsername(username) || !validJenkinsJobPath(jobPath) || !validJenkinsEnvironmentLabel(environment) || name == "" || len(name) > 80 || strings.ContainsAny(name, "\r\n\x00") {
		a.setStatus("Enter a valid Jenkins name, HTTPS base URL, username, and job path.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if len(secret) > maxSecretSize || strings.ContainsAny(secret, "\r\n\x00") {
		a.setStatus("The Jenkins API token is invalid or too long.", true)
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	preapproved := r.FormValue("jenkins_nonproduction_preapproved") == "yes"
	var savedID string
	err = withConfigLock(func() error {
		var saveErr error
		savedID, saveErr = a.saveJenkinsTargetWithNamedSecret(targetID, name, baseURL, username, jobPath, environment, secret, preapproved, namedSecretID)
		return saveErr
	})
	if err != nil {
		a.setStatus(err.Error(), true)
	} else {
		a.setStatus("Jenkins target saved. Test the connection before using MCP.", false)
	}
	if savedID == "" {
		savedID = targetID
	}
	http.Redirect(w, r, "/?jenkins_id="+url.QueryEscape(savedID), http.StatusSeeOther)
}

func (a *app) handleJenkinsTest(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	attempt := a.testConnection(r.Context(), "jenkins", strings.TrimSpace(r.FormValue("target_id")), strings.TrimSpace(r.FormValue("target")))
	message, failed := connectionTestMessage(attempt)
	a.setStatus(message, failed)
	http.Redirect(w, r, "/?jenkins_id="+url.QueryEscape(strings.TrimSpace(r.FormValue("target_id"))), http.StatusSeeOther)
}

func (a *app) testConnection(ctx context.Context, kind, targetID, name string) connectionTestAttempt {
	switch kind {
	case "github":
		target, token, err := a.loadGitHubTarget(name)
		if err != nil {
			return connectionTestAttempt{}
		}
		return a.runConnectionTest(ctx, targetID, target, token, func(ctx context.Context) error {
			_, err := fetchRepository(ctx, target, string(token), a.client)
			return err
		})
	case "jenkins":
		target, token, err := a.loadJenkinsTarget(name)
		if err != nil {
			return connectionTestAttempt{}
		}
		return a.runConnectionTest(ctx, targetID, target, token, func(ctx context.Context) error {
			_, err := fetchJenkinsJob(ctx, target, string(token), a.client)
			return err
		})
	case "harbor":
		target, secret, err := a.loadHarborTarget(name)
		if err != nil {
			return connectionTestAttempt{}
		}
		return a.runConnectionTest(ctx, targetID, target, secret, func(ctx context.Context) error {
			_, err := fetchHarborArtifacts(ctx, target, string(secret), a.client)
			return err
		})
	case "dashboard":
		target, token, err := a.loadDashboardTarget(name)
		if err != nil {
			return connectionTestAttempt{}
		}
		return a.runConnectionTest(ctx, targetID, target, token, func(ctx context.Context) error {
			_, err := fetchDashboardNamespaces(ctx, target, string(token), a.client)
			return err
		})
	default:
		return connectionTestAttempt{}
	}
}

func connectionTestMessage(attempt connectionTestAttempt) (string, bool) {
	if !attempt.attempted {
		return "Connection test was not run. Check the saved target and credential settings.", true
	}
	if attempt.succeeded {
		if !attempt.historySaved {
			return "Connection test succeeded, but its history could not be saved. Retest the current target.", true
		}
		return "Connection test succeeded. Historical status saved.", false
	}

	message := connectionFailureMessage(attempt.failureKind)
	if !attempt.historySaved {
		return message + " Historical status could not be saved. Retest the current target.", true
	}
	return message + " Historical status saved.", true
}

func connectionFailureMessage(kind connectionFailureKind) string {
	switch kind {
	case connectionFailureAddress:
		return "Connection test failed: the service could not be reached. Check the saved HTTPS address, network or VPN access, proxy path, and TLS certificate."
	case connectionFailureTimeout:
		return "Connection test failed: the request timed out. Check network or VPN access and retry."
	case connectionFailureAuthentication:
		return "Connection test failed: the service rejected the saved credential (401). Replace it with a valid credential and retest."
	case connectionFailureAccess:
		return "Connection test failed: access was denied (403). Check that the account has read access to the registered resource."
	case connectionFailureEndpoint:
		return "Connection test failed: the service redirected or could not find the requested endpoint or resource. Check the saved base URL, API or context path, and resource names; this result does not distinguish missing resources from hidden access."
	case connectionFailureRateLimit:
		return "Connection test failed: the service is rate limiting requests. Wait before retrying."
	default:
		return "Connection test failed: the service returned an unexpected response. Check the saved settings and retry."
	}
}

func githubForbiddenIsRateLimited(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}

	remaining := resp.Header.Values("X-RateLimit-Remaining")
	if len(remaining) == 1 && strings.TrimSpace(remaining[0]) == "0" {
		return true
	}

	retryAfter := resp.Header.Values("Retry-After")
	if len(retryAfter) != 1 {
		return false
	}

	value := strings.TrimSpace(retryAfter[0])
	if value == "" {
		return false
	}
	if strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
		return true
	}
	_, err := http.ParseTime(value)
	return err == nil
}

func (a *app) runConnectionTest(ctx context.Context, targetID string, checked any, secret []byte, request func(context.Context) error) connectionTestAttempt {
	defer clear(secret)
	if connectionTargetID(checked) != targetID {
		return connectionTestAttempt{}
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	err := request(ctx)
	completedAt := time.Now().UTC().Format(time.RFC3339Nano)
	attempt := connectionTestAttempt{
		attempted:    true,
		succeeded:    err == nil,
		historySaved: a.recordConnectionTest(checked, err == nil, completedAt),
		failureKind:  connectionFailureFor(err),
	}
	if attempt.historySaved {
		attempt.completedAt = completedAt
	}
	return attempt
}

func connectionTargetID(value any) string {
	switch target := value.(type) {
	case target:
		return target.ID
	case jenkinsTarget:
		return target.ID
	case harborTarget:
		return target.ID
	case dashboardTarget:
		return target.ID
	default:
		return ""
	}
}

func (a *app) recordConnectionTest(checked any, succeeded bool, completedAt string) bool {
	id := connectionTargetID(checked)
	if id == "" {
		return false
	}
	result := "failure"
	if succeeded {
		result = "success"
	}
	test := connectionTest{Result: result, CompletedAt: completedAt}
	stored := false
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return err
		}
		matches := false
		switch checked := checked.(type) {
		case target:
			index := findGitHubTargetIndex(cfg.GitHubTargets, id)
			matches = index >= 0 && cfg.GitHubTargets[index] == checked
		case jenkinsTarget:
			index := findJenkinsTargetIndex(cfg.JenkinsTargets, id)
			matches = index >= 0 && cfg.JenkinsTargets[index] == checked
		case harborTarget:
			index := findHarborTargetIndex(cfg.HarborTargets, id)
			matches = index >= 0 && cfg.HarborTargets[index] == checked
		case dashboardTarget:
			index := findDashboardTargetIndex(cfg.DashboardTargets, id)
			matches = index >= 0 && cfg.DashboardTargets[index] == checked
		}
		if !matches {
			return nil
		}
		if cfg.ConnectionTests == nil {
			cfg.ConnectionTests = make(map[string]connectionTest)
		}
		cfg.ConnectionTests[id] = test
		if err := writeConfig(a.configPath, cfg); err != nil {
			return err
		}
		stored = true
		return nil
	})
	return err == nil && stored
}

func (a *app) handleToggleTarget(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	kind, id := r.FormValue("kind"), strings.TrimSpace(r.FormValue("target_id"))
	disabled := r.FormValue("disabled") == "yes"
	err := withConfigLock(func() error { return a.setTargetDisabled(kind, id, disabled) })
	if err != nil {
		a.setStatus(err.Error(), true)
	} else {
		a.setStatus("Target availability updated.", false)
	}
	key := "github_id"
	if kind == "jenkins" {
		key = "jenkins_id"
	} else if kind == "harbor" {
		key = "harbor_id"
	} else if kind == "dashboard" {
		key = "dashboard_id"
	}
	http.Redirect(w, r, "/?"+key+"="+url.QueryEscape(id), http.StatusSeeOther)
}

func (a *app) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	if !a.checkPost(w, r) {
		return
	}
	kind, id := r.FormValue("kind"), strings.TrimSpace(r.FormValue("target_id"))
	key := "github_id"
	if kind == "jenkins" {
		key = "jenkins_id"
	} else if kind == "harbor" {
		key = "harbor_id"
	} else if kind == "dashboard" {
		key = "dashboard_id"
	}
	if r.FormValue("confirm_delete") != "yes" {
		a.setStatus("Check the delete confirmation before removing this target.", true)
		http.Redirect(w, r, "/?"+key+"="+url.QueryEscape(id), http.StatusSeeOther)
		return
	}
	err := withConfigLock(func() error { return a.deleteRegisteredTarget(kind, id) })
	if err != nil {
		a.setStatus(err.Error(), true)
	} else {
		a.setStatus("Target deleted from local settings.", false)
	}
	http.Redirect(w, r, "/?"+key+"=new", http.StatusSeeOther)
}

func (a *app) setTargetDisabled(kind, id string, disabled bool) error {
	cfg, err := readConfig(a.configPath)
	if err != nil {
		return errors.New("Could not read current settings.")
	}
	switch kind {
	case "github":
		index := findGitHubTargetIndex(cfg.GitHubTargets, id)
		if index < 0 {
			return errors.New("The selected GitHub target is not registered.")
		}
		cfg.GitHubTargets[index].Disabled = disabled
	case "jenkins":
		index := findJenkinsTargetIndex(cfg.JenkinsTargets, id)
		if index < 0 {
			return errors.New("The selected Jenkins target is not registered.")
		}
		cfg.JenkinsTargets[index].Disabled = disabled
		if disabled {
			cfg.JenkinsTargets[index].NonProductionPreapproved = false
			cfg.JenkinsTargets[index].NonProductionApprovalScope = ""
		}
	case "harbor":
		index := findHarborTargetIndex(cfg.HarborTargets, id)
		if index < 0 {
			return errors.New("The selected Harbor target is not registered.")
		}
		cfg.HarborTargets[index].Disabled = disabled
	case "dashboard":
		index := findDashboardTargetIndex(cfg.DashboardTargets, id)
		if index < 0 {
			return errors.New("The selected Dashboard target is not registered.")
		}
		cfg.DashboardTargets[index].Disabled = disabled
	default:
		return errors.New("Unknown target type.")
	}
	cfg.Version = configVersion
	if err := writeConfig(a.configPath, cfg); err != nil {
		return errors.New("Could not update target availability. Settings remain unchanged.")
	}
	return nil
}

func (a *app) deleteRegisteredTarget(kind, id string) error {
	cfg, err := readConfig(a.configPath)
	if err != nil {
		return errors.New("Could not read current settings.")
	}
	if serviceBundleReferencesTarget(cfg, kind, id) {
		return errors.New("Remove this target from its service bundle before deleting it.")
	}
	var ref string
	switch kind {
	case "github":
		index := findGitHubTargetIndex(cfg.GitHubTargets, id)
		if index < 0 {
			return errors.New("The selected GitHub target is not registered.")
		}
		ref = cfg.GitHubTargets[index].SecretRef
		cfg.GitHubTargets = append(cfg.GitHubTargets[:index], cfg.GitHubTargets[index+1:]...)
	case "jenkins":
		index := findJenkinsTargetIndex(cfg.JenkinsTargets, id)
		if index < 0 {
			return errors.New("The selected Jenkins target is not registered.")
		}
		ref = cfg.JenkinsTargets[index].SecretRef
		cfg.JenkinsTargets = append(cfg.JenkinsTargets[:index], cfg.JenkinsTargets[index+1:]...)
	case "harbor":
		index := findHarborTargetIndex(cfg.HarborTargets, id)
		if index < 0 {
			return errors.New("The selected Harbor target is not registered.")
		}
		ref = cfg.HarborTargets[index].SecretRef
		cfg.HarborTargets = append(cfg.HarborTargets[:index], cfg.HarborTargets[index+1:]...)
	case "dashboard":
		index := findDashboardTargetIndex(cfg.DashboardTargets, id)
		if index < 0 {
			return errors.New("The selected Dashboard target is not registered.")
		}
		ref = cfg.DashboardTargets[index].SecretRef
		cfg.DashboardTargets = append(cfg.DashboardTargets[:index], cfg.DashboardTargets[index+1:]...)
	default:
		return errors.New("Unknown target type.")
	}
	// readConfig populates these singleton compatibility aliases; clear them so
	// deleting the final target does not restore it while writing.
	cfg.Target, cfg.Jenkins = nil, nil
	clearConnectionTest(&cfg, id)
	cfg.Version = configVersion
	if err := writeConfig(a.configPath, cfg); err != nil {
		return errors.New("Could not delete target. Settings remain unchanged.")
	}
	if !configReferencesSecret(cfg, ref) {
		if err := a.secrets.Delete(ref); err != nil {
			return errors.New("Target was deleted from settings, but its unused credential could not be removed from Windows Credential Manager.")
		}
	}
	return nil
}

func (a *app) checkPost(w http.ResponseWriter, r *http.Request) bool {
	return a.checkPostLimit(w, r, maxFormSize)
}

func (a *app) checkPostLimit(w http.ResponseWriter, r *http.Request, limit int64) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, localizeUIMessage(uiLocaleForRequest(r), "Method not allowed"), http.StatusMethodNotAllowed)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var parseErr error
	if mediaErr == nil && mediaType == "multipart/form-data" {
		parseErr = r.ParseMultipartForm(limit)
	} else {
		parseErr = r.ParseForm()
	}
	if parseErr != nil || r.FormValue("csrf") != a.csrf {
		http.Error(w, localizeUIMessage(uiLocaleForRequest(r), "Request rejected"), http.StatusForbidden)
		return false
	}
	return true
}

func (a *app) setStatus(message string, failed bool) {
	a.statusMu.Lock()
	a.status, a.isError = message, failed
	a.statusMu.Unlock()
}

func (a *app) mcpServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "local-agent-harness", Version: "0.1.0"}, nil)
	registerSetupDraftMCPTool(server, a.setupDrafts)
	if a.ssh != nil {
		mcp.AddTool[struct{}, []SSHOperationCatalogTarget](server, &mcp.Tool{
			Name:        mcpToolRegisteredSSHTargets,
			Description: "List named SSH targets and the fixed operations the user saved for each target. This passive catalog does not contact a host and omits aliases, commands, and credentials.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, []SSHOperationCatalogTarget, error) {
			targets, err := a.ssh.ListSSHOperationCatalog(ctx)
			if err != nil {
				return nil, nil, errSSHConfigurationUnavailable
			}
			return nil, targets, nil
		})
		mcp.AddTool[SSHOperationInput, SSHOperationResult](server, &mcp.Tool{
			Name:        mcpToolRunSSHOperation,
			Description: "Run an operation saved for the selected named SSH target. The request accepts only saved target and operation identifiers plus typed values for parameters declared by that saved operation; it cannot select a host, command, endpoint, or credential. State-changing and destructive operations use their saved exact-scope approval policy. A cancellation or transport loss may leave the remote outcome unknown and is never retried automatically.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, input SSHOperationInput) (*mcp.CallToolResult, SSHOperationResult, error) {
			result, err := a.ssh.RunSSHOperation(ctx, input)
			if err != nil {
				return nil, SSHOperationResult{}, errSSHOperationRejected
			}
			return nil, result, nil
		})
	}
	mcp.AddTool[struct{}, registeredTargetsResult](server, &mcp.Tool{
		Name:        "registered_targets",
		Description: "List enabled registered targets and callable tools by adapter display name. Connection test status is historical and reports success, failure, or not_tested. Completed tests include their UTC completion time. This listing does not contact targets. It is inventory, not approval; calls are checked against saved target, resource, and action policy. Stored credential values and references are omitted.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, registeredTargetsResult, error) {
		result, err := a.registeredTargets()
		return nil, result, err
	})
	mcp.AddTool[struct{}, registeredServiceBundlesResult](server, &mcp.Tool{
		Name:        "registered_service_bundles",
		Description: "List registered service bundles, environments, linked target display names, and callable tools. Connection test status is historical and reports success, failure, or not_tested. Completed tests include their UTC completion time. This listing does not contact targets. It is inventory, not approval; calls are checked against saved policy. Stored credential values and references are omitted.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, registeredServiceBundlesResult, error) {
		result, err := a.registeredServiceBundles()
		return nil, result, err
	})
	mcp.AddTool[registeredTargetConnectionTestInput, registeredTargetConnectionTestResult](server, &mcp.Tool{
		Name:        "registered_target_connection_test",
		Description: "For an enabled, valid target with an available saved credential, send one bounded read-only request to the exact registered target selected by adapter and display name. Attempt to save success or failure with its UTC completion time in local settings; the result is not_tested if no request starts, and completed_at is present only when history is saved. This call contacts the saved service and may update local test history; catalog reads do not. No URL, resource path, credential, or permission can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in registeredTargetConnectionTestInput) (*mcp.CallToolResult, registeredTargetConnectionTestResult, error) {
		result, err := a.testRegisteredTargetConnection(ctx, in)
		return nil, result, err
	})
	mcp.AddTool[struct{}, pythonTasksResult](server, &mcp.Tool{
		Name:        "registered_python_tasks",
		Description: "List enabled registered Python task names and the run tool they permit. This passive catalog reads settings only, does not check file availability or contact a network, and never returns executable or script paths.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, pythonTasksResult, error) {
		result, err := a.registeredPythonTasks()
		return nil, result, err
	})
	mcp.AddTool[pythonTaskInput, pythonTaskResult](server, &mcp.Tool{
		Name:        "python_run_registered_task",
		Description: pythonTaskRunToolDescription,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input pythonTaskInput) (*mcp.CallToolResult, pythonTaskResult, error) {
		result, err := a.runRegisteredPythonTask(ctx, input)
		return nil, result, err
	})
	mcp.AddTool[githubTargetInput, repository](server, &mcp.Tool{
		Name:        "github_repository",
		Description: "Read the repository registered under the exact GitHub target name. No URL or credential can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in githubTargetInput) (*mcp.CallToolResult, repository, error) {
		result, err := a.registeredRepository(ctx, in.Target)
		return nil, result, err
	})
	mcp.AddTool[githubPullRequestInput, githubPullRequest](server, &mcp.Tool{
		Name:        "github_pull_request",
		Description: "Read a pull request from the repository stored on an exact registered GitHub target. The positive pull request number is the only resource selector; URL, repository, and credentials cannot be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in githubPullRequestInput) (*mcp.CallToolResult, githubPullRequest, error) {
		result, err := a.registeredGitHubPullRequest(ctx, in.Target, in.Number)
		return nil, result, err
	})
	mcp.AddTool[harborArtifactsInput, harborArtifactsResult](server, &mcp.Tool{
		Name:        "harbor_repository_artifacts",
		Description: "Read a bounded artifact page for the repository fixed to the exact registered Harbor target name. No URL, project, repository, or credential can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in harborArtifactsInput) (*mcp.CallToolResult, harborArtifactsResult, error) {
		result, err := a.registeredHarborArtifacts(ctx, in.Target)
		return nil, result, err
	})
	mcp.AddTool[harborArtifactsInput, harborProjectQuotaResult](server, &mcp.Tool{
		Name:        "harbor_project_quota",
		Description: "Read quota for the project fixed to the exact registered Harbor target name. No URL, project, repository, or credential can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in harborArtifactsInput) (*mcp.CallToolResult, harborProjectQuotaResult, error) {
		result, err := a.registeredHarborProjectQuota(ctx, in.Target)
		return nil, result, err
	})
	mcp.AddTool[dashboardTargetInput, dashboardNamespacesResult](server, &mcp.Tool{
		Name:        "dashboard_namespaces",
		Description: "List at most 100 namespace names visible to the exact registered Kubernetes Dashboard target. No URL, path, or credential can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dashboardTargetInput) (*mcp.CallToolResult, dashboardNamespacesResult, error) {
		result, err := a.registeredDashboardNamespaces(ctx, in.Target)
		return nil, result, err
	})
	mcp.AddTool[dashboardDeploymentStatusInput, dashboardDeploymentStatusResult](server, &mcp.Tool{
		Name:        "dashboard_deployment_status",
		Description: "Read Deployment replica status from the exact Dashboard target, namespace, and Deployment mapped to a registered service bundle environment. Only the service bundle and environment names are inputs; URL and resource names cannot be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dashboardDeploymentStatusInput) (*mcp.CallToolResult, dashboardDeploymentStatusResult, error) {
		result, err := a.registeredDashboardDeploymentStatus(ctx, in.ServiceBundle, in.Environment)
		return nil, result, err
	})
	mcp.AddTool[dashboardDeploymentStatusInput, dashboardDeploymentDiagnosisResult](server, &mcp.Tool{
		Name:        "dashboard_deployment_diagnosis",
		Description: "Read Deployment status, bounded related events, and Pod inventory concurrently for the exact Dashboard target and resources mapped to a registered service bundle environment. These reads may reflect nearby but different observation times; they are not an atomic snapshot. Only service bundle and environment names are inputs. Use dashboard_deployment_pod_logs separately for a selected Pod/container.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dashboardDeploymentStatusInput) (*mcp.CallToolResult, dashboardDeploymentDiagnosisResult, error) {
		result, err := a.registeredDashboardDeploymentDiagnosis(ctx, in.ServiceBundle, in.Environment)
		return nil, result, err
	})
	mcp.AddTool[dashboardDeploymentStatusInput, dashboardDeploymentEventsResult](server, &mcp.Tool{
		Name:        "dashboard_deployment_events",
		Description: "Read bounded events for the Deployment mapped to a registered service bundle environment. Only service bundle and environment names are inputs; URL and resource names cannot be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dashboardDeploymentStatusInput) (*mcp.CallToolResult, dashboardDeploymentEventsResult, error) {
		result, err := a.registeredDashboardDeploymentEvents(ctx, in.ServiceBundle, in.Environment)
		return nil, result, err
	})
	mcp.AddTool[dashboardDeploymentStatusInput, dashboardDeploymentPodsResult](server, &mcp.Tool{
		Name:        "dashboard_deployment_pods",
		Description: "Read bounded Pod status for the Deployment mapped to a registered service bundle environment. Only service bundle and environment names are inputs; URLs, namespaces, Deployment names, selectors, and credentials cannot be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dashboardDeploymentStatusInput) (*mcp.CallToolResult, dashboardDeploymentPodsResult, error) {
		result, err := a.registeredDashboardDeploymentPods(ctx, in.ServiceBundle, in.Environment)
		return nil, result, err
	})
	mcp.AddTool[dashboardDeploymentLogsInput, dashboardDeploymentLogsResult](server, &mcp.Tool{
		Name:        "dashboard_deployment_pod_logs",
		Description: "Read the latest fixed window of up to 100 log lines (up to 4 KiB each) for a Pod and container currently present in the mapped registered Deployment inventory. The full MCP response is capped at 64 KiB and marked truncated if shortened. Pod and container are candidate selectors only; namespace, Deployment, URL, log offsets, and credentials cannot be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in dashboardDeploymentLogsInput) (*mcp.CallToolResult, dashboardDeploymentLogsResult, error) {
		result, err := a.registeredDashboardDeploymentLogs(ctx, in.ServiceBundle, in.Environment, in.Pod, in.Container)
		return nil, result, err
	})
	mcp.AddTool[jenkinsTargetInput, jenkinsJobInfo](server, &mcp.Tool{
		Name:        "jenkins_registered_job",
		Description: "Read the Jenkins job registered under the exact target name. No URL, job path, or credential can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jenkinsTargetInput) (*mcp.CallToolResult, jenkinsJobInfo, error) {
		result, err := a.registeredJenkinsJob(ctx, in.Target)
		return nil, result, err
	})
	mcp.AddTool[jenkinsQueueInput, jenkinsQueueInfo](server, &mcp.Tool{
		Name:        "jenkins_registered_queue_item",
		Description: "Read Jenkins queue status for the exact registered target name and a positive numeric queue ID.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jenkinsQueueInput) (*mcp.CallToolResult, jenkinsQueueInfo, error) {
		result, err := a.registeredJenkinsQueue(ctx, in.Target, in.QueueID)
		return nil, result, err
	})
	mcp.AddTool[jenkinsLogInput, jenkinsLogSegmentResult](server, &mcp.Tool{
		Name:        "jenkins_registered_build_log",
		Description: "Read one bounded progressive log segment for the exact registered target name, positive build number, and nonnegative offset.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jenkinsLogInput) (*mcp.CallToolResult, jenkinsLogSegmentResult, error) {
		result, err := a.registeredJenkinsLog(ctx, in.Target, in.BuildNumber, in.StartOffset)
		return nil, result, err
	})
	mcp.AddTool[jenkinsTargetInput, jenkinsRunResult](server, &mcp.Tool{
		Name:        "jenkins_run_registered_job",
		Description: "Trigger a Jenkins job registered under the exact target name. Current explicit non-production preapproval runs directly; other enabled jobs require one-time local browser confirmation. No URL, job path, or credential can be supplied.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in jenkinsTargetInput) (*mcp.CallToolResult, jenkinsRunResult, error) {
		result, err := a.runRegisteredJenkinsJob(ctx, in.Target)
		return nil, result, err
	})
	return server
}

func (a *app) registeredTargets() (registeredTargetsResult, error) {
	result := registeredTargetsResult{Targets: []registeredTarget{}}
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local target settings are invalid.")
		}
		for _, t := range cfg.GitHubTargets {
			if !t.Disabled {
				result.Targets = append(result.Targets, registeredTarget{Type: "github", Name: t.Name, Actions: []string{"github_repository", "github_pull_request", "registered_target_connection_test"}, ConnectionTest: catalogConnectionTestFor(cfg, t.ID)})
			}
		}
		for _, t := range cfg.JenkinsTargets {
			if !t.Disabled {
				actions := []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log", "registered_target_connection_test"}
				if validateJenkinsTarget(t) == nil {
					actions = append(actions, "jenkins_run_registered_job")
				}
				result.Targets = append(result.Targets, registeredTarget{Type: "jenkins", Name: t.Name, Actions: actions, ConnectionTest: catalogConnectionTestFor(cfg, t.ID)})
			}
		}
		for _, t := range cfg.HarborTargets {
			if !t.Disabled {
				result.Targets = append(result.Targets, registeredTarget{Type: "harbor", Name: t.Name, Actions: []string{"harbor_repository_artifacts", "harbor_project_quota", "registered_target_connection_test"}, ConnectionTest: catalogConnectionTestFor(cfg, t.ID)})
			}
		}
		for _, t := range cfg.DashboardTargets {
			if !t.Disabled {
				result.Targets = append(result.Targets, registeredTarget{Type: "dashboard", Name: t.Name, Actions: []string{"dashboard_namespaces", "registered_target_connection_test"}, ConnectionTest: catalogConnectionTestFor(cfg, t.ID)})
			}
		}
		sort.Slice(result.Targets, func(i, j int) bool {
			if result.Targets[i].Type == result.Targets[j].Type {
				return result.Targets[i].Name < result.Targets[j].Name
			}
			return result.Targets[i].Type < result.Targets[j].Type
		})
		return nil
	})
	return result, err
}

func (a *app) registeredRepository(ctx context.Context, requestedName string) (repository, error) {
	t, token, err := a.loadGitHubTarget(requestedName)
	if err != nil {
		return repository{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchRepository(ctx, t, string(token), a.client)
}

func (a *app) registeredGitHubPullRequest(ctx context.Context, requestedName string, number int) (githubPullRequest, error) {
	if number <= 0 {
		return githubPullRequest{}, errors.New("Pull request number must be a positive integer.")
	}
	t, token, err := a.loadGitHubTarget(requestedName)
	if err != nil {
		return githubPullRequest{}, err
	}
	defer clear(token)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchGitHubPullRequest(ctx, t, string(token), number, a.client)
}

func fetchGitHubPullRequest(ctx context.Context, t target, token string, number int, client *http.Client) (githubPullRequest, error) {
	if number <= 0 {
		return githubPullRequest{}, errors.New("Pull request number must be a positive integer.")
	}
	if validateTarget(t) != nil || token == "" || strings.ContainsAny(token, "\r\n\x00") || client == nil {
		return githubPullRequest{}, errors.New("The registered GitHub target or credential is invalid.")
	}
	baseURL, err := repositoryURL(t)
	if err != nil {
		return githubPullRequest{}, errors.New("The registered GitHub target is invalid.")
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil {
		return githubPullRequest{}, errors.New("The registered GitHub target is invalid.")
	}
	endpoint.Path += "/pulls/" + strconv.Itoa(number)
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return githubPullRequest{}, errors.New("Could not create GitHub pull request request.")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "local-agent-harness")
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := clientCopy.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return githubPullRequest{}, errors.New("GitHub pull request request timed out.")
		}
		return githubPullRequest{}, errors.New("Could not reach registered GitHub Enterprise origin.")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return githubPullRequest{}, errors.New("GitHub redirected pull request request. Check enterprise origin and /api/v3 path.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return githubPullRequest{}, errors.New("GitHub rejected credential (401). Replace token or check its access.")
		case http.StatusForbidden:
			if githubForbiddenIsRateLimited(resp) {
				return githubPullRequest{}, errors.New("GitHub rate limited request. Wait before retrying.")
			}
			return githubPullRequest{}, errors.New("GitHub denied pull request access (403). Check token's repository read permission.")
		case http.StatusNotFound:
			return githubPullRequest{}, errors.New("GitHub could not find the registered pull request (404). Check repository access and number.")
		case http.StatusTooManyRequests:
			return githubPullRequest{}, errors.New("GitHub rate limited request. Wait before retrying.")
		default:
			return githubPullRequest{}, fmt.Errorf("GitHub returned HTTP %d for pull request.", resp.StatusCode)
		}
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return githubPullRequest{}, errors.New("GitHub pull request response exceeded 1 MiB or could not be read.")
	}
	var raw struct {
		Number    *int       `json:"number"`
		Title     *string    `json:"title"`
		State     *string    `json:"state"`
		Draft     *bool      `json:"draft"`
		Merged    *bool      `json:"merged"`
		UpdatedAt *time.Time `json:"updated_at"`
		Base      *struct {
			Ref  *string `json:"ref"`
			Repo *struct {
				FullName *string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
		Head *struct {
			Ref *string `json:"ref"`
			SHA *string `json:"sha"`
		} `json:"head"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Number == nil || raw.Title == nil || raw.State == nil || raw.Draft == nil || raw.Merged == nil || raw.UpdatedAt == nil || raw.Base == nil || raw.Base.Ref == nil || raw.Base.Repo == nil || raw.Base.Repo.FullName == nil || raw.Head == nil || raw.Head.Ref == nil || raw.Head.SHA == nil {
		return githubPullRequest{}, errors.New("GitHub returned an invalid pull request response.")
	}
	if *raw.Number != number || (*raw.State != "open" && *raw.State != "closed") || !validRepository(*raw.Base.Repo.FullName) || !strings.EqualFold(*raw.Base.Repo.FullName, t.Repository) || !validGitHubRef(*raw.Base.Ref) || !validGitHubRef(*raw.Head.Ref) || !validGitHubSHA(*raw.Head.SHA) || len(*raw.Title) > 4096 {
		return githubPullRequest{}, errors.New("GitHub pull request identity or fields did not match the registered request.")
	}
	result := githubPullRequest{
		Number:    *raw.Number,
		Title:     cleanOutput(*raw.Title, token, 512),
		State:     cleanOutput(*raw.State, token, 16),
		Draft:     *raw.Draft,
		Merged:    *raw.Merged,
		BaseRef:   cleanOutput(*raw.Base.Ref, token, 1024),
		HeadRef:   cleanOutput(*raw.Head.Ref, token, 1024),
		HeadSHA:   cleanOutput(*raw.Head.SHA, token, 64),
		UpdatedAt: raw.UpdatedAt.UTC(),
	}
	return result, nil
}

func validGitHubRef(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\r\n\x00")
}

func validGitHubSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
			return false
		}
	}
	return true
}

func (a *app) loadGitHubTarget(requestedName string) (target, []byte, error) {
	var selected target
	var token []byte
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("Local GitHub target settings are invalid.")
		}
		index := findGitHubTargetNameIndex(cfg.GitHubTargets, requestedName)
		if index < 0 {
			return errors.New("The requested GitHub target name is not registered.")
		}
		selected = cfg.GitHubTargets[index]
		if selected.Disabled {
			return errors.New("The registered GitHub target is disabled.")
		}
		if err := validateTarget(selected); err != nil {
			return errors.New("The saved GitHub target is invalid. Review it in local settings.")
		}
		token, err = a.secrets.Load(selected.SecretRef)
		if err != nil || len(token) == 0 || len(token) > maxSecretSize {
			clear(token)
			token = nil
			return errors.New("The saved GitHub credential is unavailable. Replace it in local settings.")
		}
		return nil
	})
	if err != nil {
		return target{}, nil, err
	}
	return selected, token, nil
}

func findGitHubTargetNameIndex(targets []target, name string) int {
	for i := range targets {
		if targets[i].Name == name {
			return i
		}
	}
	return -1
}

func fetchRepository(ctx context.Context, t target, token string, client *http.Client) (repository, error) {
	if err := validateTarget(t); err != nil || client == nil {
		return repository{}, errors.New("The registered GitHub target is invalid.")
	}
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return repository{}, errors.New("The saved GitHub credential is invalid.")
	}
	endpoint, err := repositoryURL(t)
	if err != nil {
		return repository{}, errors.New("The registered GitHub target is invalid.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return repository{}, errors.New("Could not create the GitHub request.")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "local-agent-harness")
	resp, err := client.Do(req)
	if err != nil {
		return repository{}, connectionTransportFailure(
			err,
			"Could not reach the registered GitHub Enterprise origin. Check the address, VPN, and TLS certificate.",
		)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return repository{}, withConnectionDiagnostic(connectionFailureEndpoint, errors.New("GitHub redirected the request. Check the enterprise origin and /api/v3 path."))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusRequestTimeout:
			return repository{}, withConnectionDiagnostic(connectionFailureTimeout, errors.New("GitHub request timed out (408)."))
		case http.StatusUnauthorized:
			return repository{}, withConnectionDiagnostic(connectionFailureAuthentication, errors.New("GitHub rejected the credential (401). Replace the token or check its access."))
		case http.StatusForbidden:
			if githubForbiddenIsRateLimited(resp) {
				return repository{}, withConnectionDiagnostic(connectionFailureRateLimit, errors.New("GitHub rate limited the request. Wait and retry."))
			}
			return repository{}, withConnectionDiagnostic(connectionFailureAccess, errors.New("GitHub denied access (403). Check the token's repository read permission."))
		case http.StatusNotFound:
			return repository{}, withConnectionDiagnostic(connectionFailureEndpoint, errors.New("GitHub could not find the registered repository (404). Check owner/name and access."))
		case http.StatusTooManyRequests:
			return repository{}, withConnectionDiagnostic(connectionFailureRateLimit, errors.New("GitHub rate limited the request. Wait and retry."))
		default:
			return repository{}, fmt.Errorf("GitHub returned HTTP %d.", resp.StatusCode)
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes+1))
	if err != nil || len(body) > maxAPIBytes {
		return repository{}, errors.New("GitHub response exceeded the 1 MiB limit or could not be read.")
	}
	var raw repository
	if err := json.Unmarshal(body, &raw); err != nil {
		return repository{}, errors.New("GitHub returned an invalid repository response.")
	}
	if raw.FullName == "" || raw.Name == "" || !validRepository(raw.FullName) || !strings.EqualFold(raw.FullName, t.Repository) {
		return repository{}, errors.New("GitHub response did not contain a valid repository identity.")
	}
	return repository{
		Name:          cleanOutput(raw.Name, token, 100),
		FullName:      cleanOutput(raw.FullName, token, 201),
		Private:       raw.Private,
		DefaultBranch: cleanOutput(raw.DefaultBranch, token, 255),
		HTMLURL:       cleanOutput(t.Origin+"/"+t.Repository, token, 1024),
		Description:   cleanOutput(raw.Description, token, 512),
	}, nil
}

func cleanOutput(value, secret string, limit int) string {
	return cleanOutputWithReplacement(value, secret, limit, false, "[REDACTED]")
}

func cleanOutputWithReplacement(value, secret string, limit int, preserveLineBreaks bool, replacement string) string {
	if privateKeyPEM.MatchString(value) {
		return replacement
	}
	if secret != "" {
		value = strings.ReplaceAll(value, secret, replacement)
	}
	value = githubTokenPattern.ReplaceAllString(value, replacement)
	value = credentialHeader.ReplaceAllString(value, "Authorization$1"+replacement)
	redactWholeValue := false
	value = credentialPattern.ReplaceAllStringFunc(value, func(match string) string {
		parts := credentialPattern.FindStringSubmatch(match)
		redacted := replacement
		fieldValue := parts[5]
		if isPrivateKeyField(parts[2]) || (fieldValue != "[REDACTED]" && (strings.HasPrefix(fieldValue, "{") || strings.HasPrefix(fieldValue, "["))) {
			redactWholeValue = true
		}
		if len(fieldValue) >= 2 && (fieldValue[0] == '"' || fieldValue[0] == '\'') && fieldValue[len(fieldValue)-1] == fieldValue[0] {
			redacted = fieldValue[:1] + redacted + fieldValue[:1]
		} else if parts[1] == `"` && parts[3] == `"` {
			redacted = `"` + redacted + `"`
		}
		return parts[1] + parts[2] + parts[3] + parts[4] + redacted
	})
	if redactWholeValue {
		// ponytail: discard the bounded value for composite or private-key data; use format-aware projection if retaining it is needed.
		return replacement
	}
	value = strings.Map(func(r rune) rune {
		if preserveLineBreaks && (r == '\n' || r == '\r' || r == '\t') {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
	if utf8.RuneCountInString(value) > limit {
		runes := []rune(value)
		value = string(runes[:limit]) + "…"
	}
	return value
}

func isPrivateKeyField(field string) bool {
	field = strings.ToLower(field)
	return strings.HasSuffix(field, "private_key") || strings.HasSuffix(field, "private-key") || strings.HasSuffix(field, "privatekey")
}

func newGitHubClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func repositoryURL(t target) (string, error) {
	base, err := url.Parse(t.Origin)
	if err != nil {
		return "", err
	}
	base.Path = "/api/v3/repos/" + t.Repository
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base.String(), nil
}

func validateTarget(t target) error {
	_, err := validateOrigin(t.Origin)
	if err != nil {
		return err
	}
	if !githubIDPattern.MatchString(t.ID) {
		return errors.New("invalid GitHub target ID")
	}
	if strings.TrimSpace(t.Name) == "" || len(t.Name) > 80 || strings.ContainsAny(t.Name, "\r\n\x00") {
		return errors.New("invalid target name")
	}
	if !validRepository(t.Repository) || !secretRefPattern.MatchString(t.SecretRef) {
		return errors.New("invalid target repository or credential reference")
	}
	return nil
}

func validateOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if len(raw) > 2048 || strings.ContainsAny(raw, "?#") || err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" ||
		strings.ContainsAny(u.Host, "\r\n\t ") {
		return "", errors.New("origin must be HTTPS only")
	}
	if u.Hostname() == "" || strings.HasSuffix(u.Hostname(), ".") {
		return "", errors.New("invalid host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid port")
		}
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "", "", "", ""
	return u.Scheme + "://" + u.Host, nil
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 || len(value) > 400 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
				return false
			}
		}
	}
	return true
}

func configFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "LocalAgentHarness", "config.json"), nil
}

func readConfig(path string) (config, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return config{Version: configVersion}, nil
	}
	if err != nil {
		return config{}, errors.New("settings unavailable")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigSize+1))
	if err != nil || len(data) > maxConfigSize {
		return config{}, errors.New("settings unavailable")
	}
	var cfg config
	if decodeConfig(data, &cfg) != nil || validateConfig(cfg) != nil || validateNamedSecretReferenceCoverage(cfg) != nil {
		return config{}, errors.New("settings invalid")
	}
	setConfigAliases(&cfg)
	return cfg, nil
}

func decodeConfig(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("trailing settings data")
	}
	return nil
}

func newTargetID(kind string) (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return kind + ":" + hex.EncodeToString(value), nil
}

func setConfigAliases(cfg *config) {
	cfg.Target, cfg.Jenkins = nil, nil
	if len(cfg.GitHubTargets) == 1 {
		t := cfg.GitHubTargets[0]
		cfg.Target = &t
	}
	if len(cfg.JenkinsTargets) == 1 {
		j := cfg.JenkinsTargets[0]
		cfg.Jenkins = &j
	}
}

func validateConfig(cfg config) error {
	if cfg.Version != configVersion {
		return errors.New("invalid settings version")
	}
	if err := validateNamedSecrets(cfg.NamedSecrets); err != nil {
		return err
	}
	if len(cfg.SSHTargets) > maxSSHTargetCount || validateSSHTargetSet(cfg.SSHTargets) != nil {
		return errors.New("invalid or duplicate SSH target")
	}
	githubIDs, githubNames := map[string]bool{}, map[string]bool{}
	for _, t := range cfg.GitHubTargets {
		if validateTarget(t) != nil || githubIDs[t.ID] || githubNames[t.Name] {
			return errors.New("invalid or duplicate GitHub target")
		}
		githubIDs[t.ID], githubNames[t.Name] = true, true
	}
	jenkinsIDs, jenkinsNames := map[string]bool{}, map[string]bool{}
	for _, t := range cfg.JenkinsTargets {
		if validateJenkinsTarget(t) != nil || jenkinsIDs[t.ID] || jenkinsNames[t.Name] {
			return errors.New("invalid or duplicate Jenkins target")
		}
		jenkinsIDs[t.ID], jenkinsNames[t.Name] = true, true
	}
	harborIDs, harborNames := map[string]bool{}, map[string]bool{}
	for _, t := range cfg.HarborTargets {
		if validateHarborTarget(t) != nil || harborIDs[t.ID] || harborNames[t.Name] {
			return errors.New("invalid or duplicate Harbor target")
		}
		harborIDs[t.ID], harborNames[t.Name] = true, true
	}
	dashboardIDs, dashboardNames := map[string]bool{}, map[string]bool{}
	for _, t := range cfg.DashboardTargets {
		if validateDashboardTarget(t) != nil || dashboardIDs[t.ID] || dashboardNames[t.Name] {
			return errors.New("invalid or duplicate Dashboard target")
		}
		dashboardIDs[t.ID], dashboardNames[t.Name] = true, true
	}
	pythonTaskIDs, pythonTaskNames := map[string]bool{}, map[string]bool{}
	if len(cfg.PythonTasks) > maxPythonTasks {
		return errors.New("too many registered Python tasks")
	}
	for _, task := range cfg.PythonTasks {
		nameKey := strings.ToLower(task.Name)
		if validatePythonTask(task) != nil || pythonTaskIDs[task.ID] || pythonTaskNames[nameKey] {
			return errors.New("invalid or duplicate Python task")
		}
		pythonTaskIDs[task.ID] = true
		pythonTaskNames[nameKey] = true
	}
	for id := range cfg.ConnectionTests {
		if !connectionTestTargetExists(cfg, id) {
			return errors.New("invalid connection test history")
		}
	}
	return validateServiceBundles(cfg)
}

func connectionTestTargetExists(cfg config, id string) bool {
	return findGitHubTargetIndex(cfg.GitHubTargets, id) >= 0 || findJenkinsTargetIndex(cfg.JenkinsTargets, id) >= 0 || findHarborTargetIndex(cfg.HarborTargets, id) >= 0 || findDashboardTargetIndex(cfg.DashboardTargets, id) >= 0
}

type configV3 struct {
	Version          int               `json:"version"`
	GitHubTargets    []target          `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget   `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget    `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle   `json:"service_bundles,omitempty"`
}

type configV4 struct {
	ConnectionTests  map[string]connectionTest `json:"connection_tests,omitempty"`
	Version          int                       `json:"version"`
	GitHubTargets    []target                  `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget           `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget            `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget         `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle           `json:"service_bundles,omitempty"`
}

type configV5 struct {
	ConnectionTests  map[string]connectionTest `json:"connection_tests,omitempty"`
	Version          int                       `json:"version"`
	GitHubTargets    []target                  `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget           `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget            `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget         `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle           `json:"service_bundles,omitempty"`
	PythonTasks      []pythonTaskV5            `json:"python_tasks,omitempty"`
}

type configV6 struct {
	ConnectionTests  map[string]connectionTest `json:"connection_tests,omitempty"`
	Version          int                       `json:"version"`
	GitHubTargets    []target                  `json:"github_targets,omitempty"`
	JenkinsTargets   []jenkinsTarget           `json:"jenkins_targets,omitempty"`
	HarborTargets    []harborTarget            `json:"harbor_targets,omitempty"`
	DashboardTargets []dashboardTarget         `json:"dashboard_targets,omitempty"`
	ServiceBundles   []serviceBundle           `json:"service_bundles,omitempty"`
	PythonTasks      []pythonTaskV6            `json:"python_tasks,omitempty"`
}

func migrateConfig(path string) error {
	return withConfigLock(func() error {
		file, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return errors.New("settings unavailable")
		}
		data, err := io.ReadAll(io.LimitReader(file, maxConfigSize+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil || len(data) > maxConfigSize {
			return errors.New("settings unavailable")
		}
		var header struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(data, &header) != nil {
			return errors.New("settings invalid")
		}
		if header.Version == configVersion {
			var cfg config
			if decodeConfig(data, &cfg) != nil || validateConfig(cfg) != nil || validateNamedSecretReferenceCoverage(cfg) != nil {
				return errors.New("settings invalid")
			}
			return nil
		}
		if header.Version == 7 {
			cfg, err := migrateConfigV7(data)
			if err != nil {
				return err
			}
			return writeConfig(path, cfg)
		}
		if header.Version == 8 {
			cfg, err := migrateConfigV8(data)
			if err != nil {
				return err
			}
			return writeConfig(path, cfg)
		}
		if header.Version == 6 {
			var old configV6
			if decodeConfig(data, &old) != nil || old.Version != 6 {
				return errors.New("version 6 settings invalid")
			}
			cfg := config{
				Version: configVersion, ConnectionTests: old.ConnectionTests,
				GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
				HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
				ServiceBundles: old.ServiceBundles,
			}
			for _, task := range old.PythonTasks {
				cfg.PythonTasks = append(cfg.PythonTasks, pythonTask{
					ID: task.ID, Name: task.Name, InterpreterPath: task.InterpreterPath,
					ScriptPath: task.ScriptPath, Disabled: task.Disabled,
					SecretEnvName: task.SecretEnvName, SecretRef: task.SecretRef,
				})
			}
			if validateConfig(cfg) != nil {
				return errors.New("version 6 settings invalid")
			}
			return writeConfig(path, cfg)
		}
		if header.Version == 5 {
			var old configV5
			if decodeConfig(data, &old) != nil || old.Version != 5 {
				return errors.New("version 5 settings invalid")
			}
			cfg := config{
				Version: configVersion, ConnectionTests: old.ConnectionTests,
				GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
				HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
				ServiceBundles: old.ServiceBundles,
			}
			for _, task := range old.PythonTasks {
				cfg.PythonTasks = append(cfg.PythonTasks, pythonTask{
					ID: task.ID, Name: task.Name, InterpreterPath: task.InterpreterPath,
					ScriptPath: task.ScriptPath, Disabled: task.Disabled,
				})
			}
			if validateConfig(cfg) != nil {
				return errors.New("version 5 settings invalid")
			}
			return writeConfig(path, cfg)
		}
		if header.Version == 4 {
			var old configV4
			if decodeConfig(data, &old) != nil || old.Version != 4 {
				return errors.New("version 4 settings invalid")
			}
			cfg := config{
				Version: configVersion, ConnectionTests: old.ConnectionTests,
				GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
				HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets,
				ServiceBundles: old.ServiceBundles,
			}
			if validateConfig(cfg) != nil {
				return errors.New("version 4 settings invalid")
			}
			return writeConfig(path, cfg)
		}
		if header.Version == 3 {
			var old configV3
			if decodeConfig(data, &old) != nil || old.Version != 3 {
				return errors.New("version 3 settings invalid")
			}
			cfg := config{
				Version: configVersion, GitHubTargets: old.GitHubTargets, JenkinsTargets: old.JenkinsTargets,
				HarborTargets: old.HarborTargets, DashboardTargets: old.DashboardTargets, ServiceBundles: old.ServiceBundles,
			}
			if validateConfig(cfg) != nil {
				return errors.New("version 3 settings invalid")
			}
			return writeConfig(path, cfg)
		}
		if header.Version == 2 {
			var old configV2
			if decodeConfig(data, &old) != nil || old.Version != 2 {
				return errors.New("version 2 settings invalid")
			}
			cfg := config{
				Version:          configVersion,
				GitHubTargets:    old.GitHubTargets,
				JenkinsTargets:   old.JenkinsTargets,
				HarborTargets:    old.HarborTargets,
				DashboardTargets: old.DashboardTargets,
			}
			if validateConfig(cfg) != nil {
				return errors.New("version 2 settings invalid")
			}
			return writeConfig(path, cfg)
		}
		if header.Version != legacyConfigVersion {
			return errors.New("unsupported settings version")
		}
		var old legacyConfig
		if decodeConfig(data, &old) != nil || old.Version != legacyConfigVersion || (old.Target == nil && old.Jenkins == nil) {
			return errors.New("legacy settings invalid")
		}
		cfg := config{Version: configVersion}
		if old.Target != nil {
			t := *old.Target
			if t.ID == "" {
				id, err := newTargetID("github")
				if err != nil {
					return err
				}
				t.ID = id
			}
			cfg.GitHubTargets = append(cfg.GitHubTargets, t)
		}
		if old.Jenkins != nil {
			j := *old.Jenkins
			if j.ID == "" {
				id, err := newTargetID("jenkins")
				if err != nil {
					return err
				}
				j.ID = id
			}
			cfg.JenkinsTargets = append(cfg.JenkinsTargets, j)
		}
		return writeConfig(path, cfg)
	})
}

func writeConfig(path string, cfg config) error {
	if len(cfg.GitHubTargets) == 0 && cfg.Target != nil {
		cfg.GitHubTargets = []target{*cfg.Target}
	}
	if len(cfg.JenkinsTargets) == 0 && cfg.Jenkins != nil {
		cfg.JenkinsTargets = []jenkinsTarget{*cfg.Jenkins}
	}
	for i := range cfg.GitHubTargets {
		if cfg.GitHubTargets[i].ID == "" {
			id, err := newTargetID("github")
			if err != nil {
				return err
			}
			cfg.GitHubTargets[i].ID = id
		}
	}
	for i := range cfg.JenkinsTargets {
		if cfg.JenkinsTargets[i].ID == "" {
			id, err := newTargetID("jenkins")
			if err != nil {
				return err
			}
			cfg.JenkinsTargets[i].ID = id
		}
	}
	if cfg.Target != nil && len(cfg.GitHubTargets) == 1 {
		*cfg.Target = cfg.GitHubTargets[0]
	}
	if cfg.Jenkins != nil && len(cfg.JenkinsTargets) == 1 {
		*cfg.Jenkins = cfg.JenkinsTargets[0]
	}
	rebindRotatedNamedSecret(path, &cfg)
	if err := ensureNamedSecretMetadata(&cfg); err != nil {
		return err
	}
	if validateConfig(cfg) != nil || validateNamedSecretReferenceCoverage(cfg) != nil {
		return errors.New("invalid settings")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return err
	}
	if err := json.NewEncoder(file).Encode(cfg); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return replaceFile(temp, path)
}

func secretStoreError() string {
	if !credentialStoreAvailable() {
		return "Saving secrets is available only on Windows with Credential Manager. No secret was saved."
	}
	return "Could not save the credential in Windows Credential Manager. Settings were not changed."
}

func openBrowser(address string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "windows":
		command, args = "rundll32", []string{"url.dll,FileProtocolHandler", address}
	case "darwin":
		command, args = "open", []string{address}
	default:
		command, args = "xdg-open", []string{address}
	}
	return exec.Command(command, args...).Start()
}
