package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// errOnboardingActivationNotCommitted is an internal signal to the sidecar
// store that it may release the stable target-ID reservation. The callback
// emits it only while holding the config lock after verifying that no target
// with the reserved ID exists in any target collection.
var errOnboardingActivationNotCommitted = errors.New("registration activation was not committed")

// errOnboardingActivationAmbiguous tells the durable draft store that a save
// helper may have left an unreferenced credential behind. The store must keep
// the reservation and block another apply attempt, including after restart.
var errOnboardingActivationAmbiguous = errors.New("registration activation has an ambiguous save result")

// saveOnboardingActivationTarget is a narrow fault-injection seam for testing
// failures reported after a save helper may have written a credential.
var saveOnboardingActivationTarget = func(
	c *onboardingCheckController,
	stableTargetID string,
	service onboardingServiceKind,
	fields onboardingDraftFields,
	request onboardingActivationRequest,
	credential []byte,
) (string, error) {
	return c.saveOnboardingTarget(stableTargetID, service, fields, request, credential)
}

// onboardingPolicyScope describes the fixed tool surface for a service. It is
// descriptive: this review does not add per-operation gates to MCP calls.
func onboardingPolicyScope(service onboardingServiceKind) (included, excluded, mappingRequired, notes []string, err error) {
	included = []string{}
	excluded = []string{}
	mappingRequired = []string{}
	notes = []string{"This review describes the service's fixed tool scope. Individual MCP operations cannot be selected or gated here."}
	switch service {
	case onboardingServiceGitHub:
		included = []string{"github_repository", "github_pull_request"}
	case onboardingServiceJenkins:
		included = []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log"}
		notes = append(notes, "Jenkins job triggering remains available only after per-call approval; this new target is not preapproved.")
	case onboardingServiceDashboard:
		included = []string{"dashboard_namespaces"}
		mappingRequired = []string{
			"dashboard_deployment_status",
			"dashboard_deployment_diagnosis",
			"dashboard_deployment_events",
			"dashboard_deployment_pods",
			"dashboard_deployment_pod_logs",
		}
		notes = append(notes, "Dashboard deployment, event, Pod, and log reads require service-bundle environment resource mappings.")
	default:
		return nil, nil, nil, nil, errOnboardingInvalid
	}
	return included, excluded, mappingRequired, notes, nil
}

// onboardingPolicyDigest binds the review to the exact fixed policy text shown
// to the user. Candidate data has its own digest in the check and activation CAS.
func onboardingPolicyDigest(service onboardingServiceKind, fields onboardingDraftFields) string {
	_ = fields // Candidate changes are bound separately by onboardingCandidateDigest.
	included, excluded, mappingRequired, notes, err := onboardingPolicyScope(service)
	if err != nil {
		return ""
	}
	policy := struct {
		Version         int                   `json:"version"`
		Service         onboardingServiceKind `json:"service"`
		IncludedTools   []string              `json:"included_tools"`
		ExcludedTools   []string              `json:"excluded_tools"`
		MappingRequired []string              `json:"mapping_required"`
		PolicyNotes     []string              `json:"policy_notes"`
	}{
		Version: 1, Service: service,
		IncludedTools: included, ExcludedTools: excluded,
		MappingRequired: mappingRequired, PolicyNotes: notes,
	}
	encoded, marshalErr := json.Marshal(policy)
	if marshalErr != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "policy:sha256:" + hex.EncodeToString(digest[:])
}

func onboardingPolicyReviewForDraft(draft onboardingDraft) (onboardingPolicyReview, error) {
	if draft.ID == "" || draft.Revision == 0 {
		return onboardingPolicyReview{}, errOnboardingInvalid
	}
	candidateDigest, digestErr := onboardingCandidateDigest(draft.Service, draft.Fields)
	policyDigest := onboardingPolicyDigest(draft.Service, draft.Fields)
	if digestErr != nil || !validOnboardingDigest(candidateDigest, "sha256:") || !validOnboardingDigest(policyDigest, "policy:sha256:") {
		return onboardingPolicyReview{}, errOnboardingInvalid
	}
	included, excluded, mappingRequired, notes, err := onboardingPolicyScope(draft.Service)
	if err != nil {
		return onboardingPolicyReview{}, errOnboardingInvalid
	}
	return onboardingPolicyReview{
		DraftID: draft.ID, DraftRevision: draft.Revision, CheckGeneration: draft.CheckGeneration, Service: draft.Service,
		CandidateDigest: candidateDigest, PolicyDigest: policyDigest,
		IncludedTools: included, ExcludedTools: excluded,
		MappingRequired: mappingRequired, PolicyNotes: notes,
	}, nil
}

func (c *onboardingCheckController) ReviewDraft(ctx context.Context, draftID string, expectedRevision uint64) (onboardingPolicyReview, error) {
	if ctx == nil || ctx.Err() != nil || c == nil || c.store == nil || strings.TrimSpace(c.configPath) == "" || draftID == "" || expectedRevision == 0 {
		return onboardingPolicyReview{}, errOnboardingInvalid
	}
	draft, err := c.store.GetDraft(ctx, draftID)
	if err != nil {
		return onboardingPolicyReview{}, onboardingActivationError(err)
	}
	if draft.ID != draftID || draft.Revision != expectedRevision {
		return onboardingPolicyReview{}, errOnboardingDraftStale
	}
	review, err := onboardingPolicyReviewForDraft(draft)
	if err != nil {
		return onboardingPolicyReview{}, errOnboardingInvalid
	}
	var configRevision string
	err = withConfigLock(func() error {
		if ctx.Err() != nil {
			return errOnboardingInvalid
		}
		cfg, readErr := readConfig(c.configPath)
		if readErr != nil {
			return errOnboardingUnavailable
		}
		revision, revisionErr := setupDraftConfigRevision(cfg)
		if revisionErr != nil || revision == "" {
			return errOnboardingUnavailable
		}
		configRevision = revision
		return nil
	})
	if err != nil {
		return onboardingPolicyReview{}, onboardingActivationError(err)
	}
	review.ConfigRevision = configRevision
	return review, nil
}

func (c *onboardingCheckController) ActivateDraft(ctx context.Context, request onboardingActivationRequest) (onboardingActivationRecord, error) {
	if request.Credential != nil {
		defer clear(request.Credential)
	}
	if ctx == nil || ctx.Err() != nil || c == nil || c.store == nil || c.proofs == nil || c.secrets == nil || strings.TrimSpace(c.configPath) == "" {
		return onboardingActivationRecord{}, errOnboardingInvalid
	}
	if !request.PolicyConfirmed {
		return onboardingActivationRecord{}, errOnboardingReviewChanged
	}
	cas := request.CAS
	if !validOnboardingActivationCAS(cas) || request.CheckPermit == "" {
		return onboardingActivationRecord{}, errOnboardingInvalid
	}
	if !validOnboardingActivationCredentialChoice(request.Credential, request.NamedSecretID) {
		return onboardingActivationRecord{}, errOnboardingInvalid
	}

	draft, err := c.store.GetDraft(ctx, cas.DraftID)
	if err != nil {
		if errors.Is(err, errOnboardingDraftNotFound) {
			// WithActivation checks the durable completion record before looking
			// for a draft. The callback must never run on this replay path.
			record, replayErr := c.store.WithActivation(ctx, cas, func(onboardingDraft, string) error {
				return errOnboardingDraftNotFound
			})
			if replayErr != nil {
				return onboardingActivationRecord{}, onboardingActivationError(replayErr)
			}
			policyDigest := onboardingPolicyDigest(record.Service, onboardingDraftFields{})
			if record.DraftID != cas.DraftID || record.DraftRevision != cas.ExpectedRevision ||
				record.CheckGeneration != cas.CheckGeneration || record.CandidateDigest != cas.CandidateDigest ||
				record.PolicyDigest != cas.PolicyDigest || record.ConfigRevision != cas.ConfigRevision ||
				!validOnboardingTargetID(record.Service, record.TargetID) || policyDigest != cas.PolicyDigest {
				return onboardingActivationRecord{}, errOnboardingReviewChanged
			}
			if !c.activationRecordStillMatchesConfig(ctx, record) {
				return onboardingActivationRecord{}, errOnboardingReviewChanged
			}
			return record, nil
		}
		return onboardingActivationRecord{}, onboardingActivationError(err)
	}
	if draft.ID != cas.DraftID || draft.Revision != cas.ExpectedRevision {
		return onboardingActivationRecord{}, errOnboardingDraftStale
	}
	review, err := onboardingPolicyReviewForDraft(draft)
	if err != nil {
		return onboardingActivationRecord{}, errOnboardingInvalid
	}
	if review.CandidateDigest != cas.CandidateDigest || review.PolicyDigest != cas.PolicyDigest {
		return onboardingActivationRecord{}, errOnboardingReviewChanged
	}
	receipt := draft.Check
	if receipt == nil || receipt.Outcome != onboardingCheckSucceeded || receipt.DraftID != draft.ID ||
		receipt.DraftRevision != draft.Revision || receipt.CheckGeneration != cas.CheckGeneration ||
		receipt.CandidateDigest != cas.CandidateDigest ||
		receipt.PolicyDigest != cas.PolicyDigest || receipt.ConfigRevision != cas.ConfigRevision ||
		receipt.CheckedAt.IsZero() || receipt.CheckedAt.Location() != time.UTC {
		return onboardingActivationRecord{}, errOnboardingCheckRequired
	}

	proofInput, resolvedCredential, cleanupCredential, err := c.activationProofInput(ctx, draft, cas, request)
	if cleanupCredential != nil {
		defer clear(cleanupCredential)
	}
	if err != nil {
		return onboardingActivationRecord{}, err
	}

	callbackCommitted := false
	var callbackFailure error
	record, activationErr := c.store.WithActivation(ctx, cas, func(current onboardingDraft, stableTargetID string) (callbackErr error) {
		defer func() {
			if callbackErr != nil {
				callbackFailure = callbackErr
			}
		}()
		if !validOnboardingReservedTargetID(current.Service, stableTargetID) {
			return errOnboardingUnavailable
		}

		return withConfigLock(func() error {
			cfg, readErr := readConfig(c.configPath)
			if readErr != nil {
				return errOnboardingUnavailable
			}
			exists, exact, ref := onboardingTargetByReservedID(cfg, current.Service, stableTargetID, current.Fields)
			definitelyUncommitted := func(reason error) error {
				if !exists {
					return onboardingDefinitelyNotCommitted(reason)
				}
				return reason
			}
			if current.ActivationRetry && (!exists || !exact) {
				// Once a pending reservation has already reached an apply callback,
				// an absent or conflicting target cannot release the reservation or
				// re-enter a save helper. The durable reservation protects this path
				// even if persisting the manual-recovery marker fails.
				return errors.Join(errOnboardingActivationAmbiguous, errOnboardingActivationPending)
			}
			if ctx.Err() != nil {
				return definitelyUncommitted(errOnboardingInvalid)
			}
			if current.ID != draft.ID || current.Revision != draft.Revision || current.Service != draft.Service || current.Fields != draft.Fields {
				return definitelyUncommitted(errOnboardingDraftStale)
			}
			currentReview, reviewErr := onboardingPolicyReviewForDraft(current)
			if reviewErr != nil || currentReview.CandidateDigest != cas.CandidateDigest || currentReview.PolicyDigest != cas.PolicyDigest {
				return definitelyUncommitted(errOnboardingReviewChanged)
			}
			if current.Check == nil || current.Check.Outcome != onboardingCheckSucceeded ||
				current.Check.DraftRevision != cas.ExpectedRevision || current.Check.CheckGeneration != cas.CheckGeneration ||
				current.Check.CandidateDigest != cas.CandidateDigest ||
				current.Check.PolicyDigest != cas.PolicyDigest || current.Check.ConfigRevision != cas.ConfigRevision {
				return definitelyUncommitted(errOnboardingCheckRequired)
			}
			configRevision, revisionErr := setupDraftConfigRevision(cfg)
			if revisionErr != nil || configRevision != cas.ConfigRevision {
				return definitelyUncommitted(errOnboardingCheckRequired)
			}
			if !c.activationNamedSecretStillMatches(cfg, request.NamedSecretID, proofInput.SecretMetadataDigest) {
				return definitelyUncommitted(errOnboardingCheckPermit)
			}
			if !c.activationCredentialBytesStillMatch(cfg, request, proofInput, resolvedCredential) {
				return definitelyUncommitted(errOnboardingCheckPermit)
			}
			if exists {
				if !exact || !c.activationCredentialMatches(cfg, ref, current.Service, request, proofInput, resolvedCredential) {
					return errOnboardingActivationPending
				}
			}
			if err := c.proofs.Validate(request.CheckPermit, proofInput, c.onboardingNow()); err != nil {
				return definitelyUncommitted(errOnboardingCheckPermit)
			}
			if exists {
				callbackCommitted = true
				return nil
			}
			savedID, saveErr := saveOnboardingActivationTarget(c, stableTargetID, current.Service, current.Fields, request, resolvedCredential)
			after, afterErr := readConfig(c.configPath)
			if afterErr != nil {
				if saveErr != nil && savedID == stableTargetID {
					// The helper's returned ID on failure identifies the explicit
					// cleanup-ambiguous case, even though settings cannot be reread.
					return errors.Join(errOnboardingActivationAmbiguous, errOnboardingActivationPending)
				}
				// A successful helper may have committed the target. Keep the
				// reservation so a fresh checked retry can inspect and finalize it.
				return errOnboardingActivationPending
			}
			found, exactAfter, afterRef := onboardingTargetByReservedID(after, current.Service, stableTargetID, current.Fields)
			if !found {
				if !configHasTargetID(after, stableTargetID) {
					if saveErr != nil && savedID == stableTargetID {
						// The target is absent but the helper returned its ID on an
						// unsuccessful write. That signals a possible failed vault
						// cleanup, so retain the reservation instead of inviting a
						// retry that could create another orphan credential.
						return errors.Join(errOnboardingActivationAmbiguous, errOnboardingActivationPending)
					}
					return onboardingDefinitelyNotCommitted(errOnboardingUnavailable)
				}
				return errOnboardingActivationPending
			}
			if !exactAfter || !c.activationCredentialMatches(after, afterRef, current.Service, request, proofInput, resolvedCredential) {
				return errOnboardingActivationPending
			}
			if saveErr == nil && savedID != stableTargetID {
				return errOnboardingActivationPending
			}
			callbackCommitted = true
			return nil
		})
	})
	if activationErr != nil {
		if errors.Is(activationErr, errOnboardingActivationRecoveryRequired) {
			return onboardingActivationRecord{}, onboardingActivationError(activationErr)
		}
		if callbackCommitted {
			return onboardingActivationRecord{}, errOnboardingActivationPending
		}
		if callbackFailure != nil {
			return onboardingActivationRecord{}, onboardingActivationError(callbackFailure)
		}
		if errors.Is(activationErr, errOnboardingReviewChanged) {
			latest, latestErr := c.store.GetDraft(ctx, cas.DraftID)
			if latestErr == nil && latest.Revision == cas.ExpectedRevision &&
				(latest.Check == nil || latest.Check.Outcome != onboardingCheckSucceeded || latest.Check.CheckGeneration != cas.CheckGeneration) {
				return onboardingActivationRecord{}, errOnboardingCheckRequired
			}
		}
		return onboardingActivationRecord{}, onboardingActivationError(activationErr)
	}
	if record.DraftID != cas.DraftID || record.DraftRevision != cas.ExpectedRevision || record.CheckGeneration != cas.CheckGeneration ||
		record.Service != draft.Service || record.TargetID == "" ||
		record.CandidateDigest != cas.CandidateDigest || record.PolicyDigest != cas.PolicyDigest || record.ConfigRevision != cas.ConfigRevision {
		return onboardingActivationRecord{}, errOnboardingActivationPending
	}
	if !callbackCommitted {
		// Another concurrent activation may have completed and removed the draft
		// after our GetDraft but before WithActivation acquired the sidecar lock.
		// The store returns its completed record without invoking our callback.
		if !c.activationRecordStillMatchesConfig(ctx, record) {
			return onboardingActivationRecord{}, errOnboardingReviewChanged
		}
		return record, nil
	}
	if !c.activationRecordStillMatchesConfig(ctx, record) {
		return onboardingActivationRecord{}, errOnboardingReviewChanged
	}
	return record, nil
}

func (c *onboardingCheckController) activationRecordStillMatchesConfig(ctx context.Context, record onboardingActivationRecord) bool {
	if ctx == nil || ctx.Err() != nil || c == nil || strings.TrimSpace(c.configPath) == "" ||
		record.DraftID == "" || !validOnboardingTargetID(record.Service, record.TargetID) {
		return false
	}
	matched := false
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errOnboardingInvalid
		}
		cfg, readErr := readConfig(c.configPath)
		if readErr != nil {
			return errOnboardingUnavailable
		}
		matched = onboardingTargetMatchesActivation(cfg, record)
		return nil
	})
	return err == nil && matched
}

func onboardingTargetMatchesActivation(cfg config, record onboardingActivationRecord) bool {
	var fields onboardingDraftFields
	switch record.Service {
	case onboardingServiceGitHub:
		index := findGitHubTargetIndex(cfg.GitHubTargets, record.TargetID)
		if index < 0 {
			return false
		}
		target := cfg.GitHubTargets[index]
		if validateTarget(target) != nil || target.Disabled {
			return false
		}
		fields = onboardingDraftFields{Name: target.Name, Origin: target.Origin, Repository: target.Repository}
	case onboardingServiceJenkins:
		index := findJenkinsTargetIndex(cfg.JenkinsTargets, record.TargetID)
		if index < 0 {
			return false
		}
		target := cfg.JenkinsTargets[index]
		if validateJenkinsTarget(target) != nil || target.Disabled || target.NonProductionPreapproved || target.NonProductionApprovalScope != "" {
			return false
		}
		fields = onboardingDraftFields{
			Name: target.Name, BaseURL: target.BaseURL, Username: target.Username,
			JobPath: target.JobPath, Environment: target.Environment,
		}
	case onboardingServiceDashboard:
		index := findDashboardTargetIndex(cfg.DashboardTargets, record.TargetID)
		if index < 0 {
			return false
		}
		target := cfg.DashboardTargets[index]
		if validateDashboardTarget(target) != nil || target.Disabled {
			return false
		}
		fields = onboardingDraftFields{Name: target.Name, BaseURL: target.BaseURL}
	default:
		return false
	}
	digest, err := onboardingCandidateDigest(record.Service, fields)
	return err == nil && digest == record.CandidateDigest
}

func onboardingDefinitelyNotCommitted(reason error) error {
	if reason == nil {
		reason = errOnboardingUnavailable
	}
	return errors.Join(errOnboardingActivationNotCommitted, reason)
}

func (c *onboardingCheckController) activationProofInput(ctx context.Context, draft onboardingDraft, cas onboardingActivationCAS, request onboardingActivationRequest) (onboardingCheckProofInput, []byte, []byte, error) {
	input := onboardingCheckProofInput{
		DraftID: draft.ID, DraftRevision: draft.Revision,
		CheckGeneration: cas.CheckGeneration,
		CandidateDigest: cas.CandidateDigest, PolicyDigest: cas.PolicyDigest,
		ConfigRevision: cas.ConfigRevision,
	}
	if request.NamedSecretID == "" {
		if len(request.Credential) == 0 || len(request.Credential) > maxSecretSize {
			return onboardingCheckProofInput{}, nil, nil, errOnboardingInvalid
		}
		credential := append([]byte(nil), request.Credential...)
		input.Credential = credential
		input.CredentialSource = onboardingCredentialTemporary
		return input, credential, credential, nil
	}
	if !namedSecretIDPattern.MatchString(request.NamedSecretID) || len(request.Credential) != 0 {
		return onboardingCheckProofInput{}, nil, nil, errOnboardingInvalid
	}
	if ctx == nil || ctx.Err() != nil {
		return onboardingCheckProofInput{}, nil, nil, errOnboardingInvalid
	}
	var metadata namedSecretMetadata
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errOnboardingInvalid
		}
		cfg, readErr := readConfig(c.configPath)
		if readErr != nil {
			return errOnboardingUnavailable
		}
		index := namedSecretIndex(cfg.NamedSecrets, request.NamedSecretID)
		if index < 0 {
			return errOnboardingCheckPermit
		}
		metadata = cfg.NamedSecrets[index]
		if !secretRefPattern.MatchString(metadata.CredentialRef) {
			return errOnboardingCheckPermit
		}
		return nil
	})
	if err != nil {
		return onboardingCheckProofInput{}, nil, nil, onboardingActivationError(err)
	}
	credential, err := c.secrets.Load(metadata.CredentialRef)
	if err != nil || len(credential) == 0 || len(credential) > maxSecretSize {
		clear(credential)
		return onboardingCheckProofInput{}, nil, nil, errOnboardingCheckPermit
	}
	input.Credential = credential
	input.CredentialSource = onboardingCredentialNamed
	input.NamedSecretID = request.NamedSecretID
	input.SecretMetadataDigest, err = onboardingSecretMetadataDigest(metadata)
	if err != nil || !validOnboardingDigest(input.SecretMetadataDigest, "sha256:") {
		clear(credential)
		return onboardingCheckProofInput{}, nil, nil, errOnboardingCheckPermit
	}
	return input, credential, credential, nil
}

func (c *onboardingCheckController) activationNamedSecretStillMatches(cfg config, namedSecretID, expectedDigest string) bool {
	if namedSecretID == "" {
		return expectedDigest == ""
	}
	index := namedSecretIndex(cfg.NamedSecrets, namedSecretID)
	if index < 0 {
		return false
	}
	metadata := cfg.NamedSecrets[index]
	actualDigest, err := onboardingSecretMetadataDigest(metadata)
	return err == nil && secretRefPattern.MatchString(metadata.CredentialRef) && expectedDigest != "" && actualDigest == expectedDigest
}

func (c *onboardingCheckController) activationCredentialBytesStillMatch(cfg config, request onboardingActivationRequest, proof onboardingCheckProofInput, credential []byte) bool {
	if request.NamedSecretID == "" {
		return len(request.Credential) > 0 && len(request.Credential) == len(credential) && subtle.ConstantTimeCompare(request.Credential, credential) == 1
	}
	index := namedSecretIndex(cfg.NamedSecrets, request.NamedSecretID)
	if index < 0 {
		return false
	}
	current, err := c.secrets.Load(cfg.NamedSecrets[index].CredentialRef)
	if err != nil || len(current) == 0 {
		clear(current)
		return false
	}
	defer clear(current)
	return len(current) == len(credential) && subtle.ConstantTimeCompare(current, credential) == 1 && proof.CredentialSource == onboardingCredentialNamed
}

func (c *onboardingCheckController) activationCredentialMatches(cfg config, targetSecretRef string, service onboardingServiceKind, request onboardingActivationRequest, proof onboardingCheckProofInput, credential []byte) bool {
	if !secretRefPattern.MatchString(targetSecretRef) {
		return false
	}
	if request.NamedSecretID != "" {
		index := namedSecretIndex(cfg.NamedSecrets, request.NamedSecretID)
		if index < 0 {
			return false
		}
		metadataDigest, digestErr := onboardingSecretMetadataDigest(cfg.NamedSecrets[index])
		if digestErr != nil || cfg.NamedSecrets[index].CredentialRef != targetSecretRef ||
			metadataDigest != proof.SecretMetadataDigest {
			return false
		}
	}
	stored, err := c.secrets.Load(targetSecretRef)
	if err != nil || len(stored) == 0 {
		clear(stored)
		return false
	}
	defer clear(stored)
	return len(stored) == len(credential) && subtle.ConstantTimeCompare(stored, credential) == 1
}

func (c *onboardingCheckController) saveOnboardingTarget(stableTargetID string, service onboardingServiceKind, fields onboardingDraftFields, request onboardingActivationRequest, credential []byte) (string, error) {
	secret := ""
	if request.NamedSecretID == "" {
		secret = string(credential)
	}
	switch service {
	case onboardingServiceGitHub:
		return (&app{configPath: c.configPath, secrets: c.secrets}).saveOnboardingGitHubTargetWithNamedSecret(
			stableTargetID, fields.Name, fields.Origin, fields.Repository, secret, request.NamedSecretID,
		)
	case onboardingServiceJenkins:
		return (&app{configPath: c.configPath, secrets: c.secrets}).saveOnboardingJenkinsTargetWithNamedSecret(
			stableTargetID, fields.Name, fields.BaseURL, fields.Username, fields.JobPath, fields.Environment, secret, request.NamedSecretID,
		)
	case onboardingServiceDashboard:
		return (&app{configPath: c.configPath, secrets: c.secrets}).saveOnboardingDashboardTargetWithNamedSecret(
			stableTargetID, fields.Name, fields.BaseURL, secret, request.NamedSecretID,
		)
	default:
		return "", errOnboardingInvalid
	}
}

func onboardingTargetByReservedID(cfg config, service onboardingServiceKind, targetID string, fields onboardingDraftFields) (exists, exact bool, secretRef string) {
	switch service {
	case onboardingServiceGitHub:
		index := findGitHubTargetIndex(cfg.GitHubTargets, targetID)
		if index < 0 {
			return configHasTargetID(cfg, targetID), false, ""
		}
		target := cfg.GitHubTargets[index]
		return true, validateTarget(target) == nil && !target.Disabled && target.Name == fields.Name && target.Origin == fields.Origin && target.Repository == fields.Repository, target.SecretRef
	case onboardingServiceJenkins:
		index := findJenkinsTargetIndex(cfg.JenkinsTargets, targetID)
		if index < 0 {
			return configHasTargetID(cfg, targetID), false, ""
		}
		target := cfg.JenkinsTargets[index]
		exact = validateJenkinsTarget(target) == nil && !target.Disabled && !target.NonProductionPreapproved && target.NonProductionApprovalScope == "" &&
			target.Name == fields.Name && target.BaseURL == fields.BaseURL && target.Username == fields.Username &&
			target.JobPath == fields.JobPath && target.Environment == fields.Environment
		return true, exact, target.SecretRef
	case onboardingServiceDashboard:
		index := findDashboardTargetIndex(cfg.DashboardTargets, targetID)
		if index < 0 {
			return configHasTargetID(cfg, targetID), false, ""
		}
		target := cfg.DashboardTargets[index]
		return true, validateDashboardTarget(target) == nil && !target.Disabled && target.Name == fields.Name && target.BaseURL == fields.BaseURL, target.SecretRef
	default:
		return false, false, ""
	}
}

func configHasTargetID(cfg config, targetID string) bool {
	for _, item := range cfg.GitHubTargets {
		if item.ID == targetID {
			return true
		}
	}
	for _, item := range cfg.JenkinsTargets {
		if item.ID == targetID {
			return true
		}
	}
	for _, item := range cfg.HarborTargets {
		if item.ID == targetID {
			return true
		}
	}
	for _, item := range cfg.DashboardTargets {
		if item.ID == targetID {
			return true
		}
	}
	return false
}

func validOnboardingReservedTargetID(service onboardingServiceKind, targetID string) bool {
	switch service {
	case onboardingServiceGitHub:
		return githubIDPattern.MatchString(targetID)
	case onboardingServiceJenkins:
		return jenkinsIDPattern.MatchString(targetID)
	case onboardingServiceDashboard:
		return dashboardIDPattern.MatchString(targetID)
	default:
		return false
	}
}

func validOnboardingActivationCredentialChoice(credential []byte, namedSecretID string) bool {
	if namedSecretID == "" {
		return len(credential) > 0 && len(credential) <= maxSecretSize
	}
	return len(credential) == 0 && namedSecretIDPattern.MatchString(namedSecretID)
}

func (c *onboardingCheckController) onboardingNow() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func onboardingActivationError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errOnboardingInvalid), errors.Is(err, errOnboardingDraftNotFound),
		errors.Is(err, errOnboardingDraftStale), errors.Is(err, errOnboardingCheckRequired),
		errors.Is(err, errOnboardingCheckPermit), errors.Is(err, errOnboardingReviewChanged),
		errors.Is(err, errOnboardingActivationPending), errors.Is(err, errOnboardingActivationRecoveryRequired),
		errors.Is(err, errOnboardingUnavailable):
		return err
	default:
		return errOnboardingUnavailable
	}
}
