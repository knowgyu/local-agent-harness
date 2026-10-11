//go:build windows

package main

import "path/filepath"

// The onboarding sidecar uses the same owner-only directory/file ACL policy,
// named-mutex coordination, and write-through atomic replacement as the
// existing durable setup-draft queue. Its distinct path yields a distinct lock.
func withOnboardingStoreLock(path string, fn func() error) error {
	if fn == nil || ensureOnboardingStoreRoot(path) != nil {
		return errOnboardingUnavailable
	}
	if err := withSetupDraftQueueLock(path, fn); err != nil {
		return err
	}
	return nil
}

// The sidecar lives below onboarding/<config-hash>/. Create the onboarding
// root with the same owner-only ACL as the hash directory; do not alter the
// selected config directory's permissions.
func ensureOnboardingStoreRoot(path string) error {
	clean := filepath.Clean(path)
	if !validOnboardingStorePath(clean) {
		return errOnboardingUnavailable
	}
	root := filepath.Dir(filepath.Dir(clean))
	if root == filepath.Dir(root) {
		return errOnboardingUnavailable
	}
	security, _, err := setupDraftWindowsSecurityAttributes()
	if err != nil {
		return errOnboardingUnavailable
	}
	return ensureSetupDraftQueueDirectory(filepath.Join(root, ".namespace"), security)
}

func readOnboardingStoreFile(path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > onboardingStoreMaxFileBytes {
		return nil, errOnboardingUnavailable
	}
	data, err := readSetupDraftQueueFile(path, maxBytes)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func writeOnboardingStoreFileAtomically(path string, data []byte) error {
	if len(data) == 0 || len(data) > onboardingStoreMaxFileBytes {
		return errOnboardingUnavailable
	}
	if err := writeSetupDraftQueueFileAtomically(path, data); err != nil {
		return errOnboardingUnavailable
	}
	return nil
}
