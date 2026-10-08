package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var errSSHConfigStoreInvalid = errors.New("SSH operation settings are unavailable.")

// sshConfigStore adapts the canonical settings file to the B06 store seam.
// The controller owns withConfigLock around every call; this adapter must stay
// lock-neutral so the config mutex is never acquired recursively.
type sshConfigStore struct {
	configPath string
}

func newSSHConfigStore(configPath string) *sshConfigStore {
	return &sshConfigStore{configPath: configPath}
}

func (s *sshConfigStore) Snapshot() (sshOperationConfigSnapshot, error) {
	cfg, err := s.readConfig()
	if err != nil {
		return sshOperationConfigSnapshot{}, err
	}
	targets := cloneSSHTargets(cfg.SSHTargets)
	if err := validateStoredSSHConfigTargets(targets); err != nil {
		return sshOperationConfigSnapshot{}, errSSHConfigStoreInvalid
	}
	revision, err := sshConfigTargetsRevision(targets)
	if err != nil {
		return sshOperationConfigSnapshot{}, errSSHConfigStoreInvalid
	}
	return sshOperationConfigSnapshot{Revision: revision, Targets: targets}, nil
}

func (s *sshConfigStore) CompareAndSwap(expectedRevision string, targets []sshTargetDefinition) (string, error) {
	cfg, err := s.readConfig()
	if err != nil {
		return "", err
	}
	currentRevision, err := sshConfigTargetsRevision(cfg.SSHTargets)
	if err != nil {
		return "", errSSHConfigStoreInvalid
	}
	if expectedRevision == "" || currentRevision != expectedRevision {
		return "", errSSHConfigurationStale
	}
	if err := validateStoredSSHConfigTargets(targets); err != nil {
		return "", errSSHDefinitionInvalid
	}

	replacement := cloneSSHTargets(targets)
	if len(replacement) == 0 {
		replacement = nil
	}
	cfg.SSHTargets = replacement
	if err := writeConfig(s.configPath, cfg); err != nil {
		return "", errSSHConfigurationUnavailable
	}
	return sshConfigTargetsRevision(replacement)
}

func (s *sshConfigStore) readConfig() (config, error) {
	if s == nil || s.configPath == "" {
		return config{}, errSSHConfigStoreInvalid
	}
	cfg, err := readConfig(s.configPath)
	if err != nil {
		return config{}, errSSHConfigStoreInvalid
	}
	return cfg, nil
}

func validateStoredSSHConfigTargets(targets []sshTargetDefinition) error {
	if err := validateSSHTargetSet(targets); err != nil {
		return errSSHConfigStoreInvalid
	}
	for _, target := range targets {
		if _, err := normalizeSSHTarget(target); err != nil {
			return errSSHConfigStoreInvalid
		}
		for _, operation := range target.Operations {
			if !isStoredSSHOperationValid(operation, target.ID) {
				return errSSHConfigStoreInvalid
			}
		}
	}
	return nil
}

func sshConfigTargetsRevision(targets []sshTargetDefinition) (string, error) {
	canonical := cloneSSHTargets(targets)
	if len(canonical) == 0 {
		canonical = []sshTargetDefinition{}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", errSSHConfigStoreInvalid
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
