package main

import (
	"context"
	"errors"
	"time"
)

// Onboarding drafts are ordinary, unregistered new-target candidates. They are
// separate from config targets and the short-lived AI setup-draft review queue.
type onboardingServiceKind string

const (
	onboardingServiceGitHub    onboardingServiceKind = "github"
	onboardingServiceJenkins   onboardingServiceKind = "jenkins"
	onboardingServiceDashboard onboardingServiceKind = "dashboard"
	// Harbor is supported for observed-read receipts only in this onboarding
	// wave; no ordinary Harbor draft or activation flow is exposed.
	onboardingServiceHarbor onboardingServiceKind = "harbor"
)

// onboardingDraftFields contains only normalized, non-secret candidate values.
// It deliberately has no raw URL, query, credential, credential reference, or
// named-secret ID field.
type onboardingDraftFields struct {
	Name        string `json:"name"`
	Origin      string `json:"origin,omitempty"`
	Repository  string `json:"repository,omitempty"`
	BaseURL     string `json:"base_url,omitempty"`
	Username    string `json:"username,omitempty"`
	JobPath     string `json:"job_path,omitempty"`
	Environment string `json:"environment,omitempty"`
}

type onboardingCheckOutcome string

const (
	onboardingCheckSucceeded onboardingCheckOutcome = "success"
	onboardingCheckFailed    onboardingCheckOutcome = "failure"
)

// FailureKind uses the existing fixed connection diagnostic categories. It
// must never contain an error string, response body, URL, or credential.
type onboardingCheckReceipt struct {
	DraftID         string `json:"draft_id"`
	DraftRevision   uint64 `json:"draft_revision"`
	CheckGeneration uint64 `json:"check_generation"`
	CandidateDigest string `json:"candidate_digest"`
	PolicyDigest    string `json:"policy_digest"`
	// ConfigRevision binds this persisted informational receipt to the exact
	// current configuration snapshot. PolicyDigest remains the fixed service
	// policy digest and must never be overloaded with configuration state.
	ConfigRevision string                 `json:"config_revision"`
	Outcome        onboardingCheckOutcome `json:"outcome"`
	FailureKind    connectionFailureKind  `json:"failure_kind,omitempty"`
	CheckedAt      time.Time              `json:"checked_at"`
}

// onboardingCheckResult is transient. Permit is process-local and must never
// be copied into the durable receipt, draft store, logs, or activation record.
type onboardingCheckResult struct {
	Receipt onboardingCheckReceipt `json:"receipt"`
	Permit  string                 `json:"check_permit"`
}

type onboardingDraft struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
	// ActivationRetry is true only in the transient draft passed to apply when
	// WithActivation is reusing a previously persisted pending reservation.
	// Retry callbacks must inspect config read-only and never create a new
	// secret; the exact committed target may still be finalized safely.
	ActivationRetry bool `json:"-"`
	// ActivationRecoveryRequired is derived from a durable pending activation
	// and is exposed to local UI responses only. It must never be serialized
	// into the sidecar; the pending activation is authoritative.
	ActivationRecoveryRequired bool `json:"-"`
	// CheckGeneration advances for each explicit temporary check without
	// changing the user-authored field revision.
	CheckGeneration uint64                  `json:"check_generation"`
	Service         onboardingServiceKind   `json:"service"`
	Fields          onboardingDraftFields   `json:"fields"`
	CreatedAt       time.Time               `json:"created_at"`
	UpdatedAt       time.Time               `json:"updated_at"`
	Check           *onboardingCheckReceipt `json:"check,omitempty"`
}

type onboardingDraftSaveRequest struct {
	ID               string
	ExpectedRevision uint64
	Service          onboardingServiceKind
	Fields           onboardingDraftFields
}

// onboardingCheckRequest and onboardingActivationRequest are transient
// in-memory commands. Credential bytes and NamedSecretID must never be stored,
// logged, or copied to a response or persistent envelope.
type onboardingCheckRequest struct {
	DraftID          string
	ExpectedRevision uint64
	Credential       []byte
	NamedSecretID    string
}

type onboardingCredentialSource string

const (
	onboardingCredentialTemporary onboardingCredentialSource = "temporary"
	onboardingCredentialNamed     onboardingCredentialSource = "named_secret"
	onboardingCheckPermitTTL                                 = 5 * time.Minute
)

// onboardingCheckProofInput is used only in memory by the check and activate
// paths. The registry must store only a process-keyed HMAC/fingerprint, never
// Credential, NamedSecretID, or metadata in a persistent file or log.
type onboardingCheckProofInput struct {
	DraftID              string
	DraftRevision        uint64
	CheckGeneration      uint64
	CandidateDigest      string
	PolicyDigest         string
	ConfigRevision       string
	Credential           []byte
	CredentialSource     onboardingCredentialSource
	NamedSecretID        string
	SecretMetadataDigest string
}

// onboardingCheckProofRegistry is shared by the checker and activator for the
// lifetime of one app controller. BeginDraftCheck advances the per-draft
// generation monotonically and revokes permits from older checks. Issue
// returns a random opaque short-lived permit; Issue and Validate require the
// current generation and bind every input field plus exact credential bytes
// using a process-random HMAC key. Restart loses all permits and requires a
// fresh check. InvalidateDraft revokes all permits after draft mutation.
type onboardingCheckProofRegistry interface {
	BeginDraftCheck(string, uint64) error
	Issue(onboardingCheckProofInput, time.Time) (string, error)
	Validate(string, onboardingCheckProofInput, time.Time) error
	InvalidateDraft(string)
}

type onboardingPolicyReview struct {
	DraftID         string                `json:"draft_id"`
	DraftRevision   uint64                `json:"draft_revision"`
	CheckGeneration uint64                `json:"check_generation"`
	Service         onboardingServiceKind `json:"service"`
	CandidateDigest string                `json:"candidate_digest"`
	PolicyDigest    string                `json:"policy_digest"`
	ConfigRevision  string                `json:"config_revision"`
	IncludedTools   []string              `json:"included_tools"`
	// ExcludedTools is reserved for tools permanently unavailable in this
	// target's fixed service scope. Empty does not imply every service tool is
	// directly available; mapping and per-call approval states are separate.
	ExcludedTools   []string `json:"excluded_tools"`
	MappingRequired []string `json:"mapping_required"`
	PolicyNotes     []string `json:"policy_notes"`
}

type onboardingActivationCAS struct {
	DraftID          string
	ExpectedRevision uint64
	CheckGeneration  uint64
	CandidateDigest  string
	PolicyDigest     string
	ConfigRevision   string
}

type onboardingActivationRequest struct {
	CAS             onboardingActivationCAS
	PolicyConfirmed bool
	CheckPermit     string
	Credential      []byte
	NamedSecretID   string
}

type onboardingActivationRecord struct {
	DraftID         string                `json:"draft_id"`
	DraftRevision   uint64                `json:"draft_revision"`
	CheckGeneration uint64                `json:"check_generation"`
	TargetID        string                `json:"target_id"`
	Service         onboardingServiceKind `json:"service"`
	CandidateDigest string                `json:"candidate_digest"`
	PolicyDigest    string                `json:"policy_digest"`
	ConfigRevision  string                `json:"config_revision"`
	ActivatedAt     time.Time             `json:"activated_at"`
}

const onboardingFirstReadSource = "mcp_observed"

// onboardingFirstReadReceipt omits target IDs and names. DefinitionDigest is
// the required digest of the exact safe target definition; no definition data
// or external response is persisted. Readiness uses the digest to match the
// observation to a current active target.
type onboardingFirstReadReceipt struct {
	Source           string                `json:"source"`
	Service          onboardingServiceKind `json:"service"`
	Tool             string                `json:"tool"`
	ObservedAt       time.Time             `json:"observed_at"`
	DefinitionDigest string                `json:"definition_digest"`
}

// onboardingDraftStore is the single cross-process-locked sidecar boundary for
// resumable drafts, their latest check receipt, activation recovery records,
// and observed first reads. Implementations must not read config, touch secrets,
// perform network I/O, or acquire the config lock. WithActivation holds the
// sidecar lock, durably reserves/reuses one target ID before apply, and invokes
// apply under the documented lock order: sidecar lock -> config lock. It keeps
// the draft on failure and removes it only after apply succeeds. Retrying a
// pending activation must pass the same target ID to an idempotent apply.
// If apply returns the explicit ambiguous-save sentinel, the store durably
// marks that pending activation as requiring manual recovery and rejects
// later check/activation attempts before invoking apply. Draft reads derive a
// transient UI flag from that pending record; it is not duplicated in storage.
// The transient ActivationRetry flag on the apply draft also identifies any
// reused pending reservation, including after a process crash or failed gate
// write. Retry callbacks must use read-only config reconciliation and cannot
// invoke a save helper unless the reservation is newly created.
// The implementation stores a versioned envelope beneath the config directory
// in an onboarding subdirectory isolated by the normalized config-path hash.
// StartCheck CAS-checks the draft revision, advances its nonzero check
// generation, and clears the prior receipt before any network or secret work.
// SaveCheckReceipt accepts only the current generation, so an older concurrent
// check cannot restore a stale result after a newer one has started.
type onboardingDraftStore interface {
	ListDrafts(context.Context) ([]onboardingDraft, error)
	GetDraft(context.Context, string) (onboardingDraft, error)
	CreateDraft(context.Context, onboardingServiceKind, onboardingDraftFields) (onboardingDraft, error)
	UpdateDraft(context.Context, string, uint64, onboardingDraftFields) (onboardingDraft, error)
	DeleteDraft(context.Context, string, uint64) error
	StartCheck(context.Context, string, uint64) (uint64, error)
	SaveCheckReceipt(context.Context, onboardingCheckReceipt) error
	WithActivation(
		context.Context,
		onboardingActivationCAS,
		func(onboardingDraft, string) error,
	) (onboardingActivationRecord, error)
	RecordFirstRead(context.Context, onboardingFirstReadReceipt) (bool, error)
	ListFirstReads(context.Context) ([]onboardingFirstReadReceipt, error)
}

type onboardingController interface {
	ListDrafts(context.Context) ([]onboardingDraft, error)
	GetDraft(context.Context, string) (onboardingDraft, error)
	SaveDraft(context.Context, onboardingDraftSaveRequest) (onboardingDraft, error)
	DeleteDraft(context.Context, string, uint64) error
	CheckDraft(context.Context, onboardingCheckRequest) (onboardingCheckResult, error)
	ReviewDraft(context.Context, string, uint64) (onboardingPolicyReview, error)
	ActivateDraft(context.Context, onboardingActivationRequest) (onboardingActivationRecord, error)
	RecordFirstRead(context.Context, onboardingFirstReadReceipt) (bool, error)
	ListFirstReads(context.Context) ([]onboardingFirstReadReceipt, error)
}

// The collection endpoint needs only enough information to identify and
// resume a draft. Candidate origins, repositories, job paths, and names are
// returned only by an explicit item GET.
type onboardingDraftListItemDTO struct {
	ID                         string                 `json:"id"`
	Revision                   uint64                 `json:"revision"`
	Service                    onboardingServiceKind  `json:"service"`
	CreatedAt                  time.Time              `json:"created_at"`
	UpdatedAt                  time.Time              `json:"updated_at"`
	CheckOutcome               onboardingCheckOutcome `json:"check_outcome,omitempty"`
	ActivationRecoveryRequired bool                   `json:"activation_recovery_required"`
}

type onboardingDraftListResponse struct {
	Drafts []onboardingDraftListItemDTO `json:"drafts"`
}

type onboardingDraftCreateRequestDTO struct {
	Service onboardingServiceKind `json:"service"`
	Fields  onboardingDraftFields `json:"fields"`
}

type onboardingDraftUpdateRequestDTO struct {
	ExpectedRevision uint64                `json:"expected_revision"`
	Fields           onboardingDraftFields `json:"fields"`
}

type onboardingDraftDeleteRequestDTO struct {
	ExpectedRevision uint64 `json:"expected_revision"`
}

type onboardingDraftCheckRequestDTO struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	Credential       string `json:"credential,omitempty"`
	NamedSecretID    string `json:"named_secret_id,omitempty"`
}

type onboardingDraftCheckResponseDTO struct {
	Receipt onboardingCheckReceipt `json:"receipt"`
	Permit  string                 `json:"check_permit"`
}

type onboardingPolicyReviewRequestDTO struct {
	ExpectedRevision uint64 `json:"expected_revision"`
}

type onboardingDraftActivateRequestDTO struct {
	ExpectedRevision uint64 `json:"expected_revision"`
	CheckGeneration  uint64 `json:"check_generation"`
	CandidateDigest  string `json:"candidate_digest"`
	PolicyDigest     string `json:"policy_digest"`
	ConfigRevision   string `json:"config_revision"`
	PolicyConfirmed  bool   `json:"policy_confirmed"`
	CheckPermit      string `json:"check_permit"`
	Credential       string `json:"credential,omitempty"`
	NamedSecretID    string `json:"named_secret_id,omitempty"`
}

type onboardingFirstReadListResponse struct {
	Receipts []onboardingFirstReadReceipt `json:"receipts"`
}

var (
	errOnboardingUnavailable                = errors.New("registration onboarding is unavailable")
	errOnboardingInvalid                    = errors.New("registration onboarding request is invalid")
	errOnboardingDraftNotFound              = errors.New("registration draft was not found")
	errOnboardingDraftStale                 = errors.New("registration draft changed; reload and review it again")
	errOnboardingCheckRequired              = errors.New("a current successful connection check is required")
	errOnboardingCheckPermit                = errors.New("a fresh credential-bound check is required")
	errOnboardingReviewChanged              = errors.New("the reviewed candidate or fixed policy changed")
	errOnboardingActivationPending          = errors.New("registration activation is being recovered; retry after reviewing the target")
	errOnboardingActivationRecoveryRequired = errors.New("registration activation requires manual recovery")
)
