package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	setupDraftQueueFileName      = "setup-drafts.json"
	setupDraftQueueDirectoryName = "LocalAgentHarness"
	setupDraftQueueVersion       = 1
	setupDraftQueueMaxFileBytes  = 256 << 10
	setupDraftQueueMaxReview     = setupDraftQueueMaxFileBytes
	setupDraftQueueMaxChanges    = maxSetupDraftConnections + maxSetupDraftBundles + maxSetupDraftSSHTargets
)

var (
	errSetupDraftQueueUnavailable    = errors.New("setup draft queue unavailable")
	errSetupDraftQueueInvalid        = errors.New("setup draft queue data is invalid")
	errSetupDraftQueueCapacity       = errors.New("setup draft queue is full")
	errSetupDraftQueueReviewMissing  = errors.New("setup draft queue review is unavailable")
	errSetupDraftQueueReviewExpired  = errors.New("setup draft queue review has expired")
	errSetupDraftQueueConsumeFailed  = errors.New("setup draft queue review could not be consumed after the action committed")
	errSetupDraftQueueClaimFailed    = errors.New("setup draft queue review could not be claimed")
	errSetupDraftQueueReviewMismatch = errors.New("setup draft queue review does not match the submitted reference")
)

type setupDraftQueueStore interface {
	Load(now time.Time) ([]SetupDraftReview, error)
	Put(SetupDraftReview) error
	Delete(id string) error
	WithReview(id string, now time.Time, fn func(SetupDraftReview) (consume bool, err error)) error
	ClaimReview(id, baseRevision, digest string, now time.Time, validate func(SetupDraftReview) error) (SetupDraftReview, error)
}

type setupDraftFileQueueStore struct {
	path                string
	now                 func() time.Time
	writeFileAtomically func(path string, data []byte) error
}

type setupDraftQueueEnvelope struct {
	Version int                    `json:"version"`
	Entries []setupDraftQueueEntry `json:"entries"`
}

type setupDraftQueueEntry struct {
	CreatedAt time.Time        `json:"created_at"`
	Review    SetupDraftReview `json:"review"`
}

func newSetupDraftQueueStore() (setupDraftQueueStore, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" || !filepath.IsAbs(cacheDir) {
		return nil, errSetupDraftQueueUnavailable
	}
	path := filepath.Join(cacheDir, setupDraftQueueDirectoryName, setupDraftQueueFileName)
	return newSetupDraftFileQueueStore(path, time.Now)
}

// newSetupDraftFileQueueStoreForTest is the only path-injection seam. Production
// callers use newSetupDraftQueueStore, which fixes the location under UserCacheDir.
func newSetupDraftFileQueueStoreForTest(path string, now func() time.Time) (*setupDraftFileQueueStore, error) {
	if now == nil {
		return nil, errSetupDraftQueueUnavailable
	}
	return newSetupDraftFileQueueStore(path, now)
}

func newSetupDraftFileQueueStore(path string, now func() time.Time) (*setupDraftFileQueueStore, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Base(path) != setupDraftQueueFileName || now == nil {
		return nil, errSetupDraftQueueUnavailable
	}
	cleanPath := filepath.Clean(path)
	if cleanPath == string(filepath.Separator) || filepath.Dir(cleanPath) == cleanPath {
		return nil, errSetupDraftQueueUnavailable
	}
	return &setupDraftFileQueueStore{path: cleanPath, now: now}, nil
}

func (s *setupDraftFileQueueStore) Load(now time.Time) ([]SetupDraftReview, error) {
	if s == nil || !validQueueTime(now) {
		return nil, errSetupDraftQueueUnavailable
	}
	var reviews []SetupDraftReview
	err := withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked(now)
		if err != nil {
			return err
		}
		kept := make([]setupDraftQueueEntry, 0, len(entries))
		reviews = make([]SetupDraftReview, 0, len(entries))
		for _, entry := range entries {
			if !now.Before(entry.Review.ExpiresAt) {
				continue
			}
			kept = append(kept, entry)
			reviews = append(reviews, cloneSetupDraftReview(entry.Review))
		}
		if len(kept) != len(entries) {
			return s.writeLocked(kept)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return reviews, nil
}

func (s *setupDraftFileQueueStore) Put(review SetupDraftReview) error {
	if s == nil {
		return errSetupDraftQueueUnavailable
	}
	now := s.currentTime()
	if !validSetupDraftQueueReview(review, now, now, true) {
		return errSetupDraftQueueInvalid
	}
	return withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked(now)
		if err != nil {
			return err
		}
		kept := make([]setupDraftQueueEntry, 0, len(entries)+1)
		for _, entry := range entries {
			if !now.Before(entry.Review.ExpiresAt) {
				continue
			}
			if entry.Review.ID == review.ID {
				return errSetupDraftQueueInvalid
			}
			kept = append(kept, entry)
		}
		if len(kept) >= setupDraftMaximumPending {
			return errSetupDraftQueueCapacity
		}
		kept = append(kept, setupDraftQueueEntry{CreatedAt: now.UTC(), Review: cloneSetupDraftReview(review)})
		return s.writeLocked(kept)
	})
}

func (s *setupDraftFileQueueStore) Delete(id string) error {
	if s == nil || !validSetupDraftID(id) {
		return errSetupDraftQueueInvalid
	}
	now := s.currentTime()
	if !validQueueTime(now) {
		return errSetupDraftQueueUnavailable
	}
	return withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked(now)
		if err != nil {
			return err
		}
		kept := make([]setupDraftQueueEntry, 0, len(entries))
		for _, entry := range entries {
			if entry.Review.ID != id && now.Before(entry.Review.ExpiresAt) {
				kept = append(kept, entry)
			}
		}
		return s.writeLocked(kept)
	})
}

// WithReview serializes a cross-process decision about one pending review. The
// callback runs while the queue lock is held and must not call queue methods.
// A callback error preserves the record; a successful consuming callback
// removes it with an atomic queue rewrite before releasing the lock.
func (s *setupDraftFileQueueStore) WithReview(
	id string,
	now time.Time,
	fn func(SetupDraftReview) (consume bool, err error),
) error {
	if s == nil || !validSetupDraftID(id) || !validQueueTime(now) || fn == nil {
		return errSetupDraftQueueInvalid
	}
	return withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked(now)
		if err != nil {
			return err
		}
		index := -1
		for entryIndex, entry := range entries {
			if entry.Review.ID == id {
				index = entryIndex
				break
			}
		}
		if index < 0 {
			return errSetupDraftQueueReviewMissing
		}
		entry := entries[index]
		if !now.Before(entry.Review.ExpiresAt) {
			return errSetupDraftQueueReviewExpired
		}
		consume, err := fn(cloneSetupDraftReview(entry.Review))
		if err != nil {
			return err
		}
		if !consume {
			return nil
		}
		kept := make([]setupDraftQueueEntry, 0, len(entries)-1)
		kept = append(kept, entries[:index]...)
		kept = append(kept, entries[index+1:]...)
		if err := s.writeLocked(kept); err != nil {
			return errSetupDraftQueueConsumeFailed
		}
		return nil
	})
}

// ClaimReview durably consumes a pending review before the caller can apply it.
// The queue lock covers lookup, immutable-reference checks, read-only
// preflight validation, and the atomic removal rewrite. The validator may take
// the config lock (queue -> config), but must not apply changes, perform other
// side effects, or call back into the queue. The lock is released before this
// method returns. A successful return is a one-shot claim: later caller errors
// or process termination never restore the review.
func (s *setupDraftFileQueueStore) ClaimReview(
	id, baseRevision, digest string,
	now time.Time,
	validate func(SetupDraftReview) error,
) (SetupDraftReview, error) {
	if s == nil || !validSetupDraftID(id) || !validSetupDraftRevision(baseRevision) ||
		!validSetupDraftDigest(digest) || !validQueueTime(now) || validate == nil {
		return SetupDraftReview{}, errSetupDraftQueueInvalid
	}

	var claimed SetupDraftReview
	err := withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked(now)
		if err != nil {
			return err
		}
		index := -1
		for entryIndex, entry := range entries {
			if entry.Review.ID == id {
				index = entryIndex
				break
			}
		}
		if index < 0 {
			return errSetupDraftQueueReviewMissing
		}
		entry := entries[index]
		if !now.Before(entry.Review.ExpiresAt) {
			return errSetupDraftQueueReviewExpired
		}
		if entry.Review.BaseRevision != baseRevision || entry.Review.Digest != digest {
			return errSetupDraftQueueReviewMismatch
		}

		review := cloneSetupDraftReview(entry.Review)
		if err := validate(review); err != nil {
			return err
		}

		kept := make([]setupDraftQueueEntry, 0, len(entries)-1)
		kept = append(kept, entries[:index]...)
		kept = append(kept, entries[index+1:]...)
		if err := s.writeLocked(kept); err != nil {
			return errSetupDraftQueueClaimFailed
		}
		claimed = review
		return nil
	})
	if err != nil {
		return SetupDraftReview{}, err
	}
	return claimed, nil
}

func (s *setupDraftFileQueueStore) readLocked(now time.Time) ([]setupDraftQueueEntry, error) {
	data, err := readSetupDraftQueueFile(s.path, setupDraftQueueMaxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, errSetupDraftQueueInvalid) {
			return nil, errSetupDraftQueueInvalid
		}
		return nil, errSetupDraftQueueUnavailable
	}
	if len(data) == 0 || rejectDuplicateSetupDraftJSONKeys(data) != nil {
		return nil, errSetupDraftQueueInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope setupDraftQueueEnvelope
	if decoder.Decode(&envelope) != nil {
		return nil, errSetupDraftQueueInvalid
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF || envelope.Version != setupDraftQueueVersion || envelope.Entries == nil || len(envelope.Entries) > setupDraftMaximumPending {
		return nil, errSetupDraftQueueInvalid
	}
	seen := make(map[string]struct{}, len(envelope.Entries))
	for index := range envelope.Entries {
		entry := &envelope.Entries[index]
		if !validSetupDraftQueueReview(entry.Review, entry.CreatedAt, now, false) {
			return nil, errSetupDraftQueueInvalid
		}
		if _, exists := seen[entry.Review.ID]; exists {
			return nil, errSetupDraftQueueInvalid
		}
		seen[entry.Review.ID] = struct{}{}
	}
	return envelope.Entries, nil
}

func (s *setupDraftFileQueueStore) writeLocked(entries []setupDraftQueueEntry) error {
	if len(entries) > setupDraftMaximumPending {
		return errSetupDraftQueueCapacity
	}
	envelope := setupDraftQueueEnvelope{Version: setupDraftQueueVersion, Entries: entries}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > setupDraftQueueMaxFileBytes {
		return errSetupDraftQueueCapacity
	}
	writeFileAtomically := s.writeFileAtomically
	if writeFileAtomically == nil {
		writeFileAtomically = writeSetupDraftQueueFileAtomically
	}
	if err := writeFileAtomically(s.path, data); err != nil {
		return errSetupDraftQueueUnavailable
	}
	return nil
}

func (s *setupDraftFileQueueStore) currentTime() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func validSetupDraftQueueReview(review SetupDraftReview, createdAt, now time.Time, requireLive bool) bool {
	if !validQueueTime(createdAt) || !validQueueTime(now) || !validSetupDraftID(review.ID) ||
		!validSetupDraftRevision(review.BaseRevision) || !validQueueTime(review.ExpiresAt) ||
		createdAt.After(now) || createdAt.Location() != time.UTC || review.ExpiresAt.Location() != time.UTC ||
		!createdAt.Before(review.ExpiresAt) || review.ExpiresAt.After(createdAt.Add(setupDraftMaximumLifetime)) ||
		requireLive && !now.Before(review.ExpiresAt) || review.Digest != setupDraftReviewDigest(review) ||
		!validSetupDraftDigest(review.Digest) || len(review.Changes) == 0 || len(review.Changes) > setupDraftQueueMaxChanges ||
		len(review.Sources) == 0 || len(review.Sources) > maxSetupDraftSources || len(review.Missing) > maxSetupDraftMissing {
		return false
	}
	_, err := normalizeStoredSetupDraftProposal(review.Proposal)
	if err != nil || !equalSetupDraftJSON(review.Sources, review.Proposal.Sources) {
		return false
	}
	missing, err := normalizeStoredSetupDraftMissing(review.Missing)
	if err != nil || !equalSetupDraftJSON(missing, review.Missing) {
		return false
	}
	// Review.Missing also records prerequisites derived from the current
	// settings snapshot (for example, an unavailable named credential). The
	// queue cannot recompute those without loading config; the controller
	// re-derives and compares the plan under the config lock before approval
	// can apply settings.
	encoded, err := json.Marshal(review)
	if err != nil || len(encoded) > setupDraftQueueMaxReview {
		return false
	}
	for _, change := range review.Changes {
		if !validSetupDraftText(change.Kind, 64, false) || !validSetupDraftText(change.Name, 256, false) ||
			!validSetupDraftText(change.Summary, 2048, false) || len(change.Scope) > 256 || len(change.PermissionIncrease) > 256 {
			return false
		}
		for _, value := range append(append([]string(nil), change.Scope...), change.PermissionIncrease...) {
			if !validSetupDraftText(value, 2048, false) {
				return false
			}
		}
	}
	return true
}

// normalizeStoredSetupDraftProposal revalidates a persisted controller review
// while preserving the public submission boundary. The external normalizer
// rejects generated SSH revision/approval metadata; only this queue path may
// clear those fields on a deep clone, normalize the original proposal fields,
// then require exact equality with the stored canonical proposal.
func normalizeStoredSetupDraftProposal(proposal SetupDraftSubmission) (SetupDraftSubmission, error) {
	submission := cloneSetupDraftSubmission(proposal)
	for targetIndex := range submission.SSHTargets {
		for operationIndex := range submission.SSHTargets[targetIndex].Operations {
			operation := &submission.SSHTargets[targetIndex].Operations[operationIndex]
			operation.Revision = ""
			operation.Approval.Revision = ""
			operation.Approval.Scope = nil
		}
	}
	normalized, err := normalizeSetupDraftSubmission(submission)
	if err != nil || !equalSetupDraftJSON(normalized, proposal) {
		return SetupDraftSubmission{}, errSetupDraftQueueInvalid
	}
	return normalized, nil
}

func normalizeStoredSetupDraftMissing(values []string) ([]string, error) {
	result := append([]string(nil), values...)
	if err := normalizeSetupDraftMissing(result); err != nil {
		return nil, errSetupDraftQueueInvalid
	}
	return result, nil
}

func equalSetupDraftJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func validSetupDraftDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func validQueueTime(value time.Time) bool {
	return !value.IsZero() && value.Year() >= 2000 && value.Year() <= 9999 && value.Location() != nil
}
