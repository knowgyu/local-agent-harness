package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	onboardingStoreDirectoryName  = "onboarding"
	onboardingStoreFileName       = "connection-onboarding.json"
	onboardingStoreVersion        = 1
	onboardingStoreMaxFileBytes   = 256 << 10
	onboardingStoreMaxDrafts      = 50
	onboardingStoreMaxActivations = 256
	onboardingStoreMaxFirstReads  = 256
)

var errOnboardingCapacity = errors.New("registration onboarding capacity is full")

var onboardingDraftIDPattern = regexp.MustCompile(`^onboarding:[a-f0-9]{32}$`)

type onboardingFileDraftStore struct {
	path                string
	now                 func() time.Time
	writeFileAtomically func(string, []byte) error
}

type onboardingStoreEnvelope struct {
	Version            int                           `json:"version"`
	Drafts             []onboardingDraft             `json:"drafts"`
	PendingActivations []onboardingPendingActivation `json:"pending_activations"`
	Activations        []onboardingActivationRecord  `json:"activations"`
	FirstReads         []onboardingFirstReadReceipt  `json:"first_reads"`
}

type onboardingPendingActivation struct {
	DraftID          string `json:"draft_id"`
	ExpectedRevision uint64 `json:"expected_revision"`
	CheckGeneration  uint64 `json:"check_generation"`
	CandidateDigest  string `json:"candidate_digest"`
	PolicyDigest     string `json:"policy_digest"`
	ConfigRevision   string `json:"config_revision"`
	TargetID         string `json:"target_id"`
	// RecoveryRequired marks the explicit save-helper outcome where a newly
	// created secret may not have been cleaned up. It contains no secret data.
	RecoveryRequired bool `json:"recovery_required,omitempty"`
}

func newOnboardingDraftStoreForConfigPath(configPath string) (onboardingDraftStore, error) {
	if configPath == "" || !filepath.IsAbs(configPath) {
		return nil, errOnboardingUnavailable
	}
	cleanConfigPath := filepath.Clean(configPath)
	if filepath.Dir(cleanConfigPath) == cleanConfigPath || filepath.Base(cleanConfigPath) == "" {
		return nil, errOnboardingUnavailable
	}
	absoluteConfigPath, err := filepath.Abs(cleanConfigPath)
	if err != nil {
		return nil, errOnboardingUnavailable
	}
	normalizedConfigPath := filepath.Clean(absoluteConfigPath)
	if runtime.GOOS == "windows" {
		normalizedConfigPath = strings.ToLower(normalizedConfigPath)
	}
	pathDigest := sha256.Sum256([]byte(normalizedConfigPath))
	path := filepath.Join(filepath.Dir(cleanConfigPath), onboardingStoreDirectoryName, hex.EncodeToString(pathDigest[:]), onboardingStoreFileName)
	return newOnboardingFileDraftStore(path, time.Now)
}

func newOnboardingFileDraftStoreForTest(path string, now func() time.Time) (*onboardingFileDraftStore, error) {
	return newOnboardingFileDraftStore(path, now)
}

func newOnboardingFileDraftStore(path string, now func() time.Time) (*onboardingFileDraftStore, error) {
	if !validOnboardingStorePath(path) || now == nil {
		return nil, errOnboardingUnavailable
	}
	cleanPath := filepath.Clean(path)
	if cleanPath == string(filepath.Separator) || filepath.Dir(cleanPath) == cleanPath {
		return nil, errOnboardingUnavailable
	}
	return &onboardingFileDraftStore{path: cleanPath, now: now}, nil
}

func validOnboardingStorePath(path string) bool {
	if path == "" || !filepath.IsAbs(path) || filepath.Base(path) != onboardingStoreFileName {
		return false
	}
	namespace := filepath.Base(filepath.Dir(path))
	if len(namespace) != 64 || strings.ToLower(namespace) != namespace {
		return false
	}
	if _, err := hex.DecodeString(namespace); err != nil || filepath.Base(filepath.Dir(filepath.Dir(path))) != onboardingStoreDirectoryName {
		return false
	}
	return true
}

func (s *onboardingFileDraftStore) ListDrafts(ctx context.Context) ([]onboardingDraft, error) {
	if err := s.validateCall(ctx); err != nil {
		return nil, err
	}
	var drafts []onboardingDraft
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		drafts = cloneOnboardingDrafts(envelope.Drafts)
		applyOnboardingRecoveryState(drafts, envelope.PendingActivations)
		sort.Slice(drafts, func(i, j int) bool {
			if drafts[i].UpdatedAt.Equal(drafts[j].UpdatedAt) {
				return drafts[i].ID < drafts[j].ID
			}
			return drafts[i].UpdatedAt.After(drafts[j].UpdatedAt)
		})
		return nil
	})
	if err != nil {
		return nil, s.publicError(err)
	}
	return drafts, nil
}

func (s *onboardingFileDraftStore) GetDraft(ctx context.Context, id string) (onboardingDraft, error) {
	if err := s.validateCall(ctx); err != nil || !validOnboardingDraftID(id) {
		return onboardingDraft{}, errOnboardingInvalid
	}
	var draft onboardingDraft
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		index := onboardingDraftIndex(envelope.Drafts, id)
		if index < 0 {
			return errOnboardingDraftNotFound
		}
		draft = cloneOnboardingDraft(envelope.Drafts[index])
		if onboardingPendingRecoveryRequired(envelope.PendingActivations, id) {
			draft.ActivationRecoveryRequired = true
		}
		return nil
	})
	if err != nil {
		return onboardingDraft{}, s.publicError(err)
	}
	return draft, nil
}

func (s *onboardingFileDraftStore) CreateDraft(ctx context.Context, service onboardingServiceKind, fields onboardingDraftFields) (onboardingDraft, error) {
	if err := s.validateCall(ctx); err != nil || !validOnboardingDraftFields(service, fields) {
		return onboardingDraft{}, errOnboardingInvalid
	}
	now := s.currentTime()
	if !validOnboardingTime(now) {
		return onboardingDraft{}, errOnboardingUnavailable
	}
	var created onboardingDraft
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		if len(envelope.Drafts) >= onboardingStoreMaxDrafts {
			return errOnboardingCapacity
		}
		id, err := newOnboardingDraftID()
		if err != nil {
			return errOnboardingUnavailable
		}
		for onboardingDraftIndex(envelope.Drafts, id) >= 0 {
			id, err = newOnboardingDraftID()
			if err != nil {
				return errOnboardingUnavailable
			}
		}
		created = onboardingDraft{
			ID: id, Revision: 1, Service: service, Fields: fields,
			CreatedAt: now, UpdatedAt: now,
		}
		envelope.Drafts = append(envelope.Drafts, created)
		return s.writeLocked(envelope)
	})
	if err != nil {
		return onboardingDraft{}, s.publicError(err)
	}
	return cloneOnboardingDraft(created), nil
}

func (s *onboardingFileDraftStore) UpdateDraft(ctx context.Context, id string, expectedRevision uint64, fields onboardingDraftFields) (onboardingDraft, error) {
	if err := s.validateCall(ctx); err != nil || !validOnboardingDraftID(id) || expectedRevision == 0 {
		return onboardingDraft{}, errOnboardingInvalid
	}
	now := s.currentTime()
	if !validOnboardingTime(now) {
		return onboardingDraft{}, errOnboardingUnavailable
	}
	var updated onboardingDraft
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		index := onboardingDraftIndex(envelope.Drafts, id)
		if index < 0 {
			return errOnboardingDraftNotFound
		}
		if onboardingPendingIndex(envelope.PendingActivations, id) >= 0 {
			return errOnboardingActivationPending
		}
		current := envelope.Drafts[index]
		if current.Revision != expectedRevision {
			return errOnboardingDraftStale
		}
		if !validOnboardingDraftFields(current.Service, fields) {
			return errOnboardingInvalid
		}
		if current.Revision == math.MaxUint64 {
			return errOnboardingCapacity
		}
		updated = cloneOnboardingDraft(current)
		updated.Revision++
		updated.Fields = fields
		updated.UpdatedAt = now
		updated.Check = nil
		envelope.Drafts[index] = updated
		return s.writeLocked(envelope)
	})
	if err != nil {
		return onboardingDraft{}, s.publicError(err)
	}
	return cloneOnboardingDraft(updated), nil
}

func (s *onboardingFileDraftStore) DeleteDraft(ctx context.Context, id string, expectedRevision uint64) error {
	if err := s.validateCall(ctx); err != nil || !validOnboardingDraftID(id) || expectedRevision == 0 {
		return errOnboardingInvalid
	}
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		index := onboardingDraftIndex(envelope.Drafts, id)
		if index < 0 {
			return errOnboardingDraftNotFound
		}
		if onboardingPendingIndex(envelope.PendingActivations, id) >= 0 {
			return errOnboardingActivationPending
		}
		if envelope.Drafts[index].Revision != expectedRevision {
			return errOnboardingDraftStale
		}
		envelope.Drafts = append(envelope.Drafts[:index], envelope.Drafts[index+1:]...)
		return s.writeLocked(envelope)
	})
	if err != nil {
		return s.publicError(err)
	}
	return nil
}

// StartCheck advances a per-draft attempt generation and clears the previous
// receipt before any caller performs network I/O. This makes a late result from
// an earlier concurrent check fail the receipt CAS.
func (s *onboardingFileDraftStore) StartCheck(ctx context.Context, id string, expectedRevision uint64) (uint64, error) {
	if err := s.validateCall(ctx); err != nil || !validOnboardingDraftID(id) || expectedRevision == 0 {
		return 0, errOnboardingInvalid
	}
	var generation uint64
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		index := onboardingDraftIndex(envelope.Drafts, id)
		if index < 0 {
			return errOnboardingDraftNotFound
		}
		draft := envelope.Drafts[index]
		if draft.Revision != expectedRevision {
			return errOnboardingDraftStale
		}
		if onboardingPendingRecoveryRequired(envelope.PendingActivations, id) {
			return errOnboardingActivationRecoveryRequired
		}
		if draft.CheckGeneration == math.MaxUint64 {
			return errOnboardingCapacity
		}
		draft.CheckGeneration++
		draft.Check = nil
		envelope.Drafts[index] = draft
		if err := s.writeLocked(envelope); err != nil {
			return err
		}
		generation = draft.CheckGeneration
		return nil
	})
	if err != nil {
		return 0, s.publicError(err)
	}
	return generation, nil
}

func (s *onboardingFileDraftStore) SaveCheckReceipt(ctx context.Context, receipt onboardingCheckReceipt) error {
	if err := s.validateCall(ctx); err != nil || !validOnboardingCheckReceiptShape(receipt) {
		return errOnboardingInvalid
	}
	now := s.currentTime()
	if !validOnboardingTime(now) || receipt.CheckedAt.After(now) {
		return errOnboardingInvalid
	}
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		index := onboardingDraftIndex(envelope.Drafts, receipt.DraftID)
		if index < 0 {
			return errOnboardingDraftNotFound
		}
		// A pending activation still accepts a fresh receipt for the same draft
		// revision. Recovery uses it to bind the reserved target ID to the
		// current configuration revision before retrying an idempotent apply.
		draft := envelope.Drafts[index]
		if draft.Revision != receipt.DraftRevision {
			return errOnboardingDraftStale
		}
		if draft.CheckGeneration != receipt.CheckGeneration {
			return errOnboardingCheckRequired
		}
		candidateDigest, digestErr := onboardingCandidateDigest(draft.Service, draft.Fields)
		if digestErr != nil || candidateDigest != receipt.CandidateDigest {
			return errOnboardingReviewChanged
		}
		if !validOnboardingCheckReceiptForDraft(receipt, draft, now) {
			return errOnboardingInvalid
		}
		copy := receipt
		draft.Check = &copy
		if receipt.CheckedAt.After(draft.UpdatedAt) {
			draft.UpdatedAt = receipt.CheckedAt
		}
		envelope.Drafts[index] = draft
		if pendingIndex := onboardingPendingIndex(envelope.PendingActivations, receipt.DraftID); pendingIndex >= 0 {
			pending := envelope.PendingActivations[pendingIndex]
			if !onboardingPendingCandidateMatches(pending, onboardingActivationCAS{
				DraftID: receipt.DraftID, ExpectedRevision: receipt.DraftRevision,
				CandidateDigest: receipt.CandidateDigest, PolicyDigest: receipt.PolicyDigest,
			}) {
				return errOnboardingActivationPending
			}
			pending.ConfigRevision = receipt.ConfigRevision
			pending.CheckGeneration = receipt.CheckGeneration
			envelope.PendingActivations[pendingIndex] = pending
		}
		return s.writeLocked(envelope)
	})
	if err != nil {
		return s.publicError(err)
	}
	return nil
}

func (s *onboardingFileDraftStore) WithActivation(ctx context.Context, cas onboardingActivationCAS, apply func(onboardingDraft, string) error) (onboardingActivationRecord, error) {
	if err := s.validateCall(ctx); err != nil || apply == nil || !validOnboardingActivationCAS(cas) {
		return onboardingActivationRecord{}, errOnboardingInvalid
	}
	var record onboardingActivationRecord
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		if index := onboardingActivationIndex(envelope.Activations, cas.DraftID); index >= 0 {
			previous := envelope.Activations[index]
			if !onboardingActivationMatches(previous, cas) {
				return errOnboardingReviewChanged
			}
			record = previous
			return nil
		}
		index := onboardingDraftIndex(envelope.Drafts, cas.DraftID)
		if index < 0 {
			return errOnboardingDraftNotFound
		}
		draft := envelope.Drafts[index]
		if draft.Revision != cas.ExpectedRevision {
			return errOnboardingDraftStale
		}
		candidateDigest, digestErr := onboardingCandidateDigest(draft.Service, draft.Fields)
		if digestErr != nil || candidateDigest != cas.CandidateDigest {
			return errOnboardingReviewChanged
		}
		if draft.Check == nil || draft.Check.Outcome != onboardingCheckSucceeded {
			return errOnboardingCheckRequired
		}
		if !onboardingActivationMatchesCheck(*draft.Check, cas) {
			return errOnboardingReviewChanged
		}
		if !validOnboardingCheckReceiptForDraft(*draft.Check, draft, s.currentTime()) {
			return errOnboardingCheckRequired
		}
		pendingIndex := onboardingPendingIndex(envelope.PendingActivations, cas.DraftID)
		var targetID string
		activationRetry := pendingIndex >= 0
		if pendingIndex >= 0 {
			pending := envelope.PendingActivations[pendingIndex]
			if !onboardingPendingCandidateMatches(pending, cas) {
				return errOnboardingActivationPending
			}
			if pending.RecoveryRequired {
				return errOnboardingActivationRecoveryRequired
			}
			targetID = pending.TargetID
			if pending.ConfigRevision != cas.ConfigRevision || pending.CheckGeneration != cas.CheckGeneration {
				pending.ConfigRevision = cas.ConfigRevision
				pending.CheckGeneration = cas.CheckGeneration
				envelope.PendingActivations[pendingIndex] = pending
				if err := s.writeLocked(envelope); err != nil {
					return err
				}
			}
		} else {
			if len(envelope.Activations) >= onboardingStoreMaxActivations {
				return errOnboardingCapacity
			}
			targetID, err = newOnboardingTargetID(draft.Service)
			if err != nil {
				return errOnboardingUnavailable
			}
			pending := onboardingPendingActivation{
				DraftID: cas.DraftID, ExpectedRevision: cas.ExpectedRevision,
				CheckGeneration: cas.CheckGeneration,
				CandidateDigest: cas.CandidateDigest, PolicyDigest: cas.PolicyDigest,
				ConfigRevision: cas.ConfigRevision, TargetID: targetID,
			}
			envelope.PendingActivations = append(envelope.PendingActivations, pending)
			if err := s.writeLocked(envelope); err != nil {
				return err
			}
		}
		applyDraft := cloneOnboardingDraft(draft)
		applyDraft.ActivationRetry = activationRetry
		if err := apply(applyDraft, targetID); err != nil {
			if errors.Is(err, errOnboardingActivationAmbiguous) {
				pendingIndex = onboardingPendingIndex(envelope.PendingActivations, cas.DraftID)
				if pendingIndex < 0 {
					return errOnboardingUnavailable
				}
				pending := envelope.PendingActivations[pendingIndex]
				pending.RecoveryRequired = true
				envelope.PendingActivations[pendingIndex] = pending
				if writeErr := s.writeLocked(envelope); writeErr != nil {
					return writeErr
				}
				return errOnboardingActivationRecoveryRequired
			}
			if errors.Is(err, errOnboardingActivationNotCommitted) {
				pendingIndex = onboardingPendingIndex(envelope.PendingActivations, cas.DraftID)
				if pendingIndex < 0 {
					return errOnboardingUnavailable
				}
				envelope.PendingActivations = append(envelope.PendingActivations[:pendingIndex], envelope.PendingActivations[pendingIndex+1:]...)
				if writeErr := s.writeLocked(envelope); writeErr != nil {
					return writeErr
				}
			}
			return errOnboardingUnavailable
		}
		activatedAt := s.currentTime()
		if !validOnboardingTime(activatedAt) {
			return errOnboardingUnavailable
		}
		record = onboardingActivationRecord{
			DraftID: draft.ID, TargetID: targetID, Service: draft.Service,
			DraftRevision: draft.Revision, CheckGeneration: cas.CheckGeneration,
			CandidateDigest: cas.CandidateDigest, PolicyDigest: cas.PolicyDigest,
			ConfigRevision: cas.ConfigRevision, ActivatedAt: activatedAt,
		}
		envelope.Drafts = append(envelope.Drafts[:index], envelope.Drafts[index+1:]...)
		pendingIndex = onboardingPendingIndex(envelope.PendingActivations, cas.DraftID)
		if pendingIndex < 0 {
			return errOnboardingUnavailable
		}
		envelope.PendingActivations = append(envelope.PendingActivations[:pendingIndex], envelope.PendingActivations[pendingIndex+1:]...)
		envelope.Activations = append(envelope.Activations, record)
		return s.writeLocked(envelope)
	})
	if err != nil {
		return onboardingActivationRecord{}, s.publicError(err)
	}
	return record, nil
}

func (s *onboardingFileDraftStore) RecordFirstRead(ctx context.Context, receipt onboardingFirstReadReceipt) (bool, error) {
	if err := s.validateCall(ctx); err != nil || !validOnboardingFirstReadReceipt(receipt, s.currentTime()) {
		return false, errOnboardingInvalid
	}
	added := false
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		for _, prior := range envelope.FirstReads {
			if sameOnboardingFirstRead(prior, receipt) {
				return nil
			}
		}
		if len(envelope.FirstReads) >= onboardingStoreMaxFirstReads {
			return errOnboardingCapacity
		}
		envelope.FirstReads = append(envelope.FirstReads, receipt)
		if err := s.writeLocked(envelope); err != nil {
			return err
		}
		added = true
		return nil
	})
	if err != nil {
		return false, s.publicError(err)
	}
	return added, nil
}

func (s *onboardingFileDraftStore) ListFirstReads(ctx context.Context) ([]onboardingFirstReadReceipt, error) {
	if err := s.validateCall(ctx); err != nil {
		return nil, err
	}
	var receipts []onboardingFirstReadReceipt
	err := withOnboardingStoreLock(s.path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		envelope, err := s.readLocked()
		if err != nil {
			return err
		}
		receipts = append([]onboardingFirstReadReceipt(nil), envelope.FirstReads...)
		sort.Slice(receipts, func(i, j int) bool {
			if receipts[i].ObservedAt.Equal(receipts[j].ObservedAt) {
				if receipts[i].Service == receipts[j].Service {
					return receipts[i].Tool < receipts[j].Tool
				}
				return receipts[i].Service < receipts[j].Service
			}
			return receipts[i].ObservedAt.Before(receipts[j].ObservedAt)
		})
		return nil
	})
	if err != nil {
		return nil, s.publicError(err)
	}
	return receipts, nil
}

func (s *onboardingFileDraftStore) validateCall(ctx context.Context) error {
	if s == nil || s.path == "" || s.now == nil || ctx == nil {
		return errOnboardingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (s *onboardingFileDraftStore) readLocked() (onboardingStoreEnvelope, error) {
	data, err := readOnboardingStoreFile(s.path, onboardingStoreMaxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return emptyOnboardingStoreEnvelope(), nil
	}
	if err != nil || len(data) == 0 || rejectDuplicateSetupDraftJSONKeys(data) != nil {
		return onboardingStoreEnvelope{}, errOnboardingUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope onboardingStoreEnvelope
	if decoder.Decode(&envelope) != nil {
		return onboardingStoreEnvelope{}, errOnboardingUnavailable
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF || !validOnboardingStoreEnvelope(envelope, s.currentTime()) {
		return onboardingStoreEnvelope{}, errOnboardingUnavailable
	}
	canonical, err := json.Marshal(envelope)
	if err != nil || !bytes.Equal(canonical, data) {
		return onboardingStoreEnvelope{}, errOnboardingUnavailable
	}
	return envelope, nil
}

func (s *onboardingFileDraftStore) writeLocked(envelope onboardingStoreEnvelope) error {
	if !validOnboardingStoreEnvelope(envelope, s.currentTime()) {
		return errOnboardingCapacity
	}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > onboardingStoreMaxFileBytes {
		return errOnboardingCapacity
	}
	writeFileAtomically := s.writeFileAtomically
	if writeFileAtomically == nil {
		writeFileAtomically = writeOnboardingStoreFileAtomically
	}
	if err := writeFileAtomically(s.path, data); err != nil {
		return errOnboardingUnavailable
	}
	return nil
}

func (s *onboardingFileDraftStore) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *onboardingFileDraftStore) publicError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errOnboardingInvalid), errors.Is(err, errOnboardingDraftNotFound),
		errors.Is(err, errOnboardingDraftStale), errors.Is(err, errOnboardingCheckRequired),
		errors.Is(err, errOnboardingReviewChanged), errors.Is(err, errOnboardingActivationPending),
		errors.Is(err, errOnboardingActivationRecoveryRequired),
		errors.Is(err, errOnboardingCapacity):
		return err
	default:
		return errOnboardingUnavailable
	}
}

func applyOnboardingRecoveryState(drafts []onboardingDraft, pending []onboardingPendingActivation) {
	for i := range drafts {
		drafts[i].ActivationRecoveryRequired = onboardingPendingRecoveryRequired(pending, drafts[i].ID)
	}
}

func onboardingPendingRecoveryRequired(pending []onboardingPendingActivation, draftID string) bool {
	index := onboardingPendingIndex(pending, draftID)
	return index >= 0 && pending[index].RecoveryRequired
}

func emptyOnboardingStoreEnvelope() onboardingStoreEnvelope {
	return onboardingStoreEnvelope{
		Version: onboardingStoreVersion, Drafts: []onboardingDraft{},
		PendingActivations: []onboardingPendingActivation{},
		Activations:        []onboardingActivationRecord{}, FirstReads: []onboardingFirstReadReceipt{},
	}
}

func validOnboardingStoreEnvelope(envelope onboardingStoreEnvelope, now time.Time) bool {
	if envelope.Version != onboardingStoreVersion || envelope.Drafts == nil || envelope.PendingActivations == nil ||
		envelope.Activations == nil || envelope.FirstReads == nil || len(envelope.Drafts) > onboardingStoreMaxDrafts ||
		len(envelope.PendingActivations) > onboardingStoreMaxDrafts || len(envelope.Activations) > onboardingStoreMaxActivations ||
		len(envelope.FirstReads) > onboardingStoreMaxFirstReads || !validOnboardingTime(now) {
		return false
	}
	drafts := make(map[string]onboardingDraft, len(envelope.Drafts))
	for _, draft := range envelope.Drafts {
		if !validOnboardingDraft(draft, now) {
			return false
		}
		if _, duplicate := drafts[draft.ID]; duplicate {
			return false
		}
		drafts[draft.ID] = draft
	}
	activationByDraft := make(map[string]onboardingActivationRecord, len(envelope.Activations))
	targetIDs := make(map[string]struct{}, len(envelope.Activations)+len(envelope.PendingActivations))
	for _, record := range envelope.Activations {
		if !validOnboardingActivationRecord(record, now) {
			return false
		}
		if _, duplicate := activationByDraft[record.DraftID]; duplicate {
			return false
		}
		if _, duplicate := targetIDs[record.TargetID]; duplicate {
			return false
		}
		activationByDraft[record.DraftID] = record
		targetIDs[record.TargetID] = struct{}{}
	}
	pendingDrafts := make(map[string]struct{}, len(envelope.PendingActivations))
	for _, pending := range envelope.PendingActivations {
		draft, exists := drafts[pending.DraftID]
		if !exists || !validOnboardingPendingActivation(pending, draft) {
			return false
		}
		if _, duplicate := pendingDrafts[pending.DraftID]; duplicate {
			return false
		}
		if _, duplicate := activationByDraft[pending.DraftID]; duplicate {
			return false
		}
		if _, duplicate := targetIDs[pending.TargetID]; duplicate {
			return false
		}
		pendingDrafts[pending.DraftID] = struct{}{}
		targetIDs[pending.TargetID] = struct{}{}
	}
	firstReadKeys := make(map[string]struct{}, len(envelope.FirstReads))
	for _, receipt := range envelope.FirstReads {
		if !validOnboardingFirstReadReceipt(receipt, now) {
			return false
		}
		key := onboardingFirstReadKey(receipt)
		if _, duplicate := firstReadKeys[key]; duplicate {
			return false
		}
		firstReadKeys[key] = struct{}{}
	}
	return true
}

func validOnboardingDraft(draft onboardingDraft, now time.Time) bool {
	if !validOnboardingDraftID(draft.ID) || draft.Revision == 0 || !validOnboardingService(draft.Service) ||
		!validOnboardingDraftFields(draft.Service, draft.Fields) || !validOnboardingTime(draft.CreatedAt) ||
		!validOnboardingTime(draft.UpdatedAt) || draft.CreatedAt.After(draft.UpdatedAt) || draft.UpdatedAt.After(now) {
		return false
	}
	if draft.Check != nil && !validOnboardingCheckReceiptForDraft(*draft.Check, draft, now) {
		return false
	}
	return true
}

func validOnboardingDraftFields(service onboardingServiceKind, fields onboardingDraftFields) bool {
	if !validOnboardingService(service) || !validOnboardingText(fields.Name, 80, true) {
		return false
	}
	switch service {
	case onboardingServiceGitHub:
		if fields.BaseURL != "" || fields.Username != "" || fields.JobPath != "" || fields.Environment != "" {
			return false
		}
		if fields.Origin != "" {
			origin, err := validateOrigin(fields.Origin)
			if err != nil || origin != fields.Origin || unsafeOnboardingSecrets(fields.Origin) {
				return false
			}
		}
		return fields.Repository == "" || (validRepository(fields.Repository) && len(fields.Repository) <= 256 && !unsafeOnboardingText(fields.Repository))
	case onboardingServiceJenkins:
		if fields.Origin != "" || fields.Repository != "" {
			return false
		}
		if fields.BaseURL != "" {
			baseURL, err := validateJenkinsBaseURL(fields.BaseURL)
			if err != nil || baseURL != fields.BaseURL || unsafeOnboardingSecrets(fields.BaseURL) {
				return false
			}
		}
		return validOnboardingText(fields.Username, 128, true) && (fields.Username == "" || validJenkinsUsername(fields.Username)) &&
			(fields.JobPath == "" || validJenkinsJobPath(fields.JobPath)) && validJenkinsEnvironmentLabel(fields.Environment) &&
			len(fields.Environment) <= 128 && !unsafeOnboardingText(fields.JobPath) && !unsafeOnboardingText(fields.Environment)
	case onboardingServiceDashboard:
		if fields.Origin != "" || fields.Repository != "" || fields.Username != "" || fields.JobPath != "" || fields.Environment != "" {
			return false
		}
		if fields.BaseURL == "" {
			return true
		}
		baseURL, err := validateJenkinsBaseURL(fields.BaseURL)
		return err == nil && baseURL == fields.BaseURL && !unsafeOnboardingSecrets(fields.BaseURL)
	default:
		return false
	}
}

func validOnboardingText(value string, maxBytes int, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value || unsafeOnboardingText(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func unsafeOnboardingText(value string) bool {
	return unsafeOnboardingSecrets(value) || strings.Contains(value, "://") || strings.ContainsAny(value, "?#")
}

func unsafeOnboardingSecrets(value string) bool {
	lower := strings.ToLower(value)
	return githubTokenPattern.MatchString(value) || credentialHeader.MatchString(value) || credentialPattern.MatchString(value) ||
		privateKeyPEM.MatchString(value) || secretRefPattern.MatchString(value) || namedSecretIDPattern.MatchString(value) ||
		strings.Contains(lower, "cred:") || strings.Contains(lower, "secret:")
}

func validOnboardingCheckReceiptShape(receipt onboardingCheckReceipt) bool {
	if !validOnboardingDraftID(receipt.DraftID) || receipt.DraftRevision == 0 || receipt.CheckGeneration == 0 ||
		!validOnboardingSHA256Digest(receipt.CandidateDigest) || !validOnboardingPolicyDigest(receipt.PolicyDigest) ||
		!validOnboardingConfigRevision(receipt.ConfigRevision) || !validOnboardingTime(receipt.CheckedAt) {
		return false
	}
	switch receipt.Outcome {
	case onboardingCheckSucceeded:
		return receipt.FailureKind == ""
	case onboardingCheckFailed:
		return validOnboardingFailureKind(receipt.FailureKind)
	default:
		return false
	}
}

func validOnboardingFailureKind(kind connectionFailureKind) bool {
	switch kind {
	case connectionFailureAddress, connectionFailureTimeout, connectionFailureAuthentication,
		connectionFailureAccess, connectionFailureEndpoint, connectionFailureRateLimit, connectionFailureOther:
		return true
	default:
		return false
	}
}

func validOnboardingCheckReceiptForDraft(receipt onboardingCheckReceipt, draft onboardingDraft, now time.Time) bool {
	if !validOnboardingCheckReceiptShape(receipt) || receipt.DraftID != draft.ID || receipt.DraftRevision != draft.Revision ||
		receipt.CheckGeneration != draft.CheckGeneration ||
		receipt.CheckedAt.Before(draft.CreatedAt) || receipt.CheckedAt.After(now) {
		return false
	}
	digest, err := onboardingCandidateDigest(draft.Service, draft.Fields)
	return err == nil && digest == receipt.CandidateDigest
}

func validOnboardingActivationCAS(cas onboardingActivationCAS) bool {
	return validOnboardingDraftID(cas.DraftID) && cas.ExpectedRevision > 0 && cas.CheckGeneration > 0 &&
		validOnboardingSHA256Digest(cas.CandidateDigest) && validOnboardingPolicyDigest(cas.PolicyDigest) &&
		validOnboardingConfigRevision(cas.ConfigRevision)
}

func onboardingActivationMatchesCheck(receipt onboardingCheckReceipt, cas onboardingActivationCAS) bool {
	return receipt.DraftID == cas.DraftID && receipt.DraftRevision == cas.ExpectedRevision &&
		receipt.CheckGeneration == cas.CheckGeneration &&
		receipt.CandidateDigest == cas.CandidateDigest && receipt.PolicyDigest == cas.PolicyDigest &&
		receipt.ConfigRevision == cas.ConfigRevision
}

func onboardingActivationMatches(record onboardingActivationRecord, cas onboardingActivationCAS) bool {
	return record.DraftID == cas.DraftID && record.DraftRevision == cas.ExpectedRevision &&
		record.CheckGeneration == cas.CheckGeneration &&
		record.CandidateDigest == cas.CandidateDigest &&
		record.PolicyDigest == cas.PolicyDigest && record.ConfigRevision == cas.ConfigRevision
}

func validOnboardingActivationRecord(record onboardingActivationRecord, now time.Time) bool {
	return validOnboardingDraftID(record.DraftID) && record.DraftRevision > 0 && record.CheckGeneration > 0 &&
		validOnboardingTargetID(record.Service, record.TargetID) &&
		validOnboardingService(record.Service) && validOnboardingSHA256Digest(record.CandidateDigest) &&
		validOnboardingPolicyDigest(record.PolicyDigest) && validOnboardingConfigRevision(record.ConfigRevision) &&
		validOnboardingTime(record.ActivatedAt) && !record.ActivatedAt.After(now)
}

func validOnboardingPendingActivation(pending onboardingPendingActivation, draft onboardingDraft) bool {
	if pending.DraftID != draft.ID || pending.ExpectedRevision != draft.Revision ||
		!validOnboardingDraftID(pending.DraftID) || pending.ExpectedRevision == 0 ||
		pending.CheckGeneration == 0 || pending.CheckGeneration > draft.CheckGeneration ||
		!validOnboardingSHA256Digest(pending.CandidateDigest) || !validOnboardingPolicyDigest(pending.PolicyDigest) ||
		!validOnboardingConfigRevision(pending.ConfigRevision) || !validOnboardingTargetID(draft.Service, pending.TargetID) {
		return false
	}
	if !onboardingPendingCandidateMatches(pending, onboardingActivationCAS{
		DraftID: pending.DraftID, ExpectedRevision: pending.ExpectedRevision,
		CandidateDigest: pending.CandidateDigest, PolicyDigest: pending.PolicyDigest,
		ConfigRevision: pending.ConfigRevision,
	}) {
		return false
	}
	if draft.Check == nil {
		return true
	}
	return draft.Check.CheckGeneration == pending.CheckGeneration &&
		draft.Check.DraftID == pending.DraftID && draft.Check.DraftRevision == pending.ExpectedRevision &&
		draft.Check.CandidateDigest == pending.CandidateDigest && draft.Check.PolicyDigest == pending.PolicyDigest &&
		draft.Check.ConfigRevision == pending.ConfigRevision
}

func onboardingPendingCandidateMatches(pending onboardingPendingActivation, cas onboardingActivationCAS) bool {
	return pending.DraftID == cas.DraftID && pending.ExpectedRevision == cas.ExpectedRevision &&
		pending.CandidateDigest == cas.CandidateDigest && pending.PolicyDigest == cas.PolicyDigest
}

func validOnboardingFirstReadReceipt(receipt onboardingFirstReadReceipt, now time.Time) bool {
	if receipt.Source != onboardingFirstReadSource || !validFirstReadServiceTool(string(receipt.Service), receipt.Tool) ||
		!validFirstReadDefinitionDigest(receipt.DefinitionDigest) || !validOnboardingTime(receipt.ObservedAt) || receipt.ObservedAt.After(now) {
		return false
	}
	return true
}

func sameOnboardingFirstRead(a, b onboardingFirstReadReceipt) bool {
	return a.Service == b.Service && a.Tool == b.Tool && a.DefinitionDigest == b.DefinitionDigest
}

func onboardingFirstReadKey(receipt onboardingFirstReadReceipt) string {
	return string(receipt.Service) + "\x00" + receipt.Tool + "\x00" + receipt.DefinitionDigest
}

func validOnboardingDraftID(value string) bool {
	return onboardingDraftIDPattern.MatchString(value)
}

func validOnboardingTargetID(service onboardingServiceKind, id string) bool {
	switch service {
	case onboardingServiceGitHub:
		return githubIDPattern.MatchString(id)
	case onboardingServiceJenkins:
		return jenkinsIDPattern.MatchString(id)
	case onboardingServiceDashboard:
		return dashboardIDPattern.MatchString(id)
	default:
		return false
	}
}

func validOnboardingService(service onboardingServiceKind) bool {
	switch service {
	case onboardingServiceGitHub, onboardingServiceJenkins, onboardingServiceDashboard:
		return true
	default:
		return false
	}
}

func validOnboardingSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func validOnboardingConfigRevision(value string) bool {
	const prefix = "config:sha256:"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func validOnboardingPolicyDigest(value string) bool {
	const prefix = "policy:sha256:"
	if len(value) != len(prefix)+64 || !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == 32 && strings.ToLower(value) == value
}

func validOnboardingTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 2000 && value.Year() <= 9999 && value.Location() == time.UTC
}

func newOnboardingDraftID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "onboarding:" + hex.EncodeToString(random[:]), nil
}

func newOnboardingTargetID(service onboardingServiceKind) (string, error) {
	prefix := ""
	switch service {
	case onboardingServiceGitHub:
		prefix = "github:"
	case onboardingServiceJenkins:
		prefix = "jenkins:"
	case onboardingServiceDashboard:
		prefix = "dashboard:"
	default:
		return "", errOnboardingInvalid
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func onboardingDraftIndex(drafts []onboardingDraft, id string) int {
	for index := range drafts {
		if drafts[index].ID == id {
			return index
		}
	}
	return -1
}

func onboardingPendingIndex(pending []onboardingPendingActivation, id string) int {
	for index := range pending {
		if pending[index].DraftID == id {
			return index
		}
	}
	return -1
}

func onboardingActivationIndex(records []onboardingActivationRecord, id string) int {
	for index := range records {
		if records[index].DraftID == id {
			return index
		}
	}
	return -1
}

func cloneOnboardingDraft(draft onboardingDraft) onboardingDraft {
	if draft.Check != nil {
		check := *draft.Check
		draft.Check = &check
	}
	return draft
}

func cloneOnboardingDrafts(drafts []onboardingDraft) []onboardingDraft {
	result := make([]onboardingDraft, len(drafts))
	for index, draft := range drafts {
		result[index] = cloneOnboardingDraft(draft)
	}
	return result
}
