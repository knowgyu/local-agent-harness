package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var (
	errSSHConfigurationUnavailable = errors.New("SSH operation settings are unavailable.")
	errSSHConfigurationStale       = errors.New("SSH operation settings changed. Review and save again.")
	errSSHDefinitionInvalid        = errors.New("The SSH target or operation is invalid.")
	errSSHTargetNotFound           = errors.New("The registered SSH target or operation was not found.")
	errSSHOperationRejected        = errors.New("The SSH operation was not run because its approval or scope is invalid.")
)

var sshAliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type sshTargetDefinition struct {
	ID         string                   `json:"id"`
	Name       string                   `json:"name"`
	Alias      string                   `json:"alias"`
	Operations []SSHOperationDefinition `json:"operations"`
}

type sshOperationConfigSnapshot struct {
	Revision string                `json:"revision"`
	Targets  []sshTargetDefinition `json:"targets"`
}

// sshOperationStore is a B06-private persistence seam. The central config
// adapter belongs to B01/integration and must protect Snapshot and CAS using
// the app config lock. This worker only supplies the fake-backed controller.
type sshOperationStore interface {
	Snapshot() (sshOperationConfigSnapshot, error)
	CompareAndSwap(expectedRevision string, targets []sshTargetDefinition) (string, error)
}

type sshOperationRunner interface {
	Start(context.Context, string, string) (sshRunningProcess, error)
}

type sshRunningProcess interface {
	Wait() sshProcessResult
}

type sshApprovalRequest struct {
	TargetName string
	Operation  string
	Summary    string
	Parameters []sshApprovalParameter
}

type sshOperationApprover interface {
	Confirm(context.Context, sshApprovalRequest) (bool, error)
}

type sshOperationController struct {
	store    sshOperationStore
	runner   sshOperationRunner
	approver sshOperationApprover
	mu       sync.Mutex
}

func newSSHOperationController(store sshOperationStore, runner sshOperationRunner, approver sshOperationApprover) *sshOperationController {
	return &sshOperationController{store: store, runner: runner, approver: approver}
}

func (c *sshOperationController) ListSSHTargets(ctx context.Context) ([]SSHTargetView, error) {
	snapshot, err := c.loadSnapshot(ctx)
	if err != nil {
		return nil, errSSHConfigurationUnavailable
	}
	targets := make([]SSHTargetView, 0, len(snapshot.Targets))
	for _, definition := range snapshot.Targets {
		targets = append(targets, SSHTargetView{
			ID:         definition.ID,
			Name:       definition.Name,
			Alias:      definition.Alias,
			Operations: sshOperationViews(definition.Operations),
		})
	}
	sort.Slice(targets, func(i, j int) bool {
		left, right := strings.ToLower(targets[i].Name), strings.ToLower(targets[j].Name)
		if left == right {
			return targets[i].ID < targets[j].ID
		}
		return left < right
	})
	return targets, nil
}

func (c *sshOperationController) ListSSHOperationCatalog(ctx context.Context) ([]SSHOperationCatalogTarget, error) {
	snapshot, err := c.loadSnapshot(ctx)
	if err != nil {
		return nil, errSSHConfigurationUnavailable
	}
	targets := make([]SSHOperationCatalogTarget, 0, len(snapshot.Targets))
	for _, definition := range snapshot.Targets {
		targets = append(targets, SSHOperationCatalogTarget{
			ID:         definition.ID,
			Name:       definition.Name,
			Operations: sshOperationViews(definition.Operations),
		})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].ID < targets[j].ID })
	return targets, nil
}

func (c *sshOperationController) SaveTarget(ctx context.Context, requested sshTargetDefinition, expectedRevision string) (sshTargetDefinition, string, error) {
	if c == nil || c.store == nil {
		return sshTargetDefinition{}, "", errSSHConfigurationUnavailable
	}
	if requested.ID == "" {
		id, err := newSSHDefinitionID("target")
		if err != nil {
			return sshTargetDefinition{}, "", errSSHConfigurationUnavailable
		}
		requested.ID = id
	}
	var saved sshTargetDefinition
	newRevision, err := c.mutateConfiguration(ctx, expectedRevision, func(snapshot sshOperationConfigSnapshot) ([]sshTargetDefinition, error) {
		target, err := normalizeSSHTarget(requested)
		if err != nil {
			return nil, err
		}
		targets := cloneSSHTargets(snapshot.Targets)
		index := -1
		for i, current := range targets {
			if current.ID == target.ID {
				index = i
				continue
			}
			if strings.EqualFold(current.Name, target.Name) || strings.EqualFold(current.Alias, target.Alias) {
				return nil, errSSHDefinitionInvalid
			}
		}
		if index >= 0 {
			targets[index] = target
		} else {
			targets = append(targets, target)
		}
		if err := validateSSHTargetSet(targets); err != nil {
			return nil, err
		}
		saved = target
		return targets, nil
	})
	if err != nil {
		return sshTargetDefinition{}, "", err
	}
	return cloneSSHTarget(saved), newRevision, nil
}

func (c *sshOperationController) SaveOperation(ctx context.Context, targetID string, requested SSHOperationDefinition, expectedRevision string) (SSHOperationDefinition, string, error) {
	if c == nil || c.store == nil {
		return SSHOperationDefinition{}, "", errSSHConfigurationUnavailable
	}
	if requested.ID == "" {
		id, err := newSSHDefinitionID("operation")
		if err != nil {
			return SSHOperationDefinition{}, "", errSSHConfigurationUnavailable
		}
		requested.ID = id
	}
	var saved SSHOperationDefinition
	newRevision, err := c.mutateConfiguration(ctx, expectedRevision, func(snapshot sshOperationConfigSnapshot) ([]sshTargetDefinition, error) {
		targets := cloneSSHTargets(snapshot.Targets)
		targetIndex := findSSHTargetIndex(targets, targetID)
		if targetIndex < 0 {
			return nil, errSSHTargetNotFound
		}
		operation, err := normalizeSSHOperationDefinition(requested, targets[targetIndex].ID)
		if err != nil {
			return nil, err
		}
		operations := append([]SSHOperationDefinition(nil), targets[targetIndex].Operations...)
		index := -1
		for i, current := range operations {
			if current.ID == operation.ID {
				index = i
				continue
			}
			if strings.EqualFold(current.Name, operation.Name) {
				return nil, errSSHDefinitionInvalid
			}
		}
		if index >= 0 {
			operations[index] = operation
		} else {
			operations = append(operations, operation)
		}
		targets[targetIndex].Operations = operations
		if err := validateSSHTargetSet(targets); err != nil {
			return nil, err
		}
		saved = cloneSSHOperation(operation)
		return targets, nil
	})
	if err != nil {
		return SSHOperationDefinition{}, "", err
	}
	return saved, newRevision, nil
}

func (c *sshOperationController) DeleteTarget(ctx context.Context, targetID, expectedRevision string) (string, error) {
	newRevision, err := c.mutateConfiguration(ctx, expectedRevision, func(snapshot sshOperationConfigSnapshot) ([]sshTargetDefinition, error) {
		targets := cloneSSHTargets(snapshot.Targets)
		index := findSSHTargetIndex(targets, targetID)
		if index < 0 {
			return nil, errSSHTargetNotFound
		}
		return append(targets[:index], targets[index+1:]...), nil
	})
	return newRevision, err
}

func (c *sshOperationController) DeleteOperation(ctx context.Context, targetID, operationID, expectedRevision string) (string, error) {
	newRevision, err := c.mutateConfiguration(ctx, expectedRevision, func(snapshot sshOperationConfigSnapshot) ([]sshTargetDefinition, error) {
		targets := cloneSSHTargets(snapshot.Targets)
		targetIndex := findSSHTargetIndex(targets, targetID)
		if targetIndex < 0 {
			return nil, errSSHTargetNotFound
		}
		operations := targets[targetIndex].Operations
		operationIndex := -1
		for i, operation := range operations {
			if operation.ID == operationID {
				operationIndex = i
				break
			}
		}
		if operationIndex < 0 {
			return nil, errSSHTargetNotFound
		}
		targets[targetIndex].Operations = append(operations[:operationIndex], operations[operationIndex+1:]...)
		return targets, nil
	})
	return newRevision, err
}

func (c *sshOperationController) mutateConfiguration(ctx context.Context, expectedRevision string, update func(sshOperationConfigSnapshot) ([]sshTargetDefinition, error)) (string, error) {
	if c == nil || c.store == nil {
		return "", errSSHConfigurationUnavailable
	}
	if err := checkSSHContext(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var newRevision string
	err := withConfigLock(func() error {
		if err := checkSSHContext(ctx); err != nil {
			return err
		}
		snapshot, err := c.store.Snapshot()
		if err != nil {
			return errSSHConfigurationUnavailable
		}
		if snapshot.Revision != expectedRevision {
			return errSSHConfigurationStale
		}
		targets, err := update(cloneSSHOperationSnapshot(snapshot))
		if err != nil {
			return err
		}
		newRevision, err = c.store.CompareAndSwap(snapshot.Revision, cloneSSHTargets(targets))
		if err != nil {
			return errSSHConfigurationUnavailable
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newRevision, nil
}

func (c *sshOperationController) loadSnapshot(ctx context.Context) (sshOperationConfigSnapshot, error) {
	if c == nil || c.store == nil {
		return sshOperationConfigSnapshot{}, errSSHConfigurationUnavailable
	}
	if err := checkSSHContext(ctx); err != nil {
		return sshOperationConfigSnapshot{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var snapshot sshOperationConfigSnapshot
	err := withConfigLock(func() error {
		if err := checkSSHContext(ctx); err != nil {
			return err
		}
		var err error
		snapshot, err = c.store.Snapshot()
		if err != nil {
			return errSSHConfigurationUnavailable
		}
		snapshot = cloneSSHOperationSnapshot(snapshot)
		return nil
	})
	if err != nil {
		return sshOperationConfigSnapshot{}, err
	}
	return snapshot, nil
}

func normalizeSSHTarget(target sshTargetDefinition) (sshTargetDefinition, error) {
	if !validSSHDefinitionID(target.ID) || !validSSHDisplayText(target.Name, 80) ||
		!sshAliasPattern.MatchString(target.Alias) || len(target.Operations) > maxSSHTargetOperations {
		return sshTargetDefinition{}, errSSHDefinitionInvalid
	}
	result := cloneSSHTarget(target)
	seenIDs := make(map[string]struct{}, len(result.Operations))
	seenNames := make([]string, 0, len(result.Operations))
	for i := range result.Operations {
		operation, err := normalizeSSHOperationDefinition(result.Operations[i], result.ID)
		if err != nil {
			return sshTargetDefinition{}, errSSHDefinitionInvalid
		}
		if _, exists := seenIDs[operation.ID]; exists {
			return sshTargetDefinition{}, errSSHDefinitionInvalid
		}
		seenIDs[operation.ID] = struct{}{}
		for _, prior := range seenNames {
			if strings.EqualFold(prior, operation.Name) {
				return sshTargetDefinition{}, errSSHDefinitionInvalid
			}
		}
		seenNames = append(seenNames, operation.Name)
		result.Operations[i] = operation
	}
	return result, nil
}

func validateSSHTargetSet(targets []sshTargetDefinition) error {
	if len(targets) > maxSSHTargetCount {
		return errSSHDefinitionInvalid
	}
	seenIDs := make(map[string]struct{}, len(targets))
	seenNames := make([]string, 0, len(targets))
	seenAliases := make([]string, 0, len(targets))
	for _, target := range targets {
		if !validSSHDefinitionID(target.ID) || !validSSHDisplayText(target.Name, 80) || !sshAliasPattern.MatchString(target.Alias) || len(target.Operations) > maxSSHTargetOperations {
			return errSSHDefinitionInvalid
		}
		if _, exists := seenIDs[target.ID]; exists {
			return errSSHDefinitionInvalid
		}
		seenIDs[target.ID] = struct{}{}
		for _, prior := range seenNames {
			if strings.EqualFold(prior, target.Name) {
				return errSSHDefinitionInvalid
			}
		}
		for _, prior := range seenAliases {
			if strings.EqualFold(prior, target.Alias) {
				return errSSHDefinitionInvalid
			}
		}
		seenNames = append(seenNames, target.Name)
		seenAliases = append(seenAliases, target.Alias)
	}
	return nil
}

func validSSHDefinitionID(value string) bool {
	if len(value) == 0 || len(value) > 80 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_')) {
			continue
		}
		return false
	}
	return true
}

func validSSHDisplayText(value string, maxRunes int) bool {
	if strings.TrimSpace(value) == "" || len([]rune(value)) > maxRunes {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return false
		}
	}
	return true
}

func findSSHTargetIndex(targets []sshTargetDefinition, id string) int {
	for index, target := range targets {
		if target.ID == id {
			return index
		}
	}
	return -1
}

func cloneSSHOperationSnapshot(snapshot sshOperationConfigSnapshot) sshOperationConfigSnapshot {
	return sshOperationConfigSnapshot{Revision: snapshot.Revision, Targets: cloneSSHTargets(snapshot.Targets)}
}

func cloneSSHTargets(targets []sshTargetDefinition) []sshTargetDefinition {
	result := make([]sshTargetDefinition, len(targets))
	for index, target := range targets {
		result[index] = cloneSSHTarget(target)
	}
	return result
}

func cloneSSHTarget(target sshTargetDefinition) sshTargetDefinition {
	result := target
	result.Operations = make([]SSHOperationDefinition, len(target.Operations))
	for index, operation := range target.Operations {
		result.Operations[index] = cloneSSHOperation(operation)
	}
	return result
}

func cloneSSHOperation(operation SSHOperationDefinition) SSHOperationDefinition {
	result := operation
	result.FixedArgs = append([]string(nil), operation.FixedArgs...)
	result.Parameters = append([]SSHParameterSpec(nil), operation.Parameters...)
	result.Approval.Scope = append([]string(nil), operation.Approval.Scope...)
	return result
}

func sshOperationViews(definitions []SSHOperationDefinition) []SSHOperationView {
	views := make([]SSHOperationView, 0, len(definitions))
	for _, definition := range definitions {
		views = append(views, SSHOperationView{
			ID:         definition.ID,
			Name:       definition.Name,
			Summary:    definition.Summary,
			Parameters: append([]SSHParameterSpec(nil), definition.Parameters...),
			Risk:       definition.Risk,
			Approval:   OperationApprovalPolicy{Mode: definition.Approval.Mode},
			Revision:   definition.Revision,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		left, right := strings.ToLower(views[i].Name), strings.ToLower(views[j].Name)
		if left == right {
			return views[i].ID < views[j].ID
		}
		return left < right
	})
	return views
}

func newSSHDefinitionID(kind string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return kind + "_" + hex.EncodeToString(raw[:]), nil
}
