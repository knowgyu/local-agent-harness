//go:build !windows

package main

import "path/filepath"

import "io"

// The onboarding sidecar uses the existing owner-only directory/file policy,
// flock coordination, and fsync-backed atomic replacement. Its distinct path
// yields a distinct lock from the setup-draft queue.
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
// root with the same owner-only permissions as the hash directory; do not
// alter the selected config directory's permissions.
func ensureOnboardingStoreRoot(path string) error {
	clean := filepath.Clean(path)
	if !validOnboardingStorePath(clean) {
		return errOnboardingUnavailable
	}
	root := filepath.Dir(filepath.Dir(clean))
	if root == filepath.Dir(root) {
		return errOnboardingUnavailable
	}
	return ensureSetupDraftQueueDirectory(filepath.Join(root, ".namespace"))
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

// The existing durable-queue non-Windows implementation shares this helper,
// which is defined alongside the Windows implementation in the legacy queue
// files. Provide the non-Windows build here so the package remains portable.
func writeFullQueueData(writer io.Writer, data []byte) error {
	for len(data) != 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
