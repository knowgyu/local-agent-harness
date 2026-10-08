package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
)

var (
	errSetupDraftInvalid  = errors.New("setup draft is invalid or no longer available")
	errSetupDraftExpired  = errors.New("setup draft has expired; submit it again")
	errSetupDraftStale    = errors.New("settings changed after this draft was prepared; submit it again")
	errSetupDraftStore    = errors.New("setup draft settings are unavailable")
	errSetupDraftCapacity = errors.New("too many setup drafts are awaiting review")
	errSetupDraftMissing  = errors.New("setup draft is missing information required to save; complete it and submit a new draft")
	errSetupDraftNoChange = errors.New("setup draft does not change registered settings")
)

type setupDraftStore interface {
	Snapshot() (setupDraftConfigSnapshot, error)
	CompareAndSwap(expectedRevision string, next setupDraftState) (string, error)
}

type setupDraftConfigSnapshot struct {
	Revision             string
	State                setupDraftState
	AvailableCredentials []NamedSecretView
}

type setupDraftState struct {
	Connections []setupDraftConnectionRecord
	Bundles     []setupDraftBundleRecord
	SSHTargets  []setupDraftSSHTargetRecord
}

type setupDraftConnectionRecord struct {
	ID             string
	Kind           SetupServiceKind
	Name           string
	Origin         string
	Repository     string
	Username       string
	Project        string
	JobPath        string
	Environment    string
	Namespace      string
	BaseURL        string
	CredentialName string
}

type setupDraftBundleRecord struct {
	ID         string
	Definition SetupDraftBundle
}

type setupDraftSSHTargetRecord struct {
	ID         string
	Definition SetupDraftSSHTarget
}

func cloneSetupDraftSnapshot(snapshot setupDraftConfigSnapshot) setupDraftConfigSnapshot {
	result := setupDraftConfigSnapshot{
		Revision:             snapshot.Revision,
		State:                cloneSetupDraftState(snapshot.State),
		AvailableCredentials: append([]NamedSecretView(nil), snapshot.AvailableCredentials...),
	}
	return result
}

func cloneSetupDraftState(state setupDraftState) setupDraftState {
	result := setupDraftState{
		Connections: append([]setupDraftConnectionRecord(nil), state.Connections...),
		Bundles:     make([]setupDraftBundleRecord, len(state.Bundles)),
		SSHTargets:  make([]setupDraftSSHTargetRecord, len(state.SSHTargets)),
	}
	for index, record := range state.Bundles {
		result.Bundles[index] = setupDraftBundleRecord{ID: record.ID, Definition: cloneSetupDraftBundle(record.Definition)}
	}
	for index, record := range state.SSHTargets {
		result.SSHTargets[index] = setupDraftSSHTargetRecord{ID: record.ID, Definition: cloneSetupDraftSSHTarget(record.Definition)}
	}
	return result
}

func cloneSetupDraftBundle(bundle SetupDraftBundle) SetupDraftBundle {
	result := bundle
	result.RepositoryIDs = append([]string(nil), bundle.RepositoryIDs...)
	result.Environments = append([]SetupDraftEnvironment(nil), bundle.Environments...)
	return result
}

func cloneSetupDraftSSHTarget(target SetupDraftSSHTarget) SetupDraftSSHTarget {
	result := SetupDraftSSHTarget{Name: target.Name, Alias: target.Alias, Operations: make([]SSHOperationDefinition, len(target.Operations))}
	for index, operation := range target.Operations {
		result.Operations[index] = cloneSSHOperation(operation)
	}
	return result
}

func setupDraftStateRevision(state setupDraftState) string {
	encoded, err := json.Marshal(cloneSetupDraftState(state))
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func connectionDraftRecord(id string, connection SetupDraftConnection) setupDraftConnectionRecord {
	return setupDraftConnectionRecord{
		ID: id, Kind: connection.Kind, Name: connection.Name, Origin: connection.Origin,
		Repository: connection.Repository, Username: connection.Username, Project: connection.Project,
		JobPath: connection.JobPath, Environment: connection.Environment, Namespace: connection.Namespace,
		BaseURL: connection.BaseURL, CredentialName: connection.CredentialName,
	}
}

func connectionDraftValue(record setupDraftConnectionRecord) SetupDraftConnection {
	return SetupDraftConnection{
		Kind: record.Kind, Action: SetupDraftReuse, ExistingID: record.ID, Name: record.Name,
		Origin: record.Origin, Repository: record.Repository, Username: record.Username,
		Project: record.Project, JobPath: record.JobPath, Environment: record.Environment,
		Namespace: record.Namespace, BaseURL: record.BaseURL, CredentialName: record.CredentialName,
	}
}
