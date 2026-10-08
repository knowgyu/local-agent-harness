package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
)

const (
	namedSecretCleanupQueueFileName = "named-secret-cleanups.json"
	namedSecretCleanupQueueVersion  = 1
	namedSecretCleanupQueueMaxItems = 128
	namedSecretCleanupQueueMaxBytes = 32 << 10
)

var errNamedSecretCleanupQueue = errors.New("named secret cleanup status is unavailable")

type namedSecretCleanupIntent struct {
	ID        string `json:"named_secret_id"`
	OldRef    string `json:"old_ref"`
	NewRef    string `json:"staged_new_ref"`
	DeleteOld bool   `json:"delete_old"`
	Committed bool   `json:"committed"`
}

type namedSecretCleanupEnvelope struct {
	Version int                        `json:"version"`
	Entries []namedSecretCleanupIntent `json:"entries"`
}

type namedSecretCleanupQueue interface {
	Load() ([]namedSecretCleanupIntent, error)
	Put(namedSecretCleanupIntent) error
	MarkCommitted(id, oldRef, newRef string) error
	Remove(id, oldRef string) error
}

type namedSecretCleanupFileQueue struct {
	path                string
	writeFileAtomically func(path string, data []byte) error
}

func newNamedSecretCleanupFileQueue() (namedSecretCleanupQueue, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil || cacheDir == "" || !filepath.IsAbs(cacheDir) {
		return nil, errNamedSecretCleanupQueue
	}
	return newNamedSecretCleanupFileQueueAt(filepath.Join(cacheDir, setupDraftQueueDirectoryName, namedSecretCleanupQueueFileName))
}

func newNamedSecretCleanupFileQueueAt(path string) (*namedSecretCleanupFileQueue, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Base(path) != namedSecretCleanupQueueFileName {
		return nil, errNamedSecretCleanupQueue
	}
	cleanPath := filepath.Clean(path)
	if cleanPath == string(filepath.Separator) || filepath.Dir(cleanPath) == cleanPath {
		return nil, errNamedSecretCleanupQueue
	}
	return &namedSecretCleanupFileQueue{path: cleanPath}, nil
}

func (s *namedSecretCleanupFileQueue) Load() ([]namedSecretCleanupIntent, error) {
	if s == nil || s.path == "" {
		return nil, errNamedSecretCleanupQueue
	}
	var result []namedSecretCleanupIntent
	if err := withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked()
		if err != nil {
			return err
		}
		result = append([]namedSecretCleanupIntent(nil), entries...)
		return nil
	}); err != nil {
		return nil, errNamedSecretCleanupQueue
	}
	return result, nil
}

func (s *namedSecretCleanupFileQueue) Put(intent namedSecretCleanupIntent) error {
	if s == nil || s.path == "" || !validNamedSecretCleanupIntent(intent) || intent.Committed {
		return errNamedSecretCleanupQueue
	}
	if err := withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.ID == intent.ID {
				if entry.OldRef == intent.OldRef && entry.NewRef == intent.NewRef && entry.DeleteOld == intent.DeleteOld {
					return nil
				}
				return errNamedSecretCleanupQueue
			}
			if namedSecretCleanupRefsOverlap(entry, intent) {
				return errNamedSecretCleanupQueue
			}
		}
		if len(entries) >= namedSecretCleanupQueueMaxItems {
			return errNamedSecretCleanupQueue
		}
		entries = append(entries, intent)
		return s.writeLocked(entries)
	}); err != nil {
		return errNamedSecretCleanupQueue
	}
	return nil
}

func (s *namedSecretCleanupFileQueue) MarkCommitted(id, oldRef, newRef string) error {
	if s == nil || s.path == "" || !namedSecretIDPattern.MatchString(id) ||
		!secretRefPattern.MatchString(oldRef) || !secretRefPattern.MatchString(newRef) || oldRef == newRef {
		return errNamedSecretCleanupQueue
	}
	if err := withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked()
		if err != nil {
			return err
		}
		for index := range entries {
			entry := &entries[index]
			if entry.ID == id && entry.OldRef == oldRef && entry.NewRef == newRef {
				if entry.Committed {
					return nil
				}
				entry.Committed = true
				return s.writeLocked(entries)
			}
		}
		return errNamedSecretCleanupQueue
	}); err != nil {
		return errNamedSecretCleanupQueue
	}
	return nil
}

func (s *namedSecretCleanupFileQueue) Remove(id, oldRef string) error {
	if s == nil || s.path == "" || !namedSecretIDPattern.MatchString(id) || !secretRefPattern.MatchString(oldRef) {
		return errNamedSecretCleanupQueue
	}
	if err := withSetupDraftQueueLock(s.path, func() error {
		entries, err := s.readLocked()
		if err != nil {
			return err
		}
		kept := make([]namedSecretCleanupIntent, 0, len(entries))
		removed := false
		for _, entry := range entries {
			if entry.ID == id && entry.OldRef == oldRef {
				removed = true
				continue
			}
			kept = append(kept, entry)
		}
		if !removed {
			return nil
		}
		return s.writeLocked(kept)
	}); err != nil {
		return errNamedSecretCleanupQueue
	}
	return nil
}

func (s *namedSecretCleanupFileQueue) readLocked() ([]namedSecretCleanupIntent, error) {
	data, err := readSetupDraftQueueFile(s.path, namedSecretCleanupQueueMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		return []namedSecretCleanupIntent{}, nil
	}
	if err != nil || len(data) == 0 || rejectDuplicateSetupDraftJSONKeys(data) != nil {
		return nil, errNamedSecretCleanupQueue
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var envelope namedSecretCleanupEnvelope
	if decoder.Decode(&envelope) != nil {
		return nil, errNamedSecretCleanupQueue
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF || envelope.Version != namedSecretCleanupQueueVersion || envelope.Entries == nil ||
		len(envelope.Entries) > namedSecretCleanupQueueMaxItems {
		return nil, errNamedSecretCleanupQueue
	}
	seenPairs := make(map[namedSecretCleanupIntent]struct{}, len(envelope.Entries))
	ids := make(map[string]struct{}, len(envelope.Entries))
	for _, entry := range envelope.Entries {
		if !validNamedSecretCleanupIntent(entry) {
			return nil, errNamedSecretCleanupQueue
		}
		if _, exists := seenPairs[entry]; exists {
			return nil, errNamedSecretCleanupQueue
		}
		if _, exists := ids[entry.ID]; exists {
			return nil, errNamedSecretCleanupQueue
		}
		for _, prior := range envelope.Entries {
			if prior != entry && namedSecretCleanupRefsOverlap(prior, entry) {
				return nil, errNamedSecretCleanupQueue
			}
		}
		seenPairs[entry] = struct{}{}
		ids[entry.ID] = struct{}{}
	}
	return envelope.Entries, nil
}

func (s *namedSecretCleanupFileQueue) writeLocked(entries []namedSecretCleanupIntent) error {
	if len(entries) > namedSecretCleanupQueueMaxItems {
		return errNamedSecretCleanupQueue
	}
	canonical := make([]namedSecretCleanupIntent, len(entries))
	copy(canonical, entries)
	sort.Slice(canonical, func(i, j int) bool {
		if canonical[i].ID == canonical[j].ID {
			return canonical[i].OldRef < canonical[j].OldRef
		}
		return canonical[i].ID < canonical[j].ID
	})
	envelope := namedSecretCleanupEnvelope{Version: namedSecretCleanupQueueVersion, Entries: canonical}
	data, err := json.Marshal(envelope)
	if err != nil || len(data) > namedSecretCleanupQueueMaxBytes {
		return errNamedSecretCleanupQueue
	}
	writeFileAtomically := s.writeFileAtomically
	if writeFileAtomically == nil {
		writeFileAtomically = writeSetupDraftQueueFileAtomically
	}
	if writeFileAtomically(s.path, data) != nil {
		return errNamedSecretCleanupQueue
	}
	return nil
}

func validNamedSecretCleanupIntent(intent namedSecretCleanupIntent) bool {
	return namedSecretIDPattern.MatchString(intent.ID) && secretRefPattern.MatchString(intent.OldRef) &&
		secretRefPattern.MatchString(intent.NewRef) && intent.OldRef != intent.NewRef
}

func namedSecretCleanupRefsOverlap(left, right namedSecretCleanupIntent) bool {
	return left.OldRef == right.OldRef || left.OldRef == right.NewRef ||
		left.NewRef == right.OldRef || left.NewRef == right.NewRef
}

func validateNamedSecretCleanupOwners(cfg config, intents []namedSecretCleanupIntent) error {
	for _, intent := range intents {
		if !validNamedSecretCleanupIntent(intent) || countNamedSecretID(cfg.NamedSecrets, intent.ID) != 1 {
			return errNamedSecretCleanupQueue
		}
	}
	return nil
}

func countNamedSecretID(secrets []namedSecretMetadata, id string) int {
	count := 0
	for _, secret := range secrets {
		if secret.ID == id {
			count++
		}
	}
	return count
}
