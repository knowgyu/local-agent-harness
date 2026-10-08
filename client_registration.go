package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	defaultClientRegistrationPlanTTL = 10 * time.Minute
	maxClientRegistrationPlanTTL     = 30 * time.Minute
	maxClientRegistrationConfigBytes = 4 << 20

	mcpClientIDCodex  = "codex"
	mcpClientIDClaude = "claude"
	mcpClientIDGemini = "gemini"

	mcpClientRegistrationDisplayName = "local-agent-harness"

	mcpRegistrationChangeAdded     = "registration_added"
	mcpRegistrationChangeUnchanged = "registration_unchanged"

	mcpClientInstalled                 = "installed"
	mcpClientInstalledNotObserved      = "installed_cli_not_observed"
	mcpClientRegistrationRegistered    = "registered"
	mcpClientRegistrationNotRegistered = "not_registered"
	mcpClientRegistrationConflict      = "conflict"
	mcpClientRegistrationNotObserved   = "not_observed"
	mcpClientConnectionUnverified      = "unverified"
	mcpClientBackupNotCreated          = "not_created"
	mcpClientBackupCreated             = "created"
	mcpClientBackupApplying            = "applying"
	mcpClientBackupApplied             = "applied"
	mcpClientBackupRestored            = "restored"
	mcpClientBackupStale               = "stale"
	mcpClientEvidenceNotObserved       = "not_observed"
	mcpClientEvidenceConfigObserved    = "config_observed"
)

var (
	errClientRegistrationClientUnavailable    = errors.New("client registration is unavailable")
	errClientRegistrationInvalidSpec          = errors.New("client registration details are invalid")
	errClientRegistrationUnsupportedTransport = errors.New("client registration transport is unsupported")
	errClientRegistrationNameConflict         = errors.New("a different registration already uses this name")
	errClientRegistrationPlanExpired          = errors.New("client registration plan has expired")
	errClientRegistrationStaleSettings        = errors.New("client settings changed; create a new plan and backup")
	errClientRegistrationBackupRequired       = errors.New("create a backup before applying this registration")
	errClientRegistrationBackupNotFound       = errors.New("client registration backup is unavailable")
	errClientRegistrationBackupStale          = errors.New("client settings changed after registration; restore was not applied")
	errClientRegistrationConfigTooLarge       = errors.New("client settings exceed the supported size")
	errClientRegistrationInspectionFailed     = errors.New("client registration could not be inspected")
	errClientRegistrationPlanFailed           = errors.New("client registration plan could not be created")
	errClientRegistrationBackupFailed         = errors.New("client registration backup could not be saved")
	errClientRegistrationWriteFailed          = errors.New("client registration could not be applied")
	errClientRegistrationVerificationFailed   = errors.New("client registration could not be verified")
	errClientRegistrationRestoreFailed        = errors.New("client registration backup could not be restored")
	errClientRegistrationControllerOptions    = errors.New("client registration controller options are invalid")
)

var (
	clientRegistrationDisplayNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	clientRegistrationVersionPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,63}$`)
	clientRegistrationIDPattern          = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

type clientRegistrationConfig struct {
	exists bool
	bytes  []byte
}

type clientRegistrationSnapshot struct {
	status MCPClientStatus
	config clientRegistrationConfig
}

type clientRegistrationMutation struct {
	config  clientRegistrationConfig
	changes []string
}

type clientRegistrationAdapter interface {
	clientID() string
	inspect(context.Context) (clientRegistrationSnapshot, error)
	plan(context.Context, MCPRegistrationSpec, clientRegistrationConfig) (clientRegistrationMutation, error)
	writeConfig(context.Context, clientRegistrationConfig, clientRegistrationConfig) error
	verify(context.Context) (MCPClientStatus, error)
}

type clientRegistrationDigest [sha256.Size]byte

type clientRegistrationBackupRecord struct {
	clientID       string
	receipt        MCPClientBackupReceipt
	original       clientRegistrationConfig
	originalDigest clientRegistrationDigest
	applied        clientRegistrationConfig
	appliedDigest  clientRegistrationDigest
	hasApplied     bool
	restored       bool
}

type clientRegistrationBackupStore interface {
	save(context.Context, string, clientRegistrationConfig, clientRegistrationDigest) (MCPClientBackupReceipt, error)
	load(context.Context, string, string) (clientRegistrationBackupRecord, error)
	findLatest(context.Context, string, clientRegistrationDigest) (clientRegistrationBackupRecord, error)
	prepareApply(context.Context, string, string, clientRegistrationConfig, clientRegistrationDigest) error
	markApplied(context.Context, string, string) error
	markRestored(context.Context, string, string) error
}

type mcpClientRegistrationControllerOptions struct {
	adapters    []clientRegistrationAdapter
	backupStore clientRegistrationBackupStore
	planTTL     time.Duration
	now         func() time.Time
	newID       func() (string, error)
}

type mcpClientRegistrationController struct {
	mu          sync.Mutex
	adapters    map[string]clientRegistrationAdapter
	backupStore clientRegistrationBackupStore
	plans       map[string]clientRegistrationPlanRecord
	planTTL     time.Duration
	now         func() time.Time
	newID       func() (string, error)
}

type clientRegistrationPlanRecord struct {
	plan            MCPClientRegistrationPlan
	createdAt       time.Time
	original        clientRegistrationConfig
	originalDigest  clientRegistrationDigest
	desired         clientRegistrationConfig
	desiredDigest   clientRegistrationDigest
	backupReceiptID string
}

func newMCPClientRegistrationController(options mcpClientRegistrationControllerOptions) (*mcpClientRegistrationController, error) {
	if options.planTTL == 0 {
		options.planTTL = defaultClientRegistrationPlanTTL
	}
	if options.planTTL <= 0 || options.planTTL > maxClientRegistrationPlanTTL {
		return nil, errClientRegistrationControllerOptions
	}
	if options.now == nil {
		options.now = time.Now
	}
	if options.newID == nil {
		options.newID = newClientRegistrationOpaqueID
	}
	if options.backupStore == nil {
		store, err := newCurrentUserClientRegistrationBackupStore()
		if err != nil {
			return nil, errClientRegistrationBackupFailed
		}
		options.backupStore = store
	}
	if len(options.adapters) == 0 {
		return nil, errClientRegistrationControllerOptions
	}

	adapters := make(map[string]clientRegistrationAdapter, len(options.adapters))
	for _, adapter := range options.adapters {
		if adapter == nil {
			return nil, errClientRegistrationControllerOptions
		}
		clientID := adapter.clientID()
		if !isSupportedMCPClientID(clientID) {
			return nil, errClientRegistrationControllerOptions
		}
		if _, exists := adapters[clientID]; exists {
			return nil, errClientRegistrationControllerOptions
		}
		adapters[clientID] = adapter
	}

	return &mcpClientRegistrationController{
		adapters:    adapters,
		backupStore: options.backupStore,
		plans:       map[string]clientRegistrationPlanRecord{},
		planTTL:     options.planTTL,
		now:         options.now,
		newID:       options.newID,
	}, nil
}

func (c *mcpClientRegistrationController) InspectClient(ctx context.Context, clientID string) (MCPClientStatus, error) {
	adapter, err := c.adapterFor(clientID)
	if err != nil {
		return MCPClientStatus{}, err
	}
	snapshot, err := adapter.inspect(ctx)
	if err != nil {
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationInspectionFailed)
	}
	if !validClientRegistrationConfig(snapshot.config) {
		return MCPClientStatus{}, errClientRegistrationConfigTooLarge
	}
	if snapshot.status.Installed == mcpClientInstalledNotObserved {
		if snapshot.config.exists || len(snapshot.config.bytes) != 0 {
			return MCPClientStatus{}, errClientRegistrationInspectionFailed
		}
		return normalizeClientRegistrationStatus(snapshot.status, clientID, mcpClientBackupNotCreated, mcpClientEvidenceNotObserved), nil
	}
	return normalizeClientRegistrationStatus(snapshot.status, clientID, mcpClientBackupNotCreated, mcpClientEvidenceConfigObserved), nil
}

func (c *mcpClientRegistrationController) PlanClientRegistration(ctx context.Context, spec MCPRegistrationSpec) (MCPClientRegistrationPlan, error) {
	if err := validateMCPRegistrationSpec(spec); err != nil {
		return MCPClientRegistrationPlan{}, err
	}
	adapter, err := c.adapterFor(spec.ClientID)
	if err != nil {
		return MCPClientRegistrationPlan{}, err
	}
	snapshot, err := adapter.inspect(ctx)
	if err != nil {
		return MCPClientRegistrationPlan{}, safeClientRegistrationError(err, errClientRegistrationPlanFailed)
	}
	if snapshot.status.Installed != mcpClientInstalled || !clientRegistrationConfigWasObserved(snapshot.status) {
		return MCPClientRegistrationPlan{}, errClientRegistrationClientUnavailable
	}
	if !validClientRegistrationConfig(snapshot.config) {
		return MCPClientRegistrationPlan{}, errClientRegistrationConfigTooLarge
	}
	mutation, err := adapter.plan(ctx, spec, cloneClientRegistrationConfig(snapshot.config))
	if err != nil {
		return MCPClientRegistrationPlan{}, safeClientRegistrationError(err, errClientRegistrationPlanFailed)
	}
	if !validClientRegistrationConfig(mutation.config) {
		return MCPClientRegistrationPlan{}, errClientRegistrationConfigTooLarge
	}
	changes, err := safeClientRegistrationChanges(snapshot.config, mutation.config, mutation.changes)
	if err != nil {
		return MCPClientRegistrationPlan{}, err
	}

	planID, err := c.newOpaqueID()
	if err != nil {
		return MCPClientRegistrationPlan{}, errClientRegistrationPlanFailed
	}
	baseRevision, err := c.newOpaqueID()
	if err != nil {
		return MCPClientRegistrationPlan{}, errClientRegistrationPlanFailed
	}
	plan := MCPClientRegistrationPlan{
		ID:           planID,
		ClientID:     spec.ClientID,
		BaseRevision: baseRevision,
		Changes:      changes,
	}
	record := clientRegistrationPlanRecord{
		plan:           plan,
		createdAt:      c.now(),
		original:       cloneClientRegistrationConfig(snapshot.config),
		originalDigest: digestClientRegistrationConfig(snapshot.config),
		desired:        cloneClientRegistrationConfig(mutation.config),
		desiredDigest:  digestClientRegistrationConfig(mutation.config),
	}
	c.mu.Lock()
	c.pruneExpiredPlansLocked(c.now())
	c.plans[plan.ID] = record
	c.mu.Unlock()
	return plan, nil
}

func (c *mcpClientRegistrationController) BackupClientRegistration(ctx context.Context, clientID string) (MCPClientBackupReceipt, error) {
	adapter, err := c.adapterFor(clientID)
	if err != nil {
		return MCPClientBackupReceipt{}, err
	}
	snapshot, err := adapter.inspect(ctx)
	if err != nil {
		return MCPClientBackupReceipt{}, safeClientRegistrationError(err, errClientRegistrationBackupFailed)
	}
	if snapshot.status.Installed != mcpClientInstalled || !clientRegistrationConfigWasObserved(snapshot.status) {
		return MCPClientBackupReceipt{}, errClientRegistrationClientUnavailable
	}
	if !validClientRegistrationConfig(snapshot.config) {
		return MCPClientBackupReceipt{}, errClientRegistrationConfigTooLarge
	}
	digest := digestClientRegistrationConfig(snapshot.config)
	receipt, err := c.backupStore.save(ctx, clientID, cloneClientRegistrationConfig(snapshot.config), digest)
	if err != nil {
		return MCPClientBackupReceipt{}, safeClientRegistrationError(err, errClientRegistrationBackupFailed)
	}
	if !validClientRegistrationID(receipt.ID) || receipt.Status != mcpClientBackupCreated || receipt.CreatedAt.IsZero() {
		return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
	}
	return MCPClientBackupReceipt{
		ID:        receipt.ID,
		Status:    mcpClientBackupCreated,
		CreatedAt: receipt.CreatedAt,
	}, nil
}

func (c *mcpClientRegistrationController) ApplyClientRegistration(ctx context.Context, planID, baseRevision string) (MCPClientStatus, error) {
	if !validClientRegistrationID(planID) || !validClientRegistrationID(baseRevision) {
		return MCPClientStatus{}, errClientRegistrationPlanExpired
	}
	record, ok := c.planRecord(planID)
	if !ok {
		return MCPClientStatus{}, errClientRegistrationPlanExpired
	}
	if !constantTimeStringEqual(record.plan.BaseRevision, baseRevision) {
		return MCPClientStatus{}, errClientRegistrationStaleSettings
	}
	adapter, err := c.adapterFor(record.plan.ClientID)
	if err != nil {
		return MCPClientStatus{}, err
	}
	current, err := adapter.inspect(ctx)
	if err != nil {
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationInspectionFailed)
	}
	if current.status.Installed != mcpClientInstalled || !clientRegistrationConfigWasObserved(current.status) {
		return MCPClientStatus{}, errClientRegistrationClientUnavailable
	}
	if !validClientRegistrationConfig(current.config) {
		return MCPClientStatus{}, errClientRegistrationConfigTooLarge
	}
	currentDigest := digestClientRegistrationConfig(current.config)
	if currentDigest != record.originalDigest && currentDigest != record.desiredDigest {
		return MCPClientStatus{}, errClientRegistrationStaleSettings
	}

	backup, err := c.backupForPlan(ctx, record)
	if err != nil {
		return MCPClientStatus{}, err
	}
	if backup.originalDigest != record.originalDigest || !sameClientRegistrationConfig(backup.original, record.original) {
		return MCPClientStatus{}, errClientRegistrationBackupRequired
	}
	if backup.receipt.Status != mcpClientBackupCreated && backup.receipt.Status != mcpClientBackupApplying && backup.receipt.Status != mcpClientBackupApplied {
		return MCPClientStatus{}, errClientRegistrationBackupRequired
	}

	if backup.receipt.Status == mcpClientBackupCreated {
		if err := c.backupStore.prepareApply(ctx, backup.clientID, backup.receipt.ID, cloneClientRegistrationConfig(record.desired), record.desiredDigest); err != nil {
			return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationBackupFailed)
		}
		backup.hasApplied = true
		backup.applied = cloneClientRegistrationConfig(record.desired)
		backup.appliedDigest = record.desiredDigest
		backup.receipt.Status = mcpClientBackupApplying
	}
	c.rememberBackupReceipt(planID, backup.receipt.ID)

	if !record.desiredDigestEqualOriginal() && currentDigest == record.desiredDigest {
		if err := c.backupStore.markApplied(ctx, backup.clientID, backup.receipt.ID); err != nil {
			return MCPClientStatus{}, errClientRegistrationBackupFailed
		}
		return c.statusAfterRegistration(ctx, adapter, record.plan.ClientID, mcpClientBackupApplied)
	}

	if !sameClientRegistrationConfig(current.config, record.desired) {
		if err := adapter.writeConfig(ctx, cloneClientRegistrationConfig(current.config), cloneClientRegistrationConfig(record.desired)); err != nil {
			return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationWriteFailed)
		}
	}
	if err := c.backupStore.markApplied(ctx, backup.clientID, backup.receipt.ID); err != nil {
		return MCPClientStatus{}, errClientRegistrationBackupFailed
	}
	return c.statusAfterRegistration(ctx, adapter, record.plan.ClientID, mcpClientBackupApplied)
}

func (c *mcpClientRegistrationController) VerifyClientRegistration(ctx context.Context, clientID string) (MCPClientStatus, error) {
	adapter, err := c.adapterFor(clientID)
	if err != nil {
		return MCPClientStatus{}, err
	}
	status, err := adapter.verify(ctx)
	if err != nil {
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationVerificationFailed)
	}
	if status.Installed != mcpClientInstalled {
		return normalizeClientRegistrationStatus(status, clientID, mcpClientBackupNotCreated, mcpClientEvidenceNotObserved), nil
	}
	return normalizeClientRegistrationStatus(status, clientID, mcpClientBackupNotCreated, mcpClientEvidenceConfigObserved), nil
}

func (c *mcpClientRegistrationController) RestoreClientRegistration(ctx context.Context, clientID, receiptID string) (MCPClientStatus, error) {
	adapter, err := c.adapterFor(clientID)
	if err != nil {
		return MCPClientStatus{}, err
	}
	if !validClientRegistrationID(receiptID) {
		return MCPClientStatus{}, errClientRegistrationBackupNotFound
	}
	current, err := adapter.inspect(ctx)
	if err != nil {
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationInspectionFailed)
	}
	if current.status.Installed != mcpClientInstalled || !clientRegistrationConfigWasObserved(current.status) {
		return MCPClientStatus{}, errClientRegistrationClientUnavailable
	}
	if !validClientRegistrationConfig(current.config) {
		return MCPClientStatus{}, errClientRegistrationConfigTooLarge
	}
	record, err := c.backupStore.load(ctx, clientID, receiptID)
	if err != nil {
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationBackupNotFound)
	}
	if err := validateClientRegistrationBackupRecord(record, clientID, receiptID); err != nil {
		return MCPClientStatus{}, errClientRegistrationBackupNotFound
	}
	currentDigest := digestClientRegistrationConfig(current.config)
	if currentDigest == record.originalDigest {
		if !record.hasApplied {
			return MCPClientStatus{}, errClientRegistrationBackupStale
		}
		if err := c.backupStore.markRestored(ctx, clientID, receiptID); err != nil {
			return MCPClientStatus{}, errClientRegistrationRestoreFailed
		}
		return c.statusAfterRegistration(ctx, adapter, clientID, mcpClientBackupRestored)
	}
	if !record.hasApplied || currentDigest != record.appliedDigest || !sameClientRegistrationConfig(current.config, record.applied) {
		return MCPClientStatus{}, errClientRegistrationBackupStale
	}
	if err := adapter.writeConfig(ctx, cloneClientRegistrationConfig(current.config), cloneClientRegistrationConfig(record.original)); err != nil {
		if errors.Is(err, errClientRegistrationStaleSettings) {
			return MCPClientStatus{}, errClientRegistrationBackupStale
		}
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationRestoreFailed)
	}
	if err := c.backupStore.markRestored(ctx, clientID, receiptID); err != nil {
		return MCPClientStatus{}, errClientRegistrationRestoreFailed
	}
	return c.statusAfterRegistration(ctx, adapter, clientID, mcpClientBackupRestored)
}

func (c *mcpClientRegistrationController) statusAfterRegistration(ctx context.Context, adapter clientRegistrationAdapter, clientID, backupStatus string) (MCPClientStatus, error) {
	status, err := adapter.verify(ctx)
	if err != nil {
		return MCPClientStatus{}, safeClientRegistrationError(err, errClientRegistrationVerificationFailed)
	}
	return normalizeClientRegistrationStatus(status, clientID, backupStatus, mcpClientEvidenceConfigObserved), nil
}

func (c *mcpClientRegistrationController) backupForPlan(ctx context.Context, record clientRegistrationPlanRecord) (clientRegistrationBackupRecord, error) {
	if record.backupReceiptID != "" {
		backup, err := c.backupStore.load(ctx, record.plan.ClientID, record.backupReceiptID)
		if err == nil && validateClientRegistrationBackupRecord(backup, record.plan.ClientID, record.backupReceiptID) == nil {
			return backup, nil
		}
	}
	backup, err := c.backupStore.findLatest(ctx, record.plan.ClientID, record.originalDigest)
	if err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupRequired
	}
	if validateClientRegistrationBackupRecord(backup, record.plan.ClientID, backup.receipt.ID) != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	return backup, nil
}

func (c *mcpClientRegistrationController) rememberBackupReceipt(planID, receiptID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.plans[planID]
	if !ok {
		return
	}
	record.backupReceiptID = receiptID
	c.plans[planID] = record
}

func (c *mcpClientRegistrationController) planRecord(planID string) (clientRegistrationPlanRecord, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneExpiredPlansLocked(now)
	record, ok := c.plans[planID]
	if !ok || now.Sub(record.createdAt) > c.planTTL {
		delete(c.plans, planID)
		return clientRegistrationPlanRecord{}, false
	}
	return record, true
}

func (c *mcpClientRegistrationController) pruneExpiredPlansLocked(now time.Time) {
	for planID, record := range c.plans {
		if now.Sub(record.createdAt) > c.planTTL {
			delete(c.plans, planID)
		}
	}
}

func (c *mcpClientRegistrationController) adapterFor(clientID string) (clientRegistrationAdapter, error) {
	if !isSupportedMCPClientID(clientID) {
		return nil, errClientRegistrationInvalidSpec
	}
	adapter, ok := c.adapters[clientID]
	if !ok {
		return nil, errClientRegistrationClientUnavailable
	}
	return adapter, nil
}

func (c *mcpClientRegistrationController) newOpaqueID() (string, error) {
	id, err := c.newID()
	if err != nil || !validClientRegistrationID(id) {
		return "", errClientRegistrationPlanFailed
	}
	return id, nil
}

func validateMCPRegistrationSpec(spec MCPRegistrationSpec) error {
	if !isSupportedMCPClientID(spec.ClientID) || spec.DisplayName != mcpClientRegistrationDisplayName {
		return errClientRegistrationInvalidSpec
	}
	if spec.Transport != MCPTransportStdio {
		return errClientRegistrationUnsupportedTransport
	}
	if spec.Endpoint != "" || !filepath.IsAbs(spec.Command) || len(spec.Args) != 1 || spec.Args[0] != "--mcp" || len(spec.Scope) != 1 || spec.Scope[0] != "user" {
		return errClientRegistrationInvalidSpec
	}
	for _, value := range append(append([]string{}, spec.Args...), spec.Scope...) {
		if strings.ContainsAny(value, "\r\n\x00") {
			return errClientRegistrationInvalidSpec
		}
	}
	if !clientRegistrationDisplayNamePattern.MatchString(spec.DisplayName) {
		return errClientRegistrationInvalidSpec
	}
	actualPath, err := os.Executable()
	if err != nil {
		return errClientRegistrationInvalidSpec
	}
	actualPath, err = filepath.Abs(actualPath)
	if err != nil {
		return errClientRegistrationInvalidSpec
	}
	providedPath, err := filepath.Abs(spec.Command)
	if err != nil {
		return errClientRegistrationInvalidSpec
	}
	if runtime.GOOS == "windows" {
		if !strings.EqualFold(filepath.Clean(actualPath), filepath.Clean(providedPath)) {
			return errClientRegistrationInvalidSpec
		}
	} else if filepath.Clean(actualPath) != filepath.Clean(providedPath) {
		return errClientRegistrationInvalidSpec
	}
	return nil
}

func safeClientRegistrationChanges(current, desired clientRegistrationConfig, adapterChanges []string) ([]string, error) {
	if sameClientRegistrationConfig(current, desired) {
		if len(adapterChanges) != 1 || adapterChanges[0] != mcpRegistrationChangeUnchanged {
			return nil, errClientRegistrationPlanFailed
		}
		return []string{mcpRegistrationChangeUnchanged}, nil
	}
	if !desired.exists || len(adapterChanges) != 1 || adapterChanges[0] != mcpRegistrationChangeAdded {
		return nil, errClientRegistrationNameConflict
	}
	return []string{mcpRegistrationChangeAdded}, nil
}

func normalizeClientRegistrationStatus(status MCPClientStatus, clientID, backup, evidence string) MCPClientStatus {
	installed := mcpClientInstalledNotObserved
	if status.Installed == mcpClientInstalled {
		installed = mcpClientInstalled
	}
	version := ""
	if clientRegistrationVersionPattern.MatchString(status.Version) {
		version = status.Version
	}
	registration := mcpClientRegistrationNotObserved
	switch status.Registration {
	case mcpClientRegistrationRegistered, mcpClientRegistrationNotRegistered, mcpClientRegistrationConflict, mcpClientRegistrationNotObserved:
		if installed == mcpClientInstalled {
			registration = status.Registration
		}
	}
	if status.EvidenceSource == mcpClientEvidenceNotObserved {
		evidence = mcpClientEvidenceNotObserved
	}
	if evidence != mcpClientEvidenceNotObserved && evidence != mcpClientEvidenceConfigObserved {
		evidence = mcpClientEvidenceNotObserved
	}
	if installed == mcpClientInstalled && (registration == mcpClientRegistrationNotObserved || evidence == mcpClientEvidenceNotObserved) {
		registration = mcpClientRegistrationNotObserved
		evidence = mcpClientEvidenceNotObserved
	}
	if installed == mcpClientInstalledNotObserved {
		registration = mcpClientRegistrationNotObserved
		evidence = mcpClientEvidenceNotObserved
		version = ""
	}
	return MCPClientStatus{
		ClientID:       clientID,
		Installed:      installed,
		Version:        version,
		Registration:   registration,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         backup,
		EvidenceSource: evidence,
	}
}

func safeClientRegistrationError(err, fallback error) error {
	if err == nil {
		return fallback
	}
	known := []error{
		errClientRegistrationClientUnavailable,
		errClientRegistrationInvalidSpec,
		errClientRegistrationUnsupportedTransport,
		errClientRegistrationNameConflict,
		errClientRegistrationPlanExpired,
		errClientRegistrationStaleSettings,
		errClientRegistrationBackupRequired,
		errClientRegistrationBackupNotFound,
		errClientRegistrationBackupStale,
		errClientRegistrationConfigTooLarge,
		errClientRegistrationInspectionFailed,
		errClientRegistrationPlanFailed,
		errClientRegistrationBackupFailed,
		errClientRegistrationWriteFailed,
		errClientRegistrationVerificationFailed,
		errClientRegistrationRestoreFailed,
	}
	for _, candidate := range known {
		if errors.Is(err, candidate) {
			return candidate
		}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return fallback
}

func clientRegistrationUIErrorCode(err error) string {
	switch {
	case errors.Is(err, errClientRegistrationClientUnavailable):
		return "client_unavailable"
	case errors.Is(err, errClientRegistrationInvalidSpec):
		return "invalid_spec"
	case errors.Is(err, errClientRegistrationUnsupportedTransport):
		return "unsupported_transport"
	case errors.Is(err, errClientRegistrationNameConflict):
		return "name_conflict"
	case errors.Is(err, errClientRegistrationPlanExpired):
		return "plan_expired"
	case errors.Is(err, errClientRegistrationStaleSettings):
		return "stale_settings"
	case errors.Is(err, errClientRegistrationBackupStale):
		return "backup_stale"
	case errors.Is(err, errClientRegistrationBackupRequired):
		return "backup_required"
	case errors.Is(err, errClientRegistrationBackupNotFound):
		return "backup_not_found"
	case errors.Is(err, errClientRegistrationConfigTooLarge):
		return "config_too_large"
	default:
		return "failed"
	}
}

func clientRegistrationConfigWasObserved(status MCPClientStatus) bool {
	return status.EvidenceSource == mcpClientEvidenceConfigObserved && status.Registration != mcpClientRegistrationNotObserved
}

func validClientRegistrationConfig(config clientRegistrationConfig) bool {
	if len(config.bytes) > maxClientRegistrationConfigBytes {
		return false
	}
	if !config.exists && len(config.bytes) != 0 {
		return false
	}
	return true
}

func cloneClientRegistrationConfig(config clientRegistrationConfig) clientRegistrationConfig {
	return clientRegistrationConfig{
		exists: config.exists,
		bytes:  append([]byte(nil), config.bytes...),
	}
}

func sameClientRegistrationConfig(left, right clientRegistrationConfig) bool {
	return left.exists == right.exists && bytes.Equal(left.bytes, right.bytes)
}

func digestClientRegistrationConfig(config clientRegistrationConfig) clientRegistrationDigest {
	hash := sha256.New()
	if config.exists {
		_, _ = hash.Write([]byte{1})
	} else {
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(config.bytes)
	var digest clientRegistrationDigest
	copy(digest[:], hash.Sum(nil))
	return digest
}

func constantTimeStringEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	var difference byte
	for index := range left {
		difference |= left[index] ^ right[index]
	}
	return difference == 0
}

func validClientRegistrationID(value string) bool {
	return clientRegistrationIDPattern.MatchString(value)
}

func isSupportedMCPClientID(clientID string) bool {
	switch clientID {
	case mcpClientIDCodex, mcpClientIDClaude, mcpClientIDGemini:
		return true
	default:
		return false
	}
}

func newClientRegistrationOpaqueID() (string, error) {
	var randomBytes [24]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return "", errClientRegistrationPlanFailed
	}
	return base64.RawURLEncoding.EncodeToString(randomBytes[:]), nil
}

func (record clientRegistrationPlanRecord) desiredDigestEqualOriginal() bool {
	return record.desiredDigest == record.originalDigest && sameClientRegistrationConfig(record.desired, record.original)
}
