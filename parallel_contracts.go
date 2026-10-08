package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"sort"
	"strings"
	"time"
)

const (
	parallelContractRevision     = "a00-r1"
	gatewayCredentialTargetName  = "LocalAgentHarness/MCPGateway/token-v1"
	uiFragmentTemplateNamePrefix = "lah-ui-fragment-"
	setupDraftDefaultLifetime    = 10 * time.Minute
	setupDraftMaximumLifetime    = 30 * time.Minute

	uiRouteSaveNamedSecret       = "/save-named-secret"
	uiRouteDeleteNamedSecret     = "/delete-named-secret"
	uiRouteSaveUserEnvironment   = "/save-user-environment"
	uiRouteDeleteUserEnvironment = "/delete-user-environment"
	uiRouteSaveSSHTarget         = "/save-ssh-target"
	uiRouteDeleteSSHTarget       = "/delete-ssh-target"
	uiRouteSaveSSHOperation      = "/save-ssh-operation"
	uiRouteDeleteSSHOperation    = "/delete-ssh-operation"
	uiRouteReviewSetupDraft      = "/review-setup-draft"
	uiRouteApproveSetupDraft     = "/approve-setup-draft"
	uiRouteRejectSetupDraft      = "/reject-setup-draft"
	uiRouteRuntimeStatus         = "/runtime-status"
	uiRouteSetLoginStartup       = "/set-login-startup"
	uiRouteClientPlan            = "/client-registration-plan"
	uiRouteClientBackup          = "/client-registration-backup"
	uiRouteClientApply           = "/client-registration-apply"
	uiRouteClientVerify          = "/client-registration-verify"
	uiRouteClientRestore         = "/client-registration-restore"

	mcpToolRegisteredSSHTargets = "registered_ssh_targets"
	mcpToolRunSSHOperation      = "run_ssh_operation"
	mcpToolSubmitSetupDraft     = "submit_setup_draft"
)

// NamedSecretView is safe to return to the local UI. It deliberately contains
// neither the secret value nor its Credential Manager reference.
type NamedSecretView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Purpose    string `json:"purpose,omitempty"`
	Configured bool   `json:"configured"`
	InUse      bool   `json:"in_use"`
}

// NamedSecretWrite is an internal UI-to-controller request. Value is never
// included when this type is serialized.
type NamedSecretWrite struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Purpose string `json:"purpose,omitempty"`
	Value   []byte `json:"-"`
}

type NamedSecretController interface {
	ListNamedSecrets(context.Context) ([]NamedSecretView, error)
	SaveNamedSecret(context.Context, NamedSecretWrite) (NamedSecretView, error)
	DeleteNamedSecret(context.Context, string) error
}

// UserEnvironmentEntry exposes only a variable name and whether it is set.
type UserEnvironmentEntry struct {
	Name       string `json:"name"`
	Configured bool   `json:"configured"`
}

// UserEnvironmentWrite carries a value only into the current-user backend.
type UserEnvironmentWrite struct {
	Name  string `json:"name"`
	Value []byte `json:"-"`
}

type UserEnvironmentResult struct {
	Name               string `json:"name"`
	Operation          string `json:"operation"`
	Applied            bool   `json:"applied"`
	RequiresNewSession bool   `json:"requires_new_session"`
	NextAction         string `json:"next_action,omitempty"`
}

type UserEnvironmentController interface {
	ListUserEnvironment(context.Context) ([]UserEnvironmentEntry, error)
	SetUserEnvironment(context.Context, UserEnvironmentWrite) (UserEnvironmentResult, error)
	DeleteUserEnvironment(context.Context, string) (UserEnvironmentResult, error)
}

// userEnvironmentValueStore is an internal backend seam. Current values may
// be read for preserving an update, but must never flow into public DTOs.
type userEnvironmentValueStore interface {
	ReadForUpdate(string) ([]byte, bool, error)
	SetCurrentUserValue(string, []byte) error
	DeleteCurrentUserValue(string) error
}

type SSHRiskClass string

const (
	SSHRiskReadOnly      SSHRiskClass = "read_only"
	SSHRiskStateChanging SSHRiskClass = "state_changing"
	SSHRiskDestructive   SSHRiskClass = "destructive"
)

type SetupDraftAction string

const (
	SetupDraftAdd    SetupDraftAction = "add"
	SetupDraftUpdate SetupDraftAction = "update"
	SetupDraftReuse  SetupDraftAction = "reuse"
)

type SetupServiceKind string

const (
	SetupServiceGitHub    SetupServiceKind = "github"
	SetupServiceJenkins   SetupServiceKind = "jenkins"
	SetupServiceHarbor    SetupServiceKind = "harbor"
	SetupServiceDashboard SetupServiceKind = "dashboard"
)

type OperationApprovalMode string

const (
	OperationApprovalReadOnly    OperationApprovalMode = "read_only"
	OperationApprovalPreapproved OperationApprovalMode = "preapproved_exact_scope"
	OperationApprovalPerCall     OperationApprovalMode = "per_call_confirmation"
)

type OperationApprovalPolicy struct {
	Mode     OperationApprovalMode `json:"mode"`
	Revision string                `json:"revision,omitempty"`
	Scope    []string              `json:"scope"`
}

type SSHParameterSpec struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required"`
	Description string `json:"description,omitempty"`
}

// SSHOperationDefinition is saved configuration reviewed in the local UI.
// MCP execution requests use SSHOperationInput and cannot supply a command.
type SSHOperationDefinition struct {
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	Summary    string                  `json:"summary"`
	Program    string                  `json:"program"`
	FixedArgs  []string                `json:"fixed_args"`
	Parameters []SSHParameterSpec      `json:"parameters"`
	Risk       SSHRiskClass            `json:"risk"`
	Approval   OperationApprovalPolicy `json:"approval"`
	Revision   string                  `json:"revision"`
}

type SSHOperationView struct {
	ID         string                  `json:"id"`
	Name       string                  `json:"name"`
	Summary    string                  `json:"summary"`
	Parameters []SSHParameterSpec      `json:"parameters"`
	Risk       SSHRiskClass            `json:"risk"`
	Approval   OperationApprovalPolicy `json:"approval"`
	Revision   string                  `json:"revision"`
}

type SSHTargetView struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Alias      string             `json:"alias"`
	Operations []SSHOperationView `json:"operations"`
}

// SSHOperationCatalogTarget is safe for MCP catalog output; it omits the
// OpenSSH alias and reviewed program/arguments shown only in the local UI.
type SSHOperationCatalogTarget struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Operations []SSHOperationView `json:"operations"`
}

// SSHOperationInput is restricted to saved target/task identifiers and
// schema-validated parameters; it has no host, credential, or command field.
type SSHOperationInput struct {
	TargetID    string                     `json:"target_id"`
	OperationID string                     `json:"operation_id"`
	Parameters  map[string]json.RawMessage `json:"parameters"`
}

type SSHRemoteOutcome string

const (
	SSHRemoteOutcomeConfirmed SSHRemoteOutcome = "confirmed"
	SSHRemoteOutcomeUnknown   SSHRemoteOutcome = "unknown"
	SSHRemoteOutcomeNotNeeded SSHRemoteOutcome = "not_needed"
)

type SSHOperationResult struct {
	Status          string           `json:"status"`
	Output          string           `json:"output,omitempty"`
	OutputTruncated bool             `json:"output_truncated"`
	RemoteOutcome   SSHRemoteOutcome `json:"remote_outcome"`
	NextAction      string           `json:"next_action,omitempty"`
}

type SSHOperationController interface {
	ListSSHTargets(context.Context) ([]SSHTargetView, error)
	ListSSHOperationCatalog(context.Context) ([]SSHOperationCatalogTarget, error)
	RunSSHOperation(context.Context, SSHOperationInput) (SSHOperationResult, error)
}

type SetupDraftSource struct {
	Kind       string `json:"kind"`
	Label      string `json:"label"`
	Confidence string `json:"confidence"`
}

type SetupDraftConnection struct {
	Kind           SetupServiceKind `json:"kind"`
	Action         SetupDraftAction `json:"action"`
	ExistingID     string           `json:"existing_id,omitempty"`
	Name           string           `json:"name"`
	Origin         string           `json:"origin,omitempty"`
	Repository     string           `json:"repository,omitempty"`
	Username       string           `json:"username,omitempty"`
	Project        string           `json:"project,omitempty"`
	JobPath        string           `json:"job_path,omitempty"`
	Environment    string           `json:"environment,omitempty"`
	Namespace      string           `json:"namespace,omitempty"`
	BaseURL        string           `json:"base_url,omitempty"`
	CredentialName string           `json:"credential_name,omitempty"`
}

type SetupDraftEnvironment struct {
	Name                string `json:"name"`
	JenkinsTargetID     string `json:"jenkins_target_id,omitempty"`
	HarborTargetID      string `json:"harbor_target_id,omitempty"`
	DashboardTargetID   string `json:"dashboard_target_id,omitempty"`
	DashboardNamespace  string `json:"dashboard_namespace,omitempty"`
	DashboardDeployment string `json:"dashboard_deployment,omitempty"`
}

type SetupDraftBundle struct {
	Name          string                  `json:"name"`
	RepositoryIDs []string                `json:"repository_ids"`
	Environments  []SetupDraftEnvironment `json:"environments"`
}

type SetupDraftSSHTarget struct {
	Name       string                   `json:"name"`
	Alias      string                   `json:"alias"`
	Operations []SSHOperationDefinition `json:"operations"`
}

// SetupDraftSubmission contains only proposed settings metadata. Secret values,
// Credential Manager references, and source conversation/document bodies have
// no fields in this contract.
type SetupDraftSubmission struct {
	BaseRevision string                 `json:"base_revision"`
	Sources      []SetupDraftSource     `json:"sources"`
	Missing      []string               `json:"missing"`
	Connections  []SetupDraftConnection `json:"connections"`
	Bundles      []SetupDraftBundle     `json:"bundles"`
	SSHTargets   []SetupDraftSSHTarget  `json:"ssh_targets"`
}

type SetupDraftChange struct {
	Kind               string   `json:"kind"`
	Name               string   `json:"name"`
	Summary            string   `json:"summary"`
	Scope              []string `json:"scope"`
	PermissionIncrease []string `json:"permission_increase"`
}

type SetupDraftReview struct {
	ID           string               `json:"id"`
	BaseRevision string               `json:"base_revision"`
	Digest       string               `json:"digest"`
	ExpiresAt    time.Time            `json:"expires_at"`
	Sources      []SetupDraftSource   `json:"sources"`
	Missing      []string             `json:"missing"`
	Changes      []SetupDraftChange   `json:"changes"`
	Proposal     SetupDraftSubmission `json:"proposal"`
}

type SetupDraftApprovalResult struct {
	Applied    bool   `json:"applied"`
	Stale      bool   `json:"stale"`
	NextAction string `json:"next_action,omitempty"`
}

// SetupDraftController owns one-shot expiry and compare-and-swap semantics.
// Approval must atomically check the current config revision and consume the
// reviewed draft only when the accepted change is saved.
type SetupDraftController interface {
	SubmitSetupDraft(context.Context, SetupDraftSubmission) (SetupDraftReview, error)
	ReviewSetupDraft(context.Context, string, string, string) (SetupDraftReview, error)
	ApproveSetupDraft(context.Context, string, string, string) (SetupDraftApprovalResult, error)
	RejectSetupDraft(context.Context, string, string, string) error
}

type MCPTransport string

const (
	MCPTransportStdio           MCPTransport = "stdio"
	MCPTransportStreamableHTTP  MCPTransport = "streamable_http"
	MCPTransportStdioHTTPBridge MCPTransport = "stdio_http_bridge"
)

type MCPRegistrationSpec struct {
	ClientID    string       `json:"client_id"`
	DisplayName string       `json:"display_name"`
	Transport   MCPTransport `json:"transport"`
	Endpoint    string       `json:"endpoint,omitempty"`
	Command     string       `json:"command,omitempty"`
	Args        []string     `json:"args"`
	Scope       []string     `json:"scope"`
}

type MCPRuntimeStatus struct {
	State       string    `json:"state"`
	UIEndpoint  string    `json:"ui_endpoint,omitempty"`
	MCPEndpoint string    `json:"mcp_endpoint,omitempty"`
	ChangedAt   time.Time `json:"changed_at,omitempty"`
	FailureCode string    `json:"failure_code,omitempty"`
}

type MCPClientStatus struct {
	ClientID       string `json:"client_id"`
	Installed      string `json:"installed"`
	Version        string `json:"version,omitempty"`
	Registration   string `json:"registration"`
	Connection     string `json:"connection"`
	ToolCall       string `json:"tool_call"`
	Backup         string `json:"backup"`
	EvidenceSource string `json:"evidence_source"`
}

type MCPClientBackupReceipt struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type MCPClientRegistrationPlan struct {
	ID           string   `json:"id"`
	ClientID     string   `json:"client_id"`
	BaseRevision string   `json:"base_revision"`
	Changes      []string `json:"changes"`
}

type MCPRuntimeController interface {
	Status(context.Context) (MCPRuntimeStatus, error)
	Start(context.Context) error
	Stop(context.Context) error
}

// MCPClientRegistrationController applies a plan only after a current-user
// backup. Apply takes a plan ID and base revision; restore takes an opaque
// backup receipt ID, never backup contents.
type MCPClientRegistrationController interface {
	InspectClient(context.Context, string) (MCPClientStatus, error)
	PlanClientRegistration(context.Context, MCPRegistrationSpec) (MCPClientRegistrationPlan, error)
	BackupClientRegistration(context.Context, string) (MCPClientBackupReceipt, error)
	ApplyClientRegistration(context.Context, string, string) (MCPClientStatus, error)
	VerifyClientRegistration(context.Context, string) (MCPClientStatus, error)
	RestoreClientRegistration(context.Context, string, string) (MCPClientStatus, error)
}

// gatewayTokenCredentialProvider resolves one application-owned, deterministic
// Credential Manager target. The target name is not part of config or DTOs.
type gatewayTokenCredentialProvider interface {
	LoadGatewayToken() ([]byte, error)
	SaveGatewayToken([]byte) error
	DeleteGatewayToken() error
}

// parseUIPage includes only templates embedded with the application. Partial
// markup is rendered by html/template before it is marked trusted for insertion.
func parseUIPage(files fs.FS) (*template.Template, error) {
	var parsed *template.Template
	functions := template.FuncMap{
		"renderUIFragments": func(data any) (template.HTML, error) {
			if parsed == nil {
				return "", errors.New("UI templates are not ready")
			}
			var output bytes.Buffer
			for _, name := range uiFragmentTemplateNames(parsed) {
				if err := parsed.ExecuteTemplate(&output, name, data); err != nil {
					return "", errors.New("UI fragment could not be rendered")
				}
			}
			return template.HTML(output.String()), nil
		},
	}
	parsed, err := template.New("ui.html").Funcs(functions).ParseFS(files, "ui.html", "ui_fragments/*.html")
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

func mustParseUIPage(files embed.FS) *template.Template {
	parsed, err := parseUIPage(files)
	if err != nil {
		panic(err)
	}
	return parsed
}

func uiFragmentTemplateNames(parsed *template.Template) []string {
	if parsed == nil {
		return []string{}
	}
	var names []string
	for _, candidate := range parsed.Templates() {
		if strings.HasPrefix(candidate.Name(), uiFragmentTemplateNamePrefix) {
			names = append(names, candidate.Name())
		}
	}
	sort.Strings(names)
	return names
}
