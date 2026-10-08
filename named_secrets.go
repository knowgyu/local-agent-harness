package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	errNamedSecretInvalid  = errors.New("named secret input is invalid")
	errNamedSecretNotFound = errors.New("named secret is unavailable")
	errNamedSecretInUse    = errors.New("named secret is still in use")
	errNamedSecretStore    = errors.New("named secret credential store is unavailable")
	errNamedSecretSettings = errors.New("named secret settings could not be saved")
	errNamedSecretCleanup  = errors.New("named secret was saved, but an unused credential could not be removed")
	errNamedSecretRollback = errors.New("named secret settings could not be saved and credential restoration could not be confirmed")
)

type namedSecretController struct {
	configPath string
	store      secretStore
	cleanup    namedSecretCleanupQueue
	persist    func(string, config) error
	newID      func() (string, error)
	newRef     func() (string, error)
}

func newNamedSecretController(configPath string, store secretStore) *namedSecretController {
	cleanup, _ := newNamedSecretCleanupFileQueue()
	return &namedSecretController{
		configPath: configPath,
		store:      store,
		cleanup:    cleanup,
		persist:    writeConfig,
		newID:      func() (string, error) { return newTargetID("secret") },
		newRef:     newCredentialReference,
	}
}

var _ NamedSecretController = (*namedSecretController)(nil)

func (c *namedSecretController) ListNamedSecrets(ctx context.Context) ([]NamedSecretView, error) {
	if ctx == nil || c == nil || c.configPath == "" {
		return nil, errNamedSecretSettings
	}
	if ctx.Err() != nil {
		return nil, errNamedSecretSettings
	}
	result := []NamedSecretView{}
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errNamedSecretSettings
		}
		cfg, err := readConfig(c.configPath)
		if err != nil {
			return errNamedSecretSettings
		}
		result = namedSecretViews(cfg)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *namedSecretController) SaveNamedSecret(ctx context.Context, write NamedSecretWrite) (NamedSecretView, error) {
	if ctx == nil || c == nil || c.configPath == "" || c.store == nil {
		return NamedSecretView{}, errNamedSecretInvalid
	}
	if ctx.Err() != nil || !validNamedSecretInput(write) {
		return NamedSecretView{}, errNamedSecretInvalid
	}
	var result NamedSecretView
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errNamedSecretInvalid
		}
		cfg, err := readConfig(c.configPath)
		if err != nil {
			return errNamedSecretSettings
		}
		index := namedSecretIndex(cfg.NamedSecrets, write.ID)
		if write.ID != "" && index < 0 {
			return errNamedSecretNotFound
		}
		if write.Value == nil {
			if index < 0 {
				return errNamedSecretInvalid
			}
			if namedSecretNameInUse(cfg.NamedSecrets, write.Name, index) {
				return errNamedSecretInvalid
			}
			cfg.NamedSecrets[index].Name = write.Name
			cfg.NamedSecrets[index].Purpose = write.Purpose
			if err := c.writeSettings(cfg); err != nil {
				return errNamedSecretSettings
			}
			result = namedSecretView(cfg, index)
			return nil
		}
		if len(write.Value) == 0 || len(write.Value) > maxSecretSize {
			return errNamedSecretInvalid
		}
		if (index < 0 && len(cfg.NamedSecrets) >= maxNamedSecrets) || namedSecretNameInUse(cfg.NamedSecrets, write.Name, index) {
			return errNamedSecretInvalid
		}
		var cleanupIntents []namedSecretCleanupIntent
		if c.cleanup == nil {
			if index >= 0 && cfg.NamedSecrets[index].CredentialRef != "" {
				return errNamedSecretSettings
			}
		} else {
			cleanupIntents, err = c.loadCleanupQueueLocked(cfg)
			if err != nil {
				return errNamedSecretSettings
			}
			if index >= 0 && cfg.NamedSecrets[index].CredentialRef != "" {
				pending, cleanupErr := c.retryNamedSecretCleanupLocked(ctx, write.ID)
				if cleanupErr != nil || pending {
					return errNamedSecretSettings
				}
				cfg, err = readConfig(c.configPath)
				if err != nil {
					return errNamedSecretSettings
				}
				index = namedSecretIndex(cfg.NamedSecrets, write.ID)
				if index < 0 {
					return errNamedSecretNotFound
				}
				cleanupIntents, err = c.loadCleanupQueueLocked(cfg)
				if err != nil {
					return errNamedSecretSettings
				}
			}
		}

		id := write.ID
		if index < 0 {
			id, err = c.newID()
			if err != nil || !namedSecretIDPattern.MatchString(id) || namedSecretIndex(cfg.NamedSecrets, id) >= 0 {
				return errNamedSecretInvalid
			}
		}
		ref, err := c.newRef()
		if err != nil || !secretRefPattern.MatchString(ref) || configReferencesSecret(cfg, ref) || reservedNamedSecretCleanupRef(cleanupIntents, ref) {
			return errNamedSecretInvalid
		}
		value := append([]byte(nil), write.Value...)
		defer clearBytes(value)
		if ctx.Err() != nil {
			return errNamedSecretInvalid
		}

		oldRef := ""
		if index < 0 {
			cfg.NamedSecrets = append(cfg.NamedSecrets, namedSecretMetadata{ID: id, Name: write.Name, Purpose: write.Purpose, CredentialRef: ref})
			index = len(cfg.NamedSecrets) - 1
		} else {
			oldRef = cfg.NamedSecrets[index].CredentialRef
			replaceCredentialConsumers(&cfg, oldRef, ref)
			cfg.NamedSecrets[index].Name = write.Name
			cfg.NamedSecrets[index].Purpose = write.Purpose
			cfg.NamedSecrets[index].CredentialRef = ref
		}
		intentAdded := false
		if oldRef != "" {
			intent := namedSecretCleanupIntent{
				ID: id, OldRef: oldRef, NewRef: ref,
				DeleteOld: !configReferencesSecret(cfg, oldRef),
			}
			if c.cleanup == nil || c.cleanup.Put(intent) != nil {
				return errNamedSecretSettings
			}
			intentAdded = true
		}
		if err := c.store.Save(ref, value); err != nil {
			if intentAdded {
				if c.store.Delete(ref) != nil || c.cleanup.Remove(id, oldRef) != nil {
					return errNamedSecretRollback
				}
			}
			return errNamedSecretStore
		}
		if err := c.writeSettings(cfg); err != nil {
			stored, readErr := readConfig(c.configPath)
			if readErr == nil {
				storedIndex := namedSecretIndex(stored.NamedSecrets, id)
				if storedIndex >= 0 && stored.NamedSecrets[storedIndex].CredentialRef == ref {
					result = namedSecretView(stored, storedIndex)
					if intentAdded && c.cleanup.MarkCommitted(id, oldRef, ref) != nil {
						return errNamedSecretCleanup
					}
					pending, cleanupErr := c.retryNamedSecretCleanupLocked(ctx, id)
					if cleanupErr != nil || pending {
						return errNamedSecretCleanup
					}
					return nil
				}
				stillOld := (oldRef == "" && storedIndex < 0) ||
					(oldRef != "" && storedIndex >= 0 && stored.NamedSecrets[storedIndex].CredentialRef == oldRef)
				if stillOld && !configReferencesSecret(stored, ref) {
					if c.store.Delete(ref) != nil {
						return errNamedSecretRollback
					}
					if intentAdded && c.cleanup.Remove(id, oldRef) != nil {
						return errNamedSecretRollback
					}
					return errNamedSecretSettings
				}
			}
			return errNamedSecretRollback
		}
		// Settings are committed now, so preserve the canonical view even if
		// durable cleanup bookkeeping or old-credential deletion needs retry.
		result = namedSecretView(cfg, index)
		if intentAdded && c.cleanup.MarkCommitted(id, oldRef, ref) != nil {
			return errNamedSecretCleanup
		}
		if oldRef != "" && c.cleanup != nil {
			pending, cleanupErr := c.retryNamedSecretCleanupLocked(ctx, id)
			if cleanupErr != nil || pending {
				return errNamedSecretCleanup
			}
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func (c *namedSecretController) DeleteNamedSecret(ctx context.Context, id string) error {
	if ctx == nil || c == nil || c.configPath == "" || c.store == nil {
		return errNamedSecretInvalid
	}
	if ctx.Err() != nil || !namedSecretIDPattern.MatchString(id) {
		return errNamedSecretInvalid
	}
	return withConfigLock(func() error {
		if ctx.Err() != nil {
			return errNamedSecretInvalid
		}
		cfg, err := readConfig(c.configPath)
		if err != nil {
			return errNamedSecretSettings
		}
		index := namedSecretIndex(cfg.NamedSecrets, id)
		if index < 0 {
			return errNamedSecretNotFound
		}
		secret := cfg.NamedSecrets[index]
		pending, cleanupErr := c.retryNamedSecretCleanupLocked(ctx, id)
		if cleanupErr != nil || pending {
			return errNamedSecretCleanup
		}
		// Cleanup can consume stale pre-CAS intents, so use a fresh canonical
		// settings snapshot before deciding whether the active credential is in use.
		cfg, err = readConfig(c.configPath)
		if err != nil {
			return errNamedSecretSettings
		}
		index = namedSecretIndex(cfg.NamedSecrets, id)
		if index < 0 {
			return errNamedSecretNotFound
		}
		secret = cfg.NamedSecrets[index]
		if configHasCredentialConsumer(cfg, secret.CredentialRef) {
			return errNamedSecretInUse
		}
		if ctx.Err() != nil {
			return errNamedSecretInvalid
		}
		value, err := c.store.Load(secret.CredentialRef)
		defer clearBytes(value)
		if err != nil || len(value) == 0 || len(value) > maxSecretSize {
			return errNamedSecretStore
		}
		if ctx.Err() != nil {
			return errNamedSecretInvalid
		}
		if err := c.store.Delete(secret.CredentialRef); err != nil {
			return errNamedSecretStore
		}
		cfg.NamedSecrets = append(cfg.NamedSecrets[:index], cfg.NamedSecrets[index+1:]...)
		if err := c.writeSettings(cfg); err != nil {
			if c.store.Save(secret.CredentialRef, value) != nil {
				return errNamedSecretRollback
			}
			return errNamedSecretSettings
		}
		return nil
	})
}

func (c *namedSecretController) writeSettings(cfg config) error {
	if c != nil && c.persist != nil {
		return c.persist(c.configPath, cfg)
	}
	if c == nil {
		return errNamedSecretSettings
	}
	return writeConfig(c.configPath, cfg)
}

func reservedNamedSecretCleanupRef(intents []namedSecretCleanupIntent, ref string) bool {
	for _, intent := range intents {
		if intent.OldRef == ref || intent.NewRef == ref {
			return true
		}
	}
	return false
}

func validNamedSecretInput(write NamedSecretWrite) bool {
	validID := write.ID == "" || namedSecretIDPattern.MatchString(write.ID)
	validName := write.Name != "" && strings.TrimSpace(write.Name) == write.Name &&
		len(write.Name) <= maxNamedSecretName && utf8.ValidString(write.Name) && !strings.ContainsAny(write.Name, "\r\n\x00")
	validPurpose := len(write.Purpose) <= maxNamedSecretPurpose && utf8.ValidString(write.Purpose) &&
		!strings.ContainsAny(write.Purpose, "\r\n\x00")
	emptyValue := write.Value != nil && len(write.Value) == 0
	if !validID || !validName || !validPurpose || emptyValue {
		return false
	}
	for _, value := range write.Name + write.Purpose {
		if unicode.IsControl(value) {
			return false
		}
	}
	return true
}

func namedSecretNameInUse(secrets []namedSecretMetadata, name string, except int) bool {
	name = strings.ToLower(name)
	for i := range secrets {
		if i != except && strings.ToLower(secrets[i].Name) == name {
			return true
		}
	}
	return false
}

func namedSecretIndex(secrets []namedSecretMetadata, id string) int {
	for i := range secrets {
		if secrets[i].ID == id {
			return i
		}
	}
	return -1
}

func namedSecretViews(cfg config) []NamedSecretView {
	views := make([]NamedSecretView, 0, len(cfg.NamedSecrets))
	for i := range cfg.NamedSecrets {
		views = append(views, namedSecretView(cfg, i))
	}
	return views
}

func namedSecretView(cfg config, index int) NamedSecretView {
	if index < 0 || index >= len(cfg.NamedSecrets) {
		return NamedSecretView{}
	}
	secret := cfg.NamedSecrets[index]
	return NamedSecretView{
		ID:         secret.ID,
		Name:       secret.Name,
		Purpose:    secret.Purpose,
		Configured: true,
		InUse:      configHasCredentialConsumer(cfg, secret.CredentialRef),
	}
}

func newCredentialReference() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", errors.New("could not create credential reference")
	}
	return "cred:" + hex.EncodeToString(value), nil
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
