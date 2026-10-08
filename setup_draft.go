package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	setupDraftMaximumPending                   = 64
	setupDraftApprovalClaimUncertainNextAction = "The settings were not changed by this request. The draft may have been consumed; check current settings and submit a new draft before retrying."
	setupDraftApprovalClaimedWarningNextAction = "The review was consumed, but approval could not be confirmed. Check current settings and submit a new draft if needed."
)

type setupDraftController struct {
	store      setupDraftStore
	queue      setupDraftQueueStore
	mu         sync.Mutex
	now        func() time.Time
	newDraftID func() (string, error)
	lifetime   time.Duration
}

var _ SetupDraftController = (*setupDraftController)(nil)

func newSetupDraftControllerWithQueue(store setupDraftStore, queue setupDraftQueueStore) *setupDraftController {
	return &setupDraftController{
		store:      store,
		queue:      queue,
		now:        time.Now,
		newDraftID: func() (string, error) { return newTargetID("setupdraft") },
		lifetime:   setupDraftDefaultLifetime,
	}
}

func newSetupDraftResourceID(kind string) (string, error) {
	if kind == "ssh_target" {
		return newSSHDefinitionID("target")
	}
	return newTargetID(kind)
}

func (c *setupDraftController) SubmitSetupDraft(ctx context.Context, submission SetupDraftSubmission) (SetupDraftReview, error) {
	if ctx == nil || ctx.Err() != nil || c == nil || c.store == nil || c.queue == nil {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	normalized, err := normalizeSetupDraftSubmission(submission)
	if err != nil {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := checkSetupDraftContext(ctx); err != nil {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	reviews, err := c.queue.Load(c.currentTime())
	if err != nil {
		return SetupDraftReview{}, errSetupDraftStore
	}
	if len(reviews) >= setupDraftMaximumPending {
		return SetupDraftReview{}, errSetupDraftCapacity
	}
	snapshot, err := c.readSnapshotLocked(ctx)
	if err != nil {
		return SetupDraftReview{}, err
	}
	if normalized.BaseRevision != snapshot.Revision {
		return SetupDraftReview{}, errSetupDraftStale
	}
	lifetime := c.lifetime
	if lifetime <= 0 {
		lifetime = setupDraftDefaultLifetime
	}
	if lifetime > setupDraftMaximumLifetime {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	now := c.currentTime()
	draftID, err := c.draftIDGenerator()()
	if err != nil || !validSetupDraftID(draftID) {
		return SetupDraftReview{}, errSetupDraftStore
	}
	for _, existing := range reviews {
		if existing.ID == draftID {
			return SetupDraftReview{}, errSetupDraftStore
		}
	}
	plan, err := buildSetupDraftPlan(snapshot, normalized, setupDraftResourceIDGenerator(draftID))
	if err != nil {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	if len(plan.changes) == 0 {
		return SetupDraftReview{}, errSetupDraftNoChange
	}
	review := SetupDraftReview{
		ID: draftID, BaseRevision: snapshot.Revision, ExpiresAt: now.Add(lifetime).UTC(),
		Sources: append([]SetupDraftSource(nil), normalized.Sources...),
		Missing: append([]string(nil), plan.missing...), Changes: cloneSetupDraftChanges(plan.changes),
		Proposal: cloneSetupDraftSubmission(normalized),
	}
	review.Digest = setupDraftReviewDigest(review)
	if err := c.queue.Put(review); err != nil {
		if errors.Is(err, errSetupDraftQueueCapacity) {
			return SetupDraftReview{}, errSetupDraftCapacity
		}
		return SetupDraftReview{}, errSetupDraftStore
	}
	return cloneSetupDraftReview(review), nil
}

func (c *setupDraftController) ReviewSetupDraft(ctx context.Context, id, baseRevision, digest string) (SetupDraftReview, error) {
	if ctx == nil || ctx.Err() != nil || c == nil || c.store == nil || c.queue == nil || !validSetupDraftID(id) {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	review, ok, err := c.findDraftLocked(id)
	if err != nil {
		return SetupDraftReview{}, errSetupDraftStore
	}
	if !ok {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	if !c.currentTime().Before(review.ExpiresAt) {
		_ = c.queue.Delete(id)
		return SetupDraftReview{}, errSetupDraftExpired
	}
	if review.BaseRevision != baseRevision || review.Digest != digest {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	if review.Digest != setupDraftReviewDigest(review) {
		return SetupDraftReview{}, errSetupDraftInvalid
	}
	snapshot, err := c.readSnapshotLocked(ctx)
	if err != nil {
		return SetupDraftReview{}, err
	}
	if snapshot.Revision != review.BaseRevision {
		return SetupDraftReview{}, errSetupDraftStale
	}
	return cloneSetupDraftReview(review), nil
}

func (c *setupDraftController) ApproveSetupDraft(ctx context.Context, id, baseRevision, digest string) (SetupDraftApprovalResult, error) {
	if ctx == nil || ctx.Err() != nil || c == nil || c.store == nil || c.queue == nil || !validSetupDraftID(id) {
		return SetupDraftApprovalResult{}, errSetupDraftInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	review, err := c.queue.ClaimReview(id, baseRevision, digest, c.currentTime(), func(review SetupDraftReview) error {
		if !c.currentTime().Before(review.ExpiresAt) {
			return errSetupDraftExpired
		}
		if review.ID != id || review.BaseRevision != baseRevision || review.Digest != digest || review.Digest != setupDraftReviewDigest(review) {
			return errSetupDraftInvalid
		}
		if err := checkSetupDraftContext(ctx); err != nil {
			return errSetupDraftInvalid
		}
		return withConfigLock(func() error {
			if err := checkSetupDraftContext(ctx); err != nil {
				return errSetupDraftInvalid
			}
			snapshot, err := c.store.Snapshot()
			if err != nil || !validSetupDraftSnapshot(snapshot) {
				return errSetupDraftStore
			}
			if !c.currentTime().Before(review.ExpiresAt) {
				return errSetupDraftExpired
			}
			return validateSetupDraftApprovalSnapshot(snapshot, review)
		})
	})
	if err != nil {
		if errors.Is(err, errSetupDraftQueueClaimFailed) {
			return SetupDraftApprovalResult{NextAction: setupDraftApprovalClaimUncertainNextAction}, setupDraftDecisionError(err)
		}
		err = setupDraftDecisionError(err)
		if errors.Is(err, errSetupDraftStale) {
			return SetupDraftApprovalResult{Stale: true, NextAction: "Settings changed after review. Submit a new setup draft."}, nil
		}
		return SetupDraftApprovalResult{}, err
	}

	var committed bool
	err = withConfigLock(func() error {
		if err := checkSetupDraftContext(ctx); err != nil {
			return errSetupDraftInvalid
		}
		snapshot, err := c.store.Snapshot()
		if err != nil || !validSetupDraftSnapshot(snapshot) {
			return errSetupDraftStore
		}
		if snapshot.Revision != review.BaseRevision {
			return errSetupDraftStale
		}
		plan, err := buildSetupDraftPlan(snapshot, review.Proposal, setupDraftResourceIDGenerator(review.ID))
		if err != nil || !sameSetupDraftReviewPlan(review, plan) {
			return errSetupDraftInvalid
		}
		if plan.blockingMissing {
			return errSetupDraftMissing
		}
		if !c.currentTime().Before(review.ExpiresAt) {
			return errSetupDraftExpired
		}
		if err := checkSetupDraftContext(ctx); err != nil {
			return errSetupDraftInvalid
		}
		newRevision, err := c.store.CompareAndSwap(snapshot.Revision, cloneSetupDraftState(plan.nextState))
		if err != nil {
			return err
		}
		if !validSetupDraftRevision(newRevision) || newRevision == snapshot.Revision {
			return errSetupDraftStore
		}
		committed = true
		return nil
	})
	if err != nil {
		decisionErr := setupDraftDecisionError(err)
		return SetupDraftApprovalResult{
			Stale:      errors.Is(decisionErr, errSetupDraftStale),
			NextAction: setupDraftApprovalClaimedWarningNextAction,
		}, decisionErr
	}
	if !committed {
		return SetupDraftApprovalResult{NextAction: setupDraftApprovalClaimedWarningNextAction}, errSetupDraftStore
	}
	return SetupDraftApprovalResult{Applied: true}, nil
}

func validateSetupDraftApprovalSnapshot(snapshot setupDraftConfigSnapshot, review SetupDraftReview) error {
	if !validSetupDraftSnapshot(snapshot) {
		return errSetupDraftStore
	}
	if snapshot.Revision != review.BaseRevision {
		return errSetupDraftStale
	}
	plan, err := buildSetupDraftPlan(snapshot, review.Proposal, setupDraftResourceIDGenerator(review.ID))
	if err != nil || !sameSetupDraftReviewPlan(review, plan) {
		return errSetupDraftInvalid
	}
	if plan.blockingMissing {
		return errSetupDraftMissing
	}
	return nil
}

func (c *setupDraftController) RejectSetupDraft(ctx context.Context, id, baseRevision, digest string) error {
	if ctx == nil || ctx.Err() != nil || c == nil || c.store == nil || c.queue == nil || !validSetupDraftID(id) {
		return errSetupDraftInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	err := c.queue.WithReview(id, c.currentTime(), func(review SetupDraftReview) (bool, error) {
		if !c.currentTime().Before(review.ExpiresAt) {
			return false, errSetupDraftExpired
		}
		if review.ID != id || review.BaseRevision != baseRevision || review.Digest != digest || review.Digest != setupDraftReviewDigest(review) {
			return false, errSetupDraftInvalid
		}
		if err := checkSetupDraftContext(ctx); err != nil {
			return false, errSetupDraftInvalid
		}
		err := withConfigLock(func() error {
			if err := checkSetupDraftContext(ctx); err != nil {
				return errSetupDraftInvalid
			}
			snapshot, err := c.store.Snapshot()
			if err != nil || !validSetupDraftSnapshot(snapshot) {
				return errSetupDraftStore
			}
			if snapshot.Revision != review.BaseRevision {
				return errSetupDraftStale
			}
			return nil
		})
		if err != nil {
			return false, err
		}
		return true, nil
	})
	return setupDraftDecisionError(err)
}

func setupDraftDecisionError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errSetupDraftQueueReviewMissing), errors.Is(err, errSetupDraftQueueInvalid):
		return errSetupDraftInvalid
	case errors.Is(err, errSetupDraftQueueReviewMismatch):
		return errSetupDraftInvalid
	case errors.Is(err, errSetupDraftQueueReviewExpired):
		return errSetupDraftExpired
	case errors.Is(err, errSetupDraftQueueClaimFailed), errors.Is(err, errSetupDraftQueueConsumeFailed),
		errors.Is(err, errSetupDraftQueueUnavailable), errors.Is(err, errSetupDraftQueueCapacity):
		return errSetupDraftStore
	case errors.Is(err, errSetupDraftInvalid), errors.Is(err, errSetupDraftExpired),
		errors.Is(err, errSetupDraftStale), errors.Is(err, errSetupDraftMissing),
		errors.Is(err, errSetupDraftStore):
		return err
	default:
		return errSetupDraftStore
	}
}

func (c *setupDraftController) readSnapshotLocked(ctx context.Context) (setupDraftConfigSnapshot, error) {
	if c == nil || c.store == nil || checkSetupDraftContext(ctx) != nil {
		return setupDraftConfigSnapshot{}, errSetupDraftStore
	}
	var snapshot setupDraftConfigSnapshot
	err := withConfigLock(func() error {
		if checkSetupDraftContext(ctx) != nil {
			return errSetupDraftInvalid
		}
		var err error
		snapshot, err = c.store.Snapshot()
		if err != nil || !validSetupDraftSnapshot(snapshot) {
			return errSetupDraftStore
		}
		snapshot = cloneSetupDraftSnapshot(snapshot)
		return nil
	})
	if err != nil {
		return setupDraftConfigSnapshot{}, errSetupDraftStore
	}
	return snapshot, nil
}

func (c *setupDraftController) findDraftLocked(id string) (SetupDraftReview, bool, error) {
	if c == nil || c.queue == nil {
		return SetupDraftReview{}, false, errSetupDraftStore
	}
	reviews, err := c.queue.Load(c.currentTime())
	if err != nil {
		return SetupDraftReview{}, false, err
	}
	for _, review := range reviews {
		if review.ID == id {
			return cloneSetupDraftReview(review), true, nil
		}
	}
	return SetupDraftReview{}, false, nil
}

func (c *setupDraftController) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now().UTC()
	}
	return time.Now().UTC()
}

func (c *setupDraftController) draftIDGenerator() func() (string, error) {
	if c != nil && c.newDraftID != nil {
		return c.newDraftID
	}
	return func() (string, error) { return newTargetID("setupdraft") }
}

func setupDraftResourceIDGenerator(draftID string) func(string) (string, error) {
	ordinal := 0
	return func(kind string) (string, error) {
		ordinal++
		digest := sha256.Sum256([]byte(draftID + "\x00" + kind + "\x00" + strconv.Itoa(ordinal)))
		suffix := hex.EncodeToString(digest[:16])
		switch kind {
		case "ssh_target":
			return "target_" + suffix, nil
		case "service":
			return "service:" + suffix, nil
		default:
			return kind + ":" + suffix, nil
		}
	}
}

func sameSetupDraftReviewPlan(review SetupDraftReview, plan setupDraftPlan) bool {
	if len(review.Changes) != len(plan.changes) || !equalSetupDraftTextLists(review.Missing, plan.missing) {
		return false
	}
	for index, change := range review.Changes {
		replanned := plan.changes[index]
		if change.Kind != replanned.Kind || change.Name != replanned.Name || change.Summary != replanned.Summary ||
			!equalSetupDraftTextLists(change.Scope, replanned.Scope) ||
			!equalSetupDraftTextLists(change.PermissionIncrease, replanned.PermissionIncrease) {
			return false
		}
	}
	return true
}

func equalSetupDraftTextLists(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func setupDraftReviewDigest(review SetupDraftReview) string {
	payload := struct {
		ID           string               `json:"id"`
		BaseRevision string               `json:"base_revision"`
		ExpiresAt    time.Time            `json:"expires_at"`
		Sources      []SetupDraftSource   `json:"sources"`
		Missing      []string             `json:"missing"`
		Changes      []SetupDraftChange   `json:"changes"`
		Proposal     SetupDraftSubmission `json:"proposal"`
	}{review.ID, review.BaseRevision, review.ExpiresAt.UTC(), review.Sources, review.Missing, review.Changes, review.Proposal}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validSetupDraftID(id string) bool {
	if !strings.HasPrefix(id, "setupdraft:") {
		return false
	}
	suffix := strings.TrimPrefix(id, "setupdraft:")
	if len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func checkSetupDraftContext(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return errSetupDraftInvalid
	}
	return nil
}

func cloneSetupDraftSubmission(submission SetupDraftSubmission) SetupDraftSubmission {
	result := submission
	result.Sources = append([]SetupDraftSource(nil), submission.Sources...)
	result.Missing = append([]string(nil), submission.Missing...)
	result.Connections = append([]SetupDraftConnection(nil), submission.Connections...)
	result.Bundles = make([]SetupDraftBundle, len(submission.Bundles))
	for index, bundle := range submission.Bundles {
		result.Bundles[index] = cloneSetupDraftBundle(bundle)
	}
	result.SSHTargets = make([]SetupDraftSSHTarget, len(submission.SSHTargets))
	for index, target := range submission.SSHTargets {
		result.SSHTargets[index] = cloneSetupDraftSSHTarget(target)
	}
	return result
}

func cloneSetupDraftChanges(changes []SetupDraftChange) []SetupDraftChange {
	result := make([]SetupDraftChange, len(changes))
	for index, change := range changes {
		result[index] = change
		result[index].Scope = append([]string(nil), change.Scope...)
		result[index].PermissionIncrease = append([]string(nil), change.PermissionIncrease...)
	}
	return result
}

func cloneSetupDraftReview(review SetupDraftReview) SetupDraftReview {
	result := review
	result.Sources = append([]SetupDraftSource(nil), review.Sources...)
	result.Missing = append([]string(nil), review.Missing...)
	result.Changes = cloneSetupDraftChanges(review.Changes)
	result.Proposal = cloneSetupDraftSubmission(review.Proposal)
	return result
}

type setupDraftPlan struct {
	nextState       setupDraftState
	changes         []SetupDraftChange
	missing         []string
	blockingMissing bool
}

func validSetupDraftSnapshot(snapshot setupDraftConfigSnapshot) bool {
	if !validSetupDraftRevision(snapshot.Revision) || !validSetupDraftState(snapshot.State) {
		return false
	}
	secretNames := make(map[string]bool, len(snapshot.AvailableCredentials))
	for _, secret := range snapshot.AvailableCredentials {
		if !namedSecretIDPattern.MatchString(secret.ID) || !validSetupDraftText(secret.Name, maxNamedSecretName, false) ||
			!validSetupDraftText(secret.Purpose, maxNamedSecretPurpose, true) || secretNames[strings.ToLower(secret.Name)] {
			return false
		}
		secretNames[strings.ToLower(secret.Name)] = true
	}
	return true
}

func validSetupDraftState(state setupDraftState) bool {
	if len(state.Connections) > maxSetupDraftConnections*4 || len(state.Bundles) > maxSetupDraftBundles*4 || len(state.SSHTargets) > maxSSHTargetOperations {
		return false
	}
	connectionIDs, connectionNames := make(map[string]bool), make(map[string]bool)
	githubIDs, jenkinsIDs, harborIDs, dashboardIDs := make(map[string]bool), make(map[string]bool), make(map[string]bool), make(map[string]bool)
	for _, record := range state.Connections {
		connection := connectionDraftValue(record)
		normalized, err := normalizeSetupDraftConnection(connection)
		if err != nil || normalized != connection || connectionIDs[record.ID] || connectionNames[string(record.Kind)+"\x00"+strings.ToLower(record.Name)] {
			return false
		}
		connectionIDs[record.ID], connectionNames[string(record.Kind)+"\x00"+strings.ToLower(record.Name)] = true, true
		switch record.Kind {
		case SetupServiceGitHub:
			githubIDs[record.ID] = true
		case SetupServiceJenkins:
			jenkinsIDs[record.ID] = true
		case SetupServiceHarbor:
			harborIDs[record.ID] = true
		case SetupServiceDashboard:
			dashboardIDs[record.ID] = true
		}
	}
	bundleIDs, bundleNames := make(map[string]bool), make(map[string]bool)
	for _, record := range state.Bundles {
		bundle, err := normalizeSetupDraftBundle(record.Definition)
		if !serviceBundleIDPattern.MatchString(record.ID) || err != nil || !equalSetupDraftBundle(bundle, record.Definition) || bundleIDs[record.ID] || bundleNames[strings.ToLower(bundle.Name)] {
			return false
		}
		bundleIDs[record.ID], bundleNames[strings.ToLower(bundle.Name)] = true, true
		for _, id := range bundle.RepositoryIDs {
			if !githubIDs[id] {
				return false
			}
		}
		for _, environment := range bundle.Environments {
			if environment.JenkinsTargetID != "" && !jenkinsIDs[environment.JenkinsTargetID] ||
				environment.HarborTargetID != "" && !harborIDs[environment.HarborTargetID] ||
				environment.DashboardTargetID != "" && !dashboardIDs[environment.DashboardTargetID] {
				return false
			}
		}
	}
	sshIDs, sshNames, sshAliases := make(map[string]bool), make(map[string]bool), make(map[string]bool)
	for _, record := range state.SSHTargets {
		definition, err := normalizeSSHTarget(sshTargetDefinition{
			ID: record.ID, Name: record.Definition.Name, Alias: record.Definition.Alias,
			Operations: record.Definition.Operations,
		})
		if err != nil || sshIDs[record.ID] || sshNames[strings.ToLower(record.Definition.Name)] || sshAliases[strings.ToLower(record.Definition.Alias)] {
			return false
		}
		for _, operation := range definition.Operations {
			if !isStoredSSHOperationValid(operation, record.ID) {
				return false
			}
		}
		sshIDs[record.ID], sshNames[strings.ToLower(record.Definition.Name)], sshAliases[strings.ToLower(record.Definition.Alias)] = true, true, true
	}
	return true
}

func buildSetupDraftPlan(snapshot setupDraftConfigSnapshot, proposal SetupDraftSubmission, newID func(string) (string, error)) (setupDraftPlan, error) {
	if newID == nil {
		return setupDraftPlan{}, errSetupDraftInvalid
	}
	plan := setupDraftPlan{nextState: cloneSetupDraftState(snapshot.State), missing: append([]string(nil), proposal.Missing...)}
	secretNames := make(map[string]bool, len(snapshot.AvailableCredentials))
	for _, secret := range snapshot.AvailableCredentials {
		if secret.Configured {
			secretNames[strings.ToLower(secret.Name)] = true
		}
	}
	touchedConnections := make(map[string]bool)
	for _, connection := range proposal.Connections {
		index := findSetupDraftConnectionIndex(plan.nextState.Connections, connection.Kind, connection.ExistingID)
		switch connection.Action {
		case SetupDraftAdd:
			if findSetupDraftConnectionName(plan.nextState.Connections, connection.Kind, connection.Name) >= 0 {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			id, err := newID(string(connection.Kind))
			if err != nil || !validSetupDraftConnectionID(connection.Kind, id) || findSetupDraftConnectionIndex(plan.nextState.Connections, connection.Kind, id) >= 0 {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			if connection.CredentialName == "" {
				plan.addMissing("Select a saved named credential for " + connection.Name + ".")
				plan.blockingMissing = true
			} else if !secretNames[strings.ToLower(connection.CredentialName)] {
				plan.addMissing("The named credential for " + connection.Name + " is not registered or configured.")
				plan.blockingMissing = true
			}
			plan.nextState.Connections = append(plan.nextState.Connections, connectionDraftRecord(id, connection))
			plan.changes = append(plan.changes, setupDraftConnectionChange("connection_add", connectionDraftRecord(id, connection), nil))
		case SetupDraftUpdate:
			if index < 0 || touchedConnections[string(connection.Kind)+"\x00"+connection.ExistingID] {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			touchedConnections[string(connection.Kind)+"\x00"+connection.ExistingID] = true
			old := plan.nextState.Connections[index]
			if conflict := findSetupDraftConnectionName(plan.nextState.Connections, connection.Kind, connection.Name); conflict >= 0 && conflict != index {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			if connection.CredentialName == "" {
				plan.addMissing("Select a saved named credential for " + connection.Name + ".")
				plan.blockingMissing = true
			} else if !secretNames[strings.ToLower(connection.CredentialName)] {
				plan.addMissing("The named credential for " + connection.Name + " is not registered or configured.")
				plan.blockingMissing = true
			}
			updated := connectionDraftRecord(old.ID, connection)
			if !equalSetupDraftConnectionRecord(old, updated) {
				plan.nextState.Connections[index] = updated
				plan.changes = append(plan.changes, setupDraftConnectionChange("connection_update", updated, &old))
			}
		case SetupDraftReuse:
			if index < 0 || touchedConnections[string(connection.Kind)+"\x00"+connection.ExistingID] || !equalSetupDraftConnectionRecord(plan.nextState.Connections[index], connectionDraftRecord(connection.ExistingID, connection)) {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			touchedConnections[string(connection.Kind)+"\x00"+connection.ExistingID] = true
		default:
			return setupDraftPlan{}, errSetupDraftInvalid
		}
	}

	for _, bundle := range proposal.Bundles {
		missing := setupDraftBundleMissingReferences(bundle, plan.nextState)
		if len(missing) > 0 {
			for _, entry := range missing {
				plan.addMissing(entry)
			}
			plan.blockingMissing = true
			blocked := setupDraftBundleChange("bundle_pending_mapping", bundle, nil)
			blocked.Summary = "Bundle " + bundle.Name + " needs registered targets before it can be saved."
			blocked.PermissionIncrease = nil
			plan.changes = append(plan.changes, normalizeSetupDraftChange(blocked))
			continue
		}
		index := findSetupDraftBundleIndex(plan.nextState.Bundles, bundle.Name)
		if index < 0 {
			id, err := newID("service")
			if err != nil || !serviceBundleIDPattern.MatchString(id) {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			record := setupDraftBundleRecord{ID: id, Definition: cloneSetupDraftBundle(bundle)}
			plan.nextState.Bundles = append(plan.nextState.Bundles, record)
			plan.changes = append(plan.changes, setupDraftBundleChange("bundle_add", record.Definition, nil))
			continue
		}
		old := plan.nextState.Bundles[index]
		if !equalSetupDraftBundle(old.Definition, bundle) {
			updated := setupDraftBundleRecord{ID: old.ID, Definition: cloneSetupDraftBundle(bundle)}
			plan.nextState.Bundles[index] = updated
			plan.changes = append(plan.changes, setupDraftBundleChange("bundle_update", updated.Definition, &old.Definition))
		}
	}

	for _, target := range proposal.SSHTargets {
		index := findSetupDraftSSHTargetIndex(plan.nextState.SSHTargets, target.Name)
		id := ""
		if index >= 0 {
			id = plan.nextState.SSHTargets[index].ID
		} else {
			var err error
			id, err = newID("ssh_target")
			if err != nil || !validSSHDefinitionID(id) {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
		}
		if conflict := findSetupDraftSSHTargetAlias(plan.nextState.SSHTargets, target.Alias); conflict >= 0 && conflict != index {
			return setupDraftPlan{}, errSetupDraftInvalid
		}
		updated := cloneSetupDraftSSHTarget(target)
		updatedOperations := make([]SSHOperationDefinition, len(target.Operations))
		for operationIndex, operation := range target.Operations {
			operation.Revision = ""
			operation.Approval.Revision = ""
			operation.Approval.Scope = nil
			normalized, err := normalizeSSHOperationDefinition(operation, id)
			if err != nil || normalized.Approval.Mode == OperationApprovalPreapproved {
				return setupDraftPlan{}, errSetupDraftInvalid
			}
			updatedOperations[operationIndex] = normalized
		}
		updated.Operations = updatedOperations
		if index < 0 {
			record := setupDraftSSHTargetRecord{ID: id, Definition: updated}
			plan.nextState.SSHTargets = append(plan.nextState.SSHTargets, record)
			plan.changes = append(plan.changes, setupDraftSSHChange("ssh_target_add", record, nil))
			continue
		}
		old := plan.nextState.SSHTargets[index]
		if !equalSetupDraftSSHTarget(old.Definition, updated) {
			record := setupDraftSSHTargetRecord{ID: id, Definition: updated}
			plan.nextState.SSHTargets[index] = record
			plan.changes = append(plan.changes, setupDraftSSHChange("ssh_target_update", record, &old))
		}
	}

	if !validSetupDraftState(plan.nextState) {
		return setupDraftPlan{}, errSetupDraftInvalid
	}
	plan.missing = uniqueSortedSetupDraftText(plan.missing)
	return plan, nil
}

func (plan *setupDraftPlan) addMissing(value string) {
	if validSetupDraftText(value, 200, false) {
		plan.missing = append(plan.missing, value)
	}
}

func validSetupDraftConnectionID(kind SetupServiceKind, id string) bool {
	switch kind {
	case SetupServiceGitHub:
		return githubIDPattern.MatchString(id)
	case SetupServiceJenkins:
		return jenkinsIDPattern.MatchString(id)
	case SetupServiceHarbor:
		return harborIDPattern.MatchString(id)
	case SetupServiceDashboard:
		return dashboardIDPattern.MatchString(id)
	default:
		return false
	}
}

func findSetupDraftConnectionIndex(connections []setupDraftConnectionRecord, kind SetupServiceKind, id string) int {
	for index, connection := range connections {
		if connection.Kind == kind && connection.ID == id {
			return index
		}
	}
	return -1
}

func findSetupDraftConnectionName(connections []setupDraftConnectionRecord, kind SetupServiceKind, name string) int {
	for index, connection := range connections {
		if connection.Kind == kind && strings.EqualFold(connection.Name, name) {
			return index
		}
	}
	return -1
}

func findSetupDraftBundleIndex(bundles []setupDraftBundleRecord, name string) int {
	for index, bundle := range bundles {
		if strings.EqualFold(bundle.Definition.Name, name) {
			return index
		}
	}
	return -1
}

func findSetupDraftSSHTargetIndex(targets []setupDraftSSHTargetRecord, name string) int {
	for index, target := range targets {
		if strings.EqualFold(target.Definition.Name, name) {
			return index
		}
	}
	return -1
}

func findSetupDraftSSHTargetAlias(targets []setupDraftSSHTargetRecord, alias string) int {
	for index, target := range targets {
		if strings.EqualFold(target.Definition.Alias, alias) {
			return index
		}
	}
	return -1
}

func setupDraftConnectionChange(kind string, record setupDraftConnectionRecord, before *setupDraftConnectionRecord) SetupDraftChange {
	change := SetupDraftChange{Kind: kind, Name: record.Name, Summary: setupDraftConnectionSummary(kind, record), Scope: setupDraftConnectionScope(record)}
	if before == nil {
		change.PermissionIncrease = setupServiceActions(record.Kind)
		if record.CredentialName == "" {
			change.PermissionIncrease = append(change.PermissionIncrease, "Credential mapping is missing.")
		} else {
			change.PermissionIncrease = append(change.PermissionIncrease, "Credential mapping: "+record.CredentialName)
		}
		return normalizeSetupDraftChange(change)
	}
	oldScope := stringSet(setupDraftConnectionScope(*before))
	for _, item := range setupDraftConnectionScope(record) {
		if !oldScope[item] {
			change.PermissionIncrease = append(change.PermissionIncrease, "New connection scope: "+item)
		}
	}
	if before.CredentialName != record.CredentialName {
		if record.CredentialName == "" {
			change.PermissionIncrease = append(change.PermissionIncrease, "Credential mapping was cleared.")
		} else {
			change.PermissionIncrease = append(change.PermissionIncrease, "Credential mapping changed to: "+record.CredentialName)
		}
	}
	return normalizeSetupDraftChange(change)
}

func setupDraftConnectionSummary(kind string, connection setupDraftConnectionRecord) string {
	verb := "Update"
	if kind == "connection_add" {
		verb = "Add"
	}
	return verb + " registered " + string(connection.Kind) + " target " + connection.Name + "."
}

func setupDraftConnectionScope(connection setupDraftConnectionRecord) []string {
	scope := []string{"target: " + connection.Name}
	switch connection.Kind {
	case SetupServiceGitHub:
		scope = append(scope, "origin: "+connection.Origin, "repository: "+connection.Repository)
	case SetupServiceJenkins:
		scope = append(scope, "base URL: "+connection.BaseURL, "job: "+connection.JobPath)
		if connection.Environment != "" {
			scope = append(scope, "environment label: "+connection.Environment)
		}
	case SetupServiceHarbor:
		scope = append(scope, "base URL: "+connection.BaseURL, "project: "+connection.Project, "repository: "+connection.Repository)
	case SetupServiceDashboard:
		scope = append(scope, "base URL: "+connection.BaseURL)
	}
	if connection.CredentialName != "" {
		scope = append(scope, "credential name: "+connection.CredentialName)
	}
	return uniqueSortedSetupDraftText(scope)
}

func setupServiceActions(kind SetupServiceKind) []string {
	switch kind {
	case SetupServiceGitHub:
		return []string{"github_repository", "github_pull_request", "registered_target_connection_test"}
	case SetupServiceJenkins:
		return []string{"jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log", "jenkins_run_registered_job"}
	case SetupServiceHarbor:
		return []string{"harbor_repository_artifacts", "harbor_project_quota", "registered_target_connection_test"}
	case SetupServiceDashboard:
		return []string{"dashboard_namespaces", "registered_target_connection_test"}
	default:
		return nil
	}
}

func setupDraftBundleMissingReferences(bundle SetupDraftBundle, state setupDraftState) []string {
	missing := []string{}
	githubIDs, jenkinsIDs, harborIDs, dashboardIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, connection := range state.Connections {
		switch connection.Kind {
		case SetupServiceGitHub:
			githubIDs[connection.ID] = true
		case SetupServiceJenkins:
			jenkinsIDs[connection.ID] = true
		case SetupServiceHarbor:
			harborIDs[connection.ID] = true
		case SetupServiceDashboard:
			dashboardIDs[connection.ID] = true
		}
	}
	for _, id := range bundle.RepositoryIDs {
		if !githubIDs[id] {
			missing = append(missing, "Bundle "+bundle.Name+" references an unregistered GitHub target.")
		}
	}
	for _, environment := range bundle.Environments {
		if environment.JenkinsTargetID != "" && !jenkinsIDs[environment.JenkinsTargetID] ||
			environment.HarborTargetID != "" && !harborIDs[environment.HarborTargetID] ||
			environment.DashboardTargetID != "" && !dashboardIDs[environment.DashboardTargetID] {
			missing = append(missing, "Environment "+environment.Name+" references an unregistered service target.")
		}
	}
	return uniqueSortedSetupDraftText(missing)
}

func setupDraftBundleChange(kind string, bundle SetupDraftBundle, before *SetupDraftBundle) SetupDraftChange {
	change := SetupDraftChange{Kind: kind, Name: bundle.Name, Summary: "Add service bundle " + bundle.Name + ".", Scope: setupDraftBundleScope(bundle)}
	if before == nil {
		change.PermissionIncrease = []string{"Make the listed registered service scopes discoverable through this bundle."}
		return normalizeSetupDraftChange(change)
	}
	change.Summary = "Update service bundle " + bundle.Name + "."
	oldScope := stringSet(setupDraftBundleScope(*before))
	for _, item := range setupDraftBundleScope(bundle) {
		if !oldScope[item] {
			change.PermissionIncrease = append(change.PermissionIncrease, "New bundle mapping: "+item)
		}
	}
	return normalizeSetupDraftChange(change)
}

func setupDraftBundleScope(bundle SetupDraftBundle) []string {
	scope := []string{"bundle: " + bundle.Name}
	for _, id := range bundle.RepositoryIDs {
		scope = append(scope, "GitHub target: "+id)
	}
	for _, environment := range bundle.Environments {
		scope = append(scope, "environment: "+environment.Name)
		if environment.JenkinsTargetID != "" {
			scope = append(scope, "Jenkins target: "+environment.JenkinsTargetID)
		}
		if environment.HarborTargetID != "" {
			scope = append(scope, "Harbor target: "+environment.HarborTargetID)
		}
		if environment.DashboardTargetID != "" {
			scope = append(scope, "Dashboard target: "+environment.DashboardTargetID)
		}
		if environment.DashboardNamespace != "" {
			scope = append(scope, "Dashboard namespace: "+environment.DashboardNamespace, "Dashboard deployment: "+environment.DashboardDeployment)
		}
	}
	return uniqueSortedSetupDraftText(scope)
}

func setupDraftSSHChange(kind string, record setupDraftSSHTargetRecord, before *setupDraftSSHTargetRecord) SetupDraftChange {
	change := SetupDraftChange{Kind: kind, Name: record.Definition.Name, Summary: "Add registered SSH target " + record.Definition.Name + ".", Scope: setupDraftSSHScope(record.Definition)}
	if before == nil {
		for _, operation := range record.Definition.Operations {
			change.PermissionIncrease = append(change.PermissionIncrease, "SSH operation added: "+operation.Name+" ("+string(operation.Risk)+")")
		}
		return normalizeSetupDraftChange(change)
	}
	change.Summary = "Update registered SSH target " + record.Definition.Name + "."
	if before.Definition.Alias != record.Definition.Alias {
		change.PermissionIncrease = append(change.PermissionIncrease, "SSH alias changed; review the associated host and trust configuration.")
	}
	oldOperations := make(map[string]SSHOperationDefinition, len(before.Definition.Operations))
	for _, operation := range before.Definition.Operations {
		oldOperations[operation.ID] = operation
	}
	for _, operation := range record.Definition.Operations {
		old, exists := oldOperations[operation.ID]
		if !exists || old.Revision != operation.Revision {
			change.PermissionIncrease = append(change.PermissionIncrease, "SSH operation added or changed: "+operation.Name+" ("+string(operation.Risk)+")")
		}
	}
	return normalizeSetupDraftChange(change)
}

func setupDraftSSHScope(target SetupDraftSSHTarget) []string {
	scope := []string{"SSH alias: " + target.Alias, "SSH target: " + target.Name}
	for _, operation := range target.Operations {
		scope = append(scope, "operation: "+operation.Name+" ("+string(operation.Risk)+")")
	}
	return uniqueSortedSetupDraftText(scope)
}

func normalizeSetupDraftChange(change SetupDraftChange) SetupDraftChange {
	change.Scope = uniqueSortedSetupDraftText(change.Scope)
	change.PermissionIncrease = uniqueSortedSetupDraftText(change.PermissionIncrease)
	return change
}

func uniqueSortedSetupDraftText(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func stringSet(values []string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func equalSetupDraftConnectionRecord(left, right setupDraftConnectionRecord) bool {
	return left == right
}

func equalSetupDraftBundle(left, right SetupDraftBundle) bool {
	leftBytes, leftErr := json.Marshal(cloneSetupDraftBundle(left))
	rightBytes, rightErr := json.Marshal(cloneSetupDraftBundle(right))
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}

func equalSetupDraftSSHTarget(left, right SetupDraftSSHTarget) bool {
	leftBytes, leftErr := json.Marshal(cloneSetupDraftSSHTarget(left))
	rightBytes, rightErr := json.Marshal(cloneSetupDraftSSHTarget(right))
	return leftErr == nil && rightErr == nil && string(leftBytes) == string(rightBytes)
}
