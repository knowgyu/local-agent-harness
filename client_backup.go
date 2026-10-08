package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	clientRegistrationBackupRecordVersion = 1
	maxClientRegistrationBackupFileBytes  = 12 << 20
	maxClientRegistrationBackupStoreBytes = 256 << 20
	maxClientRegistrationBackupRecords    = 128
	maxClientRegistrationPendingAge       = 24 * time.Hour
)

type clientRegistrationBackupStoreOnDisk struct {
	path  string
	mu    sync.Mutex
	now   func() time.Time
	newID func() (string, error)
}

type clientRegistrationBackupEnvelope struct {
	Version        int       `json:"version"`
	ClientID       string    `json:"client_id"`
	ReceiptID      string    `json:"receipt_id"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	OriginalExists bool      `json:"original_exists"`
	OriginalBytes  []byte    `json:"original_bytes,omitempty"`
	OriginalDigest string    `json:"original_digest"`
	HasApplied     bool      `json:"has_applied"`
	AppliedExists  bool      `json:"applied_exists,omitempty"`
	AppliedBytes   []byte    `json:"applied_bytes,omitempty"`
	AppliedDigest  string    `json:"applied_digest,omitempty"`
	Restored       bool      `json:"restored"`
}

type clientRegistrationBackupFileInfo struct {
	name      string
	path      string
	size      int64
	createdAt time.Time
	restored  bool
}

func newCurrentUserClientRegistrationBackupStore() (clientRegistrationBackupStore, error) {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		return nil, errClientRegistrationBackupFailed
	}
	path := filepath.Join(base, "LocalAgentHarness", "client-registration-backups")
	return newClientRegistrationBackupStoreAt(path)
}

func newClientRegistrationBackupStoreAt(path string) (clientRegistrationBackupStore, error) {
	if path == "" {
		return nil, errClientRegistrationBackupFailed
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, errClientRegistrationBackupFailed
	}
	if err := os.MkdirAll(absolutePath, 0o700); err != nil {
		return nil, errClientRegistrationBackupFailed
	}
	if err := ensureClientRegistrationBackupDirectory(absolutePath); err != nil {
		return nil, errClientRegistrationBackupFailed
	}
	return &clientRegistrationBackupStoreOnDisk{
		path:  absolutePath,
		now:   time.Now,
		newID: newClientRegistrationOpaqueID,
	}, nil
}

func (s *clientRegistrationBackupStoreOnDisk) save(ctx context.Context, clientID string, original clientRegistrationConfig, digest clientRegistrationDigest) (MCPClientBackupReceipt, error) {
	if err := ctx.Err(); err != nil {
		return MCPClientBackupReceipt{}, err
	}
	if !isSupportedMCPClientID(clientID) || !validClientRegistrationConfig(original) || digestClientRegistrationConfig(original) != digest {
		return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureClientRegistrationBackupDirectory(s.path); err != nil {
		return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
	}
	for attempt := 0; attempt < 4; attempt++ {
		id, err := s.newID()
		if err != nil || !validClientRegistrationID(id) {
			return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
		}
		path := s.recordPath(clientID, id)
		if _, err := os.Lstat(path); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
		}
		now := s.now().UTC()
		record := clientRegistrationBackupRecord{
			clientID:       clientID,
			receipt:        MCPClientBackupReceipt{ID: id, Status: mcpClientBackupCreated, CreatedAt: now},
			original:       cloneClientRegistrationConfig(original),
			originalDigest: digest,
		}
		if err := validateClientRegistrationBackupRecord(record, clientID, id); err != nil {
			return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
		}
		if err := s.ensureQuotaFor(record); err != nil {
			return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
		}
		if err := s.writeRecord(record, true); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
		}
		return record.receipt, nil
	}
	return MCPClientBackupReceipt{}, errClientRegistrationBackupFailed
}

func (s *clientRegistrationBackupStoreOnDisk) load(ctx context.Context, clientID, receiptID string) (clientRegistrationBackupRecord, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationBackupRecord{}, err
	}
	if !isSupportedMCPClientID(clientID) || !validClientRegistrationID(receiptID) {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureClientRegistrationBackupDirectory(s.path); err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	record, err := s.readRecord(s.recordPath(clientID, receiptID))
	if err != nil || validateClientRegistrationBackupRecord(record, clientID, receiptID) != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	return record, nil
}

func (s *clientRegistrationBackupStoreOnDisk) findLatest(ctx context.Context, clientID string, digest clientRegistrationDigest) (clientRegistrationBackupRecord, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationBackupRecord{}, err
	}
	if !isSupportedMCPClientID(clientID) {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ensureClientRegistrationBackupDirectory(s.path); err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	var best clientRegistrationBackupRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := ctx.Err(); err != nil {
			return clientRegistrationBackupRecord{}, err
		}
		path := filepath.Join(s.path, entry.Name())
		record, readErr := s.readRecord(path)
		if readErr != nil || validateClientRegistrationBackupRecord(record, record.clientID, record.receipt.ID) != nil {
			return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
		}
		if record.clientID != clientID || record.originalDigest != digest {
			continue
		}
		if best.receipt.ID == "" || record.receipt.CreatedAt.After(best.receipt.CreatedAt) {
			best = record
		}
	}
	if best.receipt.ID == "" {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	return best, nil
}

func (s *clientRegistrationBackupStoreOnDisk) prepareApply(ctx context.Context, clientID, receiptID string, applied clientRegistrationConfig, digest clientRegistrationDigest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validClientRegistrationConfig(applied) || digestClientRegistrationConfig(applied) != digest {
		return errClientRegistrationBackupFailed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.readRecord(s.recordPath(clientID, receiptID))
	if err != nil || validateClientRegistrationBackupRecord(record, clientID, receiptID) != nil {
		return errClientRegistrationBackupNotFound
	}
	if record.receipt.Status == mcpClientBackupCreated {
		record.applied = cloneClientRegistrationConfig(applied)
		record.appliedDigest = digest
		record.hasApplied = true
		record.receipt.Status = mcpClientBackupApplying
	} else if record.receipt.Status != mcpClientBackupApplying && record.receipt.Status != mcpClientBackupApplied {
		return errClientRegistrationBackupRequired
	} else if !record.hasApplied || record.appliedDigest != digest || !sameClientRegistrationConfig(record.applied, applied) {
		return errClientRegistrationBackupStale
	}
	return s.writeRecord(record, false)
}

func (s *clientRegistrationBackupStoreOnDisk) markApplied(ctx context.Context, clientID, receiptID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.readRecord(s.recordPath(clientID, receiptID))
	if err != nil || validateClientRegistrationBackupRecord(record, clientID, receiptID) != nil {
		return errClientRegistrationBackupNotFound
	}
	if !record.hasApplied || (record.receipt.Status != mcpClientBackupApplying && record.receipt.Status != mcpClientBackupApplied) {
		return errClientRegistrationBackupRequired
	}
	record.receipt.Status = mcpClientBackupApplied
	return s.writeRecord(record, false)
}

func (s *clientRegistrationBackupStoreOnDisk) markRestored(ctx context.Context, clientID, receiptID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := s.readRecord(s.recordPath(clientID, receiptID))
	if err != nil || validateClientRegistrationBackupRecord(record, clientID, receiptID) != nil {
		return errClientRegistrationBackupNotFound
	}
	if !record.hasApplied || (record.receipt.Status != mcpClientBackupApplying && record.receipt.Status != mcpClientBackupApplied && record.receipt.Status != mcpClientBackupRestored) {
		return errClientRegistrationBackupRequired
	}
	record.receipt.Status = mcpClientBackupRestored
	record.restored = true
	return s.writeRecord(record, false)
}

func (s *clientRegistrationBackupStoreOnDisk) recordPath(clientID, receiptID string) string {
	return filepath.Join(s.path, clientID+"-"+receiptID+".json")
}

func (s *clientRegistrationBackupStoreOnDisk) readRecord(path string) (clientRegistrationBackupRecord, error) {
	if err := validateClientRegistrationBackupStoragePath(s.path, true); err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	file, err := openClientRegistrationBackupFile(path)
	if err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxClientRegistrationBackupFileBytes {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	data, err := io.ReadAll(io.LimitReader(file, maxClientRegistrationBackupFileBytes+1))
	if err != nil || len(data) > maxClientRegistrationBackupFileBytes {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	var envelope clientRegistrationBackupEnvelope
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	return clientRegistrationBackupRecordFromEnvelope(envelope)
}

func (s *clientRegistrationBackupStoreOnDisk) writeRecord(record clientRegistrationBackupRecord, exclusive bool) error {
	if err := ensureClientRegistrationBackupDirectory(s.path); err != nil {
		return errClientRegistrationBackupFailed
	}
	if err := validateClientRegistrationBackupRecord(record, record.clientID, record.receipt.ID); err != nil {
		return errClientRegistrationBackupFailed
	}
	data, err := json.Marshal(clientRegistrationBackupEnvelopeFromRecord(record))
	if err != nil || len(data) > maxClientRegistrationBackupFileBytes {
		return errClientRegistrationBackupFailed
	}
	temp, err := os.CreateTemp(s.path, ".pending-*.tmp")
	if err != nil {
		return errClientRegistrationBackupFailed
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := secureClientRegistrationBackupFile(tempName); err != nil {
		_ = temp.Close()
		return errClientRegistrationBackupFailed
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return errClientRegistrationBackupFailed
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return errClientRegistrationBackupFailed
	}
	if err := temp.Close(); err != nil {
		return errClientRegistrationBackupFailed
	}
	target := s.recordPath(record.clientID, record.receipt.ID)
	if !exclusive {
		if err := validateClientRegistrationBackupStoragePath(target, false); err != nil {
			return errClientRegistrationBackupFailed
		}
	} else if _, err := os.Lstat(target); err == nil {
		return os.ErrExist
	} else if !errors.Is(err, os.ErrNotExist) {
		return errClientRegistrationBackupFailed
	}
	if err := os.Rename(tempName, target); err != nil {
		return errClientRegistrationBackupFailed
	}
	if err := secureClientRegistrationBackupFile(target); err != nil {
		return errClientRegistrationBackupFailed
	}
	return nil
}

func (s *clientRegistrationBackupStoreOnDisk) ensureQuotaFor(additional clientRegistrationBackupRecord) error {
	entries, err := os.ReadDir(s.path)
	if err != nil {
		return err
	}
	var records []clientRegistrationBackupFileInfo
	var total int64
	for _, entry := range entries {
		path := filepath.Join(s.path, entry.Name())
		if entry.IsDir() {
			return errClientRegistrationBackupNotFound
		}
		if strings.HasPrefix(entry.Name(), ".pending-") && strings.HasSuffix(entry.Name(), ".tmp") {
			if err := validateClientRegistrationBackupStoragePath(path, false); err != nil {
				return errClientRegistrationBackupFailed
			}
			info, err := os.Stat(path)
			if err != nil || info.Size() > maxClientRegistrationBackupFileBytes {
				return errClientRegistrationBackupFailed
			}
			if age := s.now().Sub(info.ModTime()); age > maxClientRegistrationPendingAge {
				if err := os.Remove(path); err != nil {
					return errClientRegistrationBackupFailed
				}
				continue
			}
			total += info.Size()
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".json") {
			return errClientRegistrationBackupNotFound
		}
		record, readErr := s.readRecord(path)
		if readErr != nil || validateClientRegistrationBackupRecord(record, record.clientID, record.receipt.ID) != nil {
			return errClientRegistrationBackupNotFound
		}
		if err := validateClientRegistrationBackupStoragePath(path, false); err != nil {
			return errClientRegistrationBackupFailed
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		records = append(records, clientRegistrationBackupFileInfo{
			name:      entry.Name(),
			path:      path,
			size:      info.Size(),
			createdAt: record.receipt.CreatedAt,
			restored:  record.receipt.Status == mcpClientBackupRestored,
		})
		total += info.Size()
	}
	additionalSize, err := encodedClientRegistrationBackupRecordSize(additional)
	if err != nil {
		return err
	}
	if additionalSize > maxClientRegistrationBackupFileBytes {
		return errClientRegistrationBackupFailed
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].createdAt.Before(records[j].createdAt)
	})
	for len(records) >= maxClientRegistrationBackupRecords || total+additionalSize > maxClientRegistrationBackupStoreBytes {
		pruned := false
		for index, record := range records {
			if !record.restored {
				continue
			}
			if err := validateClientRegistrationBackupStoragePath(record.path, false); err != nil {
				return errClientRegistrationBackupFailed
			}
			if err := os.Remove(record.path); err != nil {
				return errClientRegistrationBackupFailed
			}
			total -= record.size
			records = append(records[:index], records[index+1:]...)
			pruned = true
			break
		}
		if !pruned {
			return errClientRegistrationBackupFailed
		}
	}
	return nil
}

func encodedClientRegistrationBackupRecordSize(record clientRegistrationBackupRecord) (int64, error) {
	data, err := json.Marshal(clientRegistrationBackupEnvelopeFromRecord(record))
	if err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func clientRegistrationBackupEnvelopeFromRecord(record clientRegistrationBackupRecord) clientRegistrationBackupEnvelope {
	envelope := clientRegistrationBackupEnvelope{
		Version:        clientRegistrationBackupRecordVersion,
		ClientID:       record.clientID,
		ReceiptID:      record.receipt.ID,
		Status:         record.receipt.Status,
		CreatedAt:      record.receipt.CreatedAt.UTC(),
		OriginalExists: record.original.exists,
		OriginalBytes:  append([]byte(nil), record.original.bytes...),
		OriginalDigest: hex.EncodeToString(record.originalDigest[:]),
		HasApplied:     record.hasApplied,
		Restored:       record.restored,
	}
	if record.hasApplied {
		envelope.AppliedExists = record.applied.exists
		envelope.AppliedBytes = append([]byte(nil), record.applied.bytes...)
		envelope.AppliedDigest = hex.EncodeToString(record.appliedDigest[:])
	}
	return envelope
}

func clientRegistrationBackupRecordFromEnvelope(envelope clientRegistrationBackupEnvelope) (clientRegistrationBackupRecord, error) {
	if envelope.Version != clientRegistrationBackupRecordVersion {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	originalDigest, err := parseClientRegistrationDigest(envelope.OriginalDigest)
	if err != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	record := clientRegistrationBackupRecord{
		clientID:       envelope.ClientID,
		receipt:        MCPClientBackupReceipt{ID: envelope.ReceiptID, Status: envelope.Status, CreatedAt: envelope.CreatedAt},
		original:       clientRegistrationConfig{exists: envelope.OriginalExists, bytes: append([]byte(nil), envelope.OriginalBytes...)},
		originalDigest: originalDigest,
		hasApplied:     envelope.HasApplied,
		restored:       envelope.Restored,
	}
	if envelope.HasApplied {
		record.appliedDigest, err = parseClientRegistrationDigest(envelope.AppliedDigest)
		if err != nil {
			return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
		}
		record.applied = clientRegistrationConfig{exists: envelope.AppliedExists, bytes: append([]byte(nil), envelope.AppliedBytes...)}
	} else if envelope.AppliedExists || len(envelope.AppliedBytes) != 0 || envelope.AppliedDigest != "" {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	if validateClientRegistrationBackupRecord(record, record.clientID, record.receipt.ID) != nil {
		return clientRegistrationBackupRecord{}, errClientRegistrationBackupNotFound
	}
	return record, nil
}

func parseClientRegistrationDigest(value string) (clientRegistrationDigest, error) {
	var digest clientRegistrationDigest
	parsed, err := hex.DecodeString(value)
	if err != nil || len(parsed) != sha256.Size {
		return digest, fmt.Errorf("invalid digest")
	}
	copy(digest[:], parsed)
	return digest, nil
}

func validateClientRegistrationBackupRecord(record clientRegistrationBackupRecord, clientID, receiptID string) error {
	if record.clientID != clientID || !isSupportedMCPClientID(record.clientID) || record.receipt.ID != receiptID || !validClientRegistrationID(record.receipt.ID) {
		return errClientRegistrationBackupNotFound
	}
	if !validClientRegistrationConfig(record.original) || digestClientRegistrationConfig(record.original) != record.originalDigest {
		return errClientRegistrationBackupNotFound
	}
	if record.receipt.CreatedAt.IsZero() || record.receipt.CreatedAt.Location() == nil {
		return errClientRegistrationBackupNotFound
	}
	switch record.receipt.Status {
	case mcpClientBackupCreated:
		if record.hasApplied || record.restored {
			return errClientRegistrationBackupNotFound
		}
	case mcpClientBackupApplying, mcpClientBackupApplied:
		if !record.hasApplied || record.restored {
			return errClientRegistrationBackupNotFound
		}
	case mcpClientBackupRestored:
		if !record.hasApplied || !record.restored {
			return errClientRegistrationBackupNotFound
		}
	default:
		return errClientRegistrationBackupNotFound
	}
	if record.hasApplied {
		if !validClientRegistrationConfig(record.applied) || digestClientRegistrationConfig(record.applied) != record.appliedDigest {
			return errClientRegistrationBackupNotFound
		}
	}
	return nil
}
