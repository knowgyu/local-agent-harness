package main

import (
	"context"
	"sort"
)

func (c *namedSecretController) PendingNamedSecretCleanupIDs(ctx context.Context) ([]string, error) {
	if ctx == nil || c == nil || c.configPath == "" || c.cleanup == nil || ctx.Err() != nil {
		return nil, errNamedSecretCleanupQueue
	}
	var result []string
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errNamedSecretCleanupQueue
		}
		cfg, err := readConfig(c.configPath)
		if err != nil {
			return errNamedSecretCleanupQueue
		}
		intents, err := c.loadCleanupQueueLocked(cfg)
		if err != nil {
			return errNamedSecretCleanupQueue
		}
		staged, orphaned, err := classifyNamedSecretCleanupIntents(cfg, intents)
		if err != nil {
			return errNamedSecretCleanupQueue
		}
		ids := make(map[string]struct{}, len(staged)+len(orphaned))
		for _, intent := range staged {
			ids[intent.ID] = struct{}{}
		}
		for _, intent := range orphaned {
			ids[intent.ID] = struct{}{}
		}
		result = make([]string, 0, len(ids))
		for id := range ids {
			result = append(result, id)
		}
		sort.Strings(result)
		return nil
	})
	if err != nil {
		return nil, errNamedSecretCleanupQueue
	}
	return result, nil
}

func (c *namedSecretController) RetryNamedSecretCleanup(ctx context.Context, id string) (bool, error) {
	if ctx == nil || c == nil || c.configPath == "" || c.cleanup == nil || c.store == nil ||
		ctx.Err() != nil || !namedSecretIDPattern.MatchString(id) {
		return false, errNamedSecretCleanupQueue
	}
	pending := false
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errNamedSecretCleanup
		}
		var err error
		pending, err = c.retryNamedSecretCleanupLocked(ctx, id)
		return err
	})
	if err != nil {
		if pending {
			return true, errNamedSecretCleanup
		}
		return false, errNamedSecretCleanupQueue
	}
	return pending, nil
}

func (c *namedSecretController) loadCleanupQueueLocked(cfg config) ([]namedSecretCleanupIntent, error) {
	if c == nil || c.cleanup == nil {
		return nil, errNamedSecretCleanupQueue
	}
	intents, err := c.cleanup.Load()
	if err != nil || validateNamedSecretCleanupOwners(cfg, intents) != nil {
		return nil, errNamedSecretCleanupQueue
	}
	return intents, nil
}

// retryNamedSecretCleanupLocked runs with the canonical settings lock held.
// Queue operations acquire their own lock only briefly; no queue lock is held
// while reading settings or calling Credential Manager.
func (c *namedSecretController) retryNamedSecretCleanupLocked(ctx context.Context, id string) (bool, error) {
	if c == nil || c.cleanup == nil || c.store == nil || !namedSecretIDPattern.MatchString(id) {
		return true, errNamedSecretCleanupQueue
	}
	cfg, err := readConfig(c.configPath)
	if err != nil {
		return true, errNamedSecretCleanupQueue
	}
	intents, err := c.loadCleanupQueueLocked(cfg)
	if err != nil {
		return true, errNamedSecretCleanupQueue
	}
	if len(intents) == 0 {
		return false, nil
	}
	staged, orphaned, err := classifyNamedSecretCleanupIntents(cfg, intents)
	if err != nil {
		return true, errNamedSecretCleanupQueue
	}
	for _, intent := range staged {
		if intent.ID != id {
			continue
		}
		if ctx.Err() != nil || c.store.Delete(intent.NewRef) != nil {
			return true, errNamedSecretCleanup
		}
		if c.cleanup.Remove(intent.ID, intent.OldRef) != nil {
			return true, errNamedSecretCleanup
		}
	}
	for _, intent := range orphaned {
		if intent.ID != id {
			continue
		}
		if !intent.Committed && c.cleanup.MarkCommitted(intent.ID, intent.OldRef, intent.NewRef) != nil {
			return true, errNamedSecretCleanup
		}
		if intent.DeleteOld {
			if ctx.Err() != nil || c.store.Delete(intent.OldRef) != nil {
				return true, errNamedSecretCleanup
			}
		}
		if c.cleanup.Remove(intent.ID, intent.OldRef) != nil {
			return true, errNamedSecretCleanup
		}
	}
	remaining, err := c.cleanup.Load()
	if err != nil {
		return true, errNamedSecretCleanupQueue
	}
	for _, intent := range remaining {
		if intent.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// Prepared records whose settings owner still points at oldRef represent a
// pre-CAS crash/failure; only the unreferenced staged credential may be
// removed. A committed record with oldRef active is treated as an ambiguity,
// never as permission to delete. A prepared record with owner==newRef proves
// the settings commit even when the commit-marker write was interrupted.
func classifyNamedSecretCleanupIntents(
	cfg config,
	intents []namedSecretCleanupIntent,
) (staged, orphaned []namedSecretCleanupIntent, err error) {
	if validateNamedSecretCleanupOwners(cfg, intents) != nil {
		return nil, nil, errNamedSecretCleanupQueue
	}
	for _, intent := range intents {
		index := namedSecretIndex(cfg.NamedSecrets, intent.ID)
		if index < 0 || !secretRefPattern.MatchString(cfg.NamedSecrets[index].CredentialRef) {
			return nil, nil, errNamedSecretCleanupQueue
		}
		ownerRef := cfg.NamedSecrets[index].CredentialRef
		if !intent.Committed && ownerRef == intent.OldRef {
			if configReferencesSecret(cfg, intent.NewRef) {
				return nil, nil, errNamedSecretCleanupQueue
			}
			staged = append(staged, intent)
			continue
		}
		if !intent.Committed && ownerRef != intent.NewRef {
			return nil, nil, errNamedSecretCleanupQueue
		}
		if ownerRef == intent.OldRef {
			return nil, nil, errNamedSecretCleanupQueue
		}
		if intent.DeleteOld && configReferencesSecret(cfg, intent.OldRef) {
			return nil, nil, errNamedSecretCleanupQueue
		}
		orphaned = append(orphaned, intent)
	}
	return staged, orphaned, nil
}
