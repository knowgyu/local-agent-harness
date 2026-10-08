package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	claudeRegistrationFileName = ".claude.json"
	claudeRegistrationMaxDepth = 128
)

var (
	errClaudeRegistrationConfigInvalid = errors.New("claude client configuration is invalid")
	errClaudeRegistrationSettingsRead  = errors.New("claude client settings are unavailable")
)

type claudeRegistrationOptions struct {
	configPath        string
	lookupExecutable  func(string) (string, error)
	currentExecutable func() (string, error)
	configDir         func() string
}

type claudeRegistrationAdapter struct {
	options claudeRegistrationOptions
}

type claudeMCPServerEntry struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func newClaudeRegistrationAdapter() *claudeRegistrationAdapter {
	return newClaudeRegistrationAdapterWithOptions(claudeRegistrationOptions{})
}

func newClaudeRegistrationAdapterWithOptions(options claudeRegistrationOptions) *claudeRegistrationAdapter {
	if options.lookupExecutable == nil {
		options.lookupExecutable = exec.LookPath
	}
	if options.currentExecutable == nil {
		options.currentExecutable = os.Executable
	}
	if options.configDir == nil {
		options.configDir = func() string {
			return os.Getenv("CLAUDE_CONFIG_DIR")
		}
	}
	return &claudeRegistrationAdapter{options: options}
}

func (a *claudeRegistrationAdapter) clientID() string {
	return mcpClientIDClaude
}

func (a *claudeRegistrationAdapter) inspect(ctx context.Context) (clientRegistrationSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationSnapshot{}, err
	}

	installed, err := a.installed()
	if err != nil {
		return clientRegistrationSnapshot{}, err
	}
	if !installed {
		return clientRegistrationSnapshot{
			status: claudeStatus(mcpClientInstalledNotObserved, mcpClientRegistrationNotObserved, mcpClientEvidenceNotObserved),
		}, nil
	}
	if a.hasCustomConfigDir() {
		return clientRegistrationSnapshot{
			status: claudeStatus(mcpClientInstalled, mcpClientRegistrationNotObserved, mcpClientEvidenceNotObserved),
		}, nil
	}

	configPath, err := a.settingsPath()
	if err != nil {
		return clientRegistrationSnapshot{}, errClaudeRegistrationSettingsRead
	}
	config, err := readClaudeConfig(configPath)
	if err != nil {
		if errors.Is(err, errClientRegistrationConfigTooLarge) {
			return clientRegistrationSnapshot{}, errClientRegistrationConfigTooLarge
		}
		return clientRegistrationSnapshot{}, errClaudeRegistrationSettingsRead
	}

	state := mcpClientRegistrationNotObserved
	evidence := mcpClientEvidenceNotObserved
	if err := validateClaudeConfig(config); err == nil {
		state, err = a.registrationState(config)
		if err != nil {
			return clientRegistrationSnapshot{}, err
		}
		if state != mcpClientRegistrationNotObserved {
			evidence = mcpClientEvidenceConfigObserved
		}
	}
	return clientRegistrationSnapshot{
		status: claudeStatus(mcpClientInstalled, state, evidence),
		config: config,
	}, nil
}

func (a *claudeRegistrationAdapter) plan(
	ctx context.Context,
	spec MCPRegistrationSpec,
	current clientRegistrationConfig,
) (clientRegistrationMutation, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationMutation{}, err
	}
	if err := a.requireAvailable(); err != nil {
		return clientRegistrationMutation{}, err
	}
	appPath, err := a.applicationPath()
	if err != nil {
		return clientRegistrationMutation{}, errClientRegistrationInvalidSpec
	}
	if err := validateClaudeRegistrationSpec(spec, appPath); err != nil {
		return clientRegistrationMutation{}, err
	}
	if len(current.bytes) > maxClientRegistrationConfigBytes {
		return clientRegistrationMutation{}, errClientRegistrationConfigTooLarge
	}
	if !current.exists && len(current.bytes) != 0 {
		return clientRegistrationMutation{}, errClaudeRegistrationConfigInvalid
	}

	root, servers, err := decodeClaudeConfig(current)
	if err != nil {
		return clientRegistrationMutation{}, err
	}
	projectHasName, err := claudeProjectHasServerName(root, spec.DisplayName)
	if err != nil {
		return clientRegistrationMutation{}, errClaudeRegistrationConfigInvalid
	}
	if projectHasName {
		return clientRegistrationMutation{}, errClientRegistrationNameConflict
	}

	desired := claudeMCPServerEntry{
		Type:    "stdio",
		Command: spec.Command,
		Args:    []string{"--mcp"},
	}
	if existing, ok := servers[spec.DisplayName]; ok {
		if claudeEntryMatches(existing, desired) {
			return clientRegistrationMutation{
				config:  current,
				changes: []string{mcpRegistrationChangeUnchanged},
			}, nil
		}
		return clientRegistrationMutation{}, errClientRegistrationNameConflict
	}

	entry, err := json.Marshal(desired)
	if err != nil {
		return clientRegistrationMutation{}, errClaudeRegistrationConfigInvalid
	}
	servers[spec.DisplayName] = entry
	serverBytes, err := json.Marshal(servers)
	if err != nil {
		return clientRegistrationMutation{}, errClaudeRegistrationConfigInvalid
	}
	root["mcpServers"] = serverBytes
	updated, err := json.Marshal(root)
	if err != nil {
		return clientRegistrationMutation{}, errClaudeRegistrationConfigInvalid
	}
	if len(updated) > maxClientRegistrationConfigBytes {
		return clientRegistrationMutation{}, errClientRegistrationConfigTooLarge
	}

	return clientRegistrationMutation{
		config: clientRegistrationConfig{
			exists: true,
			bytes:  updated,
		},
		changes: []string{mcpRegistrationChangeAdded},
	}, nil
}

func (a *claudeRegistrationAdapter) writeConfig(
	ctx context.Context,
	expected clientRegistrationConfig,
	replacement clientRegistrationConfig,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.requireAvailable(); err != nil {
		return err
	}
	if len(expected.bytes) > maxClientRegistrationConfigBytes || len(replacement.bytes) > maxClientRegistrationConfigBytes {
		return errClientRegistrationConfigTooLarge
	}
	if (!expected.exists && len(expected.bytes) != 0) || (!replacement.exists && len(replacement.bytes) != 0) {
		return errClaudeRegistrationConfigInvalid
	}
	if replacement.exists {
		if err := validateClaudeConfig(replacement); err != nil {
			return err
		}
	}

	configPath, err := a.settingsPath()
	if err != nil {
		return errClaudeRegistrationSettingsRead
	}
	current, err := readClaudeConfig(configPath)
	if err != nil {
		if errors.Is(err, errClientRegistrationConfigTooLarge) {
			return errClientRegistrationConfigTooLarge
		}
		return errClaudeRegistrationSettingsRead
	}
	if !sameClientRegistrationConfig(current, expected) {
		return errClientRegistrationStaleSettings
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	if !replacement.exists {
		if err := os.Remove(configPath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errClientRegistrationStaleSettings
			}
			return errClaudeRegistrationSettingsRead
		}
		return nil
	}
	return writeClaudeConfigAtomically(ctx, configPath, expected, replacement)
}

func (a *claudeRegistrationAdapter) verify(ctx context.Context) (MCPClientStatus, error) {
	snapshot, err := a.inspect(ctx)
	if err != nil {
		return MCPClientStatus{}, err
	}
	return snapshot.status, nil
}

func (a *claudeRegistrationAdapter) installed() (bool, error) {
	_, err := a.options.lookupExecutable("claude")
	if err == nil {
		return true, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return false, nil
	}
	return false, errClientRegistrationClientUnavailable
}

func (a *claudeRegistrationAdapter) hasCustomConfigDir() bool {
	return a.options.configDir() != ""
}

func (a *claudeRegistrationAdapter) requireAvailable() error {
	installed, err := a.installed()
	if err != nil {
		return err
	}
	if !installed || a.hasCustomConfigDir() {
		return errClientRegistrationClientUnavailable
	}
	return nil
}

func (a *claudeRegistrationAdapter) settingsPath() (string, error) {
	if a.options.configPath != "" {
		if !filepath.IsAbs(a.options.configPath) {
			return "", errClaudeRegistrationSettingsRead
		}
		return filepath.Clean(a.options.configPath), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errClaudeRegistrationSettingsRead
	}
	return filepath.Join(home, claudeRegistrationFileName), nil
}

func (a *claudeRegistrationAdapter) applicationPath() (string, error) {
	path, err := a.options.currentExecutable()
	if err != nil || !filepath.IsAbs(path) {
		return "", errClaudeRegistrationSettingsRead
	}
	return filepath.Clean(path), nil
}

func (a *claudeRegistrationAdapter) registrationState(config clientRegistrationConfig) (string, error) {
	root, servers, err := decodeClaudeConfig(config)
	if err != nil {
		return mcpClientRegistrationConflict, nil
	}
	projectHasName, err := claudeProjectHasServerName(root, mcpClientRegistrationDisplayName)
	if err != nil {
		return mcpClientRegistrationNotObserved, nil
	}
	if projectHasName {
		return mcpClientRegistrationConflict, nil
	}
	entry, ok := servers[mcpClientRegistrationDisplayName]
	if !ok {
		return mcpClientRegistrationNotRegistered, nil
	}
	appPath, err := a.applicationPath()
	if err != nil {
		return "", err
	}
	desired := claudeMCPServerEntry{
		Type:    "stdio",
		Command: appPath,
		Args:    []string{"--mcp"},
	}
	if claudeEntryMatches(entry, desired) {
		return mcpClientRegistrationRegistered, nil
	}
	return mcpClientRegistrationConflict, nil
}

func validateClaudeRegistrationSpec(spec MCPRegistrationSpec, appPath string) error {
	if spec.ClientID != mcpClientIDClaude || spec.DisplayName != mcpClientRegistrationDisplayName {
		return errClientRegistrationInvalidSpec
	}
	if spec.Transport != MCPTransportStdio {
		return errClientRegistrationUnsupportedTransport
	}
	if spec.Endpoint != "" || spec.Command != appPath || !filepath.IsAbs(spec.Command) {
		return errClientRegistrationInvalidSpec
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--mcp" || len(spec.Scope) != 1 || spec.Scope[0] != "user" {
		return errClientRegistrationInvalidSpec
	}
	return nil
}

func claudeStatus(installed, registration, evidence string) MCPClientStatus {
	return MCPClientStatus{
		ClientID:       mcpClientIDClaude,
		Installed:      installed,
		Registration:   registration,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         mcpClientBackupNotCreated,
		EvidenceSource: evidence,
	}
}

func readClaudeConfig(path string) (clientRegistrationConfig, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return clientRegistrationConfig{}, nil
		}
		return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
	}
	if !pathInfo.Mode().IsRegular() {
		return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return clientRegistrationConfig{}, nil
		}
		return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || !os.SameFile(pathInfo, info) {
		closeErr := file.Close()
		if closeErr != nil {
			return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
		}
		return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
	}
	if info.Size() > maxClientRegistrationConfigBytes {
		closeErr := file.Close()
		if closeErr != nil {
			return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
		}
		return clientRegistrationConfig{}, errClientRegistrationConfigTooLarge
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxClientRegistrationConfigBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return clientRegistrationConfig{}, errClaudeRegistrationSettingsRead
	}
	if len(contents) > maxClientRegistrationConfigBytes {
		return clientRegistrationConfig{}, errClientRegistrationConfigTooLarge
	}
	return clientRegistrationConfig{exists: true, bytes: contents}, nil
}

func writeClaudeConfigAtomically(
	ctx context.Context,
	path string,
	expected clientRegistrationConfig,
	replacement clientRegistrationConfig,
) error {
	directory := filepath.Dir(path)
	temp, err := os.CreateTemp(directory, ".lah-claude-config-*.tmp")
	if err != nil {
		return errClaudeRegistrationSettingsRead
	}
	tempPath := temp.Name()
	cleanup := func() error {
		if err := os.Remove(tempPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errClaudeRegistrationSettingsRead
		}
		return nil
	}
	fail := func(cause error) error {
		if closeErr := temp.Close(); closeErr != nil {
			cause = errClaudeRegistrationSettingsRead
		}
		if cleanupErr := cleanup(); cleanupErr != nil {
			return cleanupErr
		}
		return cause
	}

	if _, err := temp.Write(replacement.bytes); err != nil {
		return fail(errClaudeRegistrationSettingsRead)
	}
	if err := temp.Sync(); err != nil {
		return fail(errClaudeRegistrationSettingsRead)
	}
	if err := temp.Close(); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return cleanupErr
		}
		return errClaudeRegistrationSettingsRead
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return cleanupErr
		}
		return err
	}

	current, err := readClaudeConfig(path)
	if err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return cleanupErr
		}
		if errors.Is(err, errClientRegistrationConfigTooLarge) {
			return errClientRegistrationConfigTooLarge
		}
		return errClaudeRegistrationSettingsRead
	}
	if !sameClientRegistrationConfig(current, expected) {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return cleanupErr
		}
		return errClientRegistrationStaleSettings
	}
	if err := os.Rename(tempPath, path); err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return cleanupErr
		}
		return errClaudeRegistrationSettingsRead
	}
	return nil
}

func decodeClaudeConfig(config clientRegistrationConfig) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	if !config.exists {
		if len(config.bytes) != 0 {
			return nil, nil, errClaudeRegistrationConfigInvalid
		}
		return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
	}
	if len(config.bytes) == 0 || len(config.bytes) > maxClientRegistrationConfigBytes {
		return nil, nil, errClaudeRegistrationConfigInvalid
	}
	if err := validateUniqueClaudeJSON(config.bytes); err != nil {
		return nil, nil, errClaudeRegistrationConfigInvalid
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(config.bytes, &root); err != nil || root == nil {
		return nil, nil, errClaudeRegistrationConfigInvalid
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := root["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil || servers == nil {
			return nil, nil, errClaudeRegistrationConfigInvalid
		}
	}
	return root, servers, nil
}

func validateClaudeConfig(config clientRegistrationConfig) error {
	_, _, err := decodeClaudeConfig(config)
	return err
}

func validateUniqueClaudeJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeClaudeJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errClaudeRegistrationConfigInvalid
	}
	return nil
}

func consumeClaudeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > claudeRegistrationMaxDepth {
		return errClaudeRegistrationConfigInvalid
	}
	token, err := decoder.Token()
	if err != nil {
		return errClaudeRegistrationConfigInvalid
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return errClaudeRegistrationConfigInvalid
			}
			key, ok := keyToken.(string)
			if !ok {
				return errClaudeRegistrationConfigInvalid
			}
			if _, exists := seen[key]; exists {
				return errClaudeRegistrationConfigInvalid
			}
			seen[key] = struct{}{}
			if err := consumeClaudeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errClaudeRegistrationConfigInvalid
		}
	case '[':
		for decoder.More() {
			if err := consumeClaudeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errClaudeRegistrationConfigInvalid
		}
	default:
		return errClaudeRegistrationConfigInvalid
	}
	return nil
}

func claudeProjectHasServerName(root map[string]json.RawMessage, name string) (bool, error) {
	rawProjects, ok := root["projects"]
	if !ok {
		return false, nil
	}
	var projects map[string]json.RawMessage
	if err := json.Unmarshal(rawProjects, &projects); err != nil || projects == nil {
		return false, errClaudeRegistrationConfigInvalid
	}
	for _, rawProject := range projects {
		var project map[string]json.RawMessage
		if err := json.Unmarshal(rawProject, &project); err != nil || project == nil {
			return false, errClaudeRegistrationConfigInvalid
		}
		rawServers, ok := project["mcpServers"]
		if !ok {
			continue
		}
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(rawServers, &servers); err != nil || servers == nil {
			return false, errClaudeRegistrationConfigInvalid
		}
		if _, exists := servers[name]; exists {
			return true, nil
		}
	}
	return false, nil
}

func claudeEntryMatches(raw json.RawMessage, desired claudeMCPServerEntry) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || len(fields) != 3 {
		return false
	}
	if _, ok := fields["type"]; !ok {
		return false
	}
	if _, ok := fields["command"]; !ok {
		return false
	}
	if _, ok := fields["args"]; !ok {
		return false
	}
	var actual claudeMCPServerEntry
	if err := json.Unmarshal(raw, &actual); err != nil {
		return false
	}
	if actual.Type != desired.Type || actual.Command != desired.Command || len(actual.Args) != len(desired.Args) {
		return false
	}
	for index := range desired.Args {
		if actual.Args[index] != desired.Args[index] {
			return false
		}
	}
	return true
}
