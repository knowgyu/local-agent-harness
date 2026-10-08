package main

import (
	"context"
	"sort"
)

const uiRouteRetryNamedSecretCleanup = "/retry-named-secret-cleanup"

type namedSecretCleanupUIController interface {
	PendingNamedSecretCleanupIDs(context.Context) ([]string, error)
	RetryNamedSecretCleanup(context.Context, string) (pending bool, err error)
}

type namedSecretCleanupUIState struct {
	Status     string   `json:"status"`
	PendingIDs []string `json:"pending_ids"`
}

func currentNamedSecretCleanupUIState(ctx context.Context, controller NamedSecretController, views []NamedSecretView) namedSecretCleanupUIState {
	state := namedSecretCleanupUIState{Status: "unavailable", PendingIDs: []string{}}
	cleanup, ok := controller.(namedSecretCleanupUIController)
	if !ok {
		return state
	}

	ids, err := cleanup.PendingNamedSecretCleanupIDs(ctx)
	if err != nil || len(ids) > 256 {
		return state
	}

	if len(views) > 256 {
		return state
	}
	knownIDs := make(map[string]struct{}, len(views))
	for _, view := range views {
		if !namedSecretIDPattern.MatchString(view.ID) {
			return state
		}
		if _, duplicate := knownIDs[view.ID]; duplicate {
			return state
		}
		knownIDs[view.ID] = struct{}{}
	}

	pendingIDs := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if !namedSecretIDPattern.MatchString(id) {
			return state
		}
		if _, ok := knownIDs[id]; !ok {
			return state
		}
		if _, ok := seen[id]; ok {
			return state
		}
		seen[id] = struct{}{}
		pendingIDs = append(pendingIDs, id)
	}

	sort.Strings(pendingIDs)
	state.Status = "available"
	state.PendingIDs = pendingIDs
	return state
}
