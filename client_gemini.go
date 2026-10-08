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
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

const (
	geminiCLIExecutableName       = "gemini"
	geminiCLISettingsFileName     = "settings.json"
	geminiCLISettingsDirectory    = ".gemini"
	geminiCLIHomeEnvironment      = "GEMINI_CLI_HOME"
	geminiCLIRegistrationMaxBytes = 4 << 20
	geminiCLIRegistrationMaxDepth = 128
)

var (
	geminiCLIRegistrationNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	errGeminiCLISettingsUnavailable  = errors.New("gemini cli settings are unavailable")
	errGeminiCLISettingsInvalid      = errors.New("gemini cli settings cannot be processed safely")
)

type geminiCLIRegistrationAdapter struct {
	configPath string
	command    string
	lookPath   func(string) (string, error)
}

type geminiCLIRegistrationEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

func newGeminiCLIRegistrationAdapter() (*geminiCLIRegistrationAdapter, error) {
	command, err := os.Executable()
	if err != nil {
		return nil, errGeminiCLISettingsUnavailable
	}
	command, err = filepath.Abs(command)
	if err != nil || !filepath.IsAbs(command) {
		return nil, errGeminiCLISettingsUnavailable
	}

	home := os.Getenv(geminiCLIHomeEnvironment)
	if strings.TrimSpace(home) == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return nil, errGeminiCLISettingsUnavailable
		}
	}
	configPath, err := geminiCLISettingsPath(home)
	if err != nil {
		return nil, errGeminiCLISettingsUnavailable
	}

	return newGeminiCLIRegistrationAdapterForPath(configPath, command, exec.LookPath), nil
}

func newGeminiCLIRegistrationAdapterForPath(
	configPath string,
	command string,
	lookPath func(string) (string, error),
) *geminiCLIRegistrationAdapter {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	return &geminiCLIRegistrationAdapter{
		configPath: configPath,
		command:    command,
		lookPath:   lookPath,
	}
}

func geminiCLISettingsPath(home string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", errGeminiCLISettingsUnavailable
	}
	absoluteHome, err := filepath.Abs(home)
	if err != nil {
		return "", errGeminiCLISettingsUnavailable
	}
	return filepath.Join(absoluteHome, geminiCLISettingsDirectory, geminiCLISettingsFileName), nil
}

func (a *geminiCLIRegistrationAdapter) clientID() string {
	return mcpClientIDGemini
}

func (a *geminiCLIRegistrationAdapter) inspect(ctx context.Context) (clientRegistrationSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationSnapshot{}, err
	}
	if err := a.ensureCLIAvailable(); err != nil {
		return clientRegistrationSnapshot{status: geminiCLIUnobservedStatus()}, nil
	}
	config, err := a.readConfig(ctx)
	if err != nil {
		return clientRegistrationSnapshot{}, err
	}
	status, err := a.statusForConfig(config, mcpClientRegistrationDisplayName)
	if err != nil {
		return clientRegistrationSnapshot{}, err
	}
	return clientRegistrationSnapshot{status: status, config: config}, nil
}

func (a *geminiCLIRegistrationAdapter) plan(
	ctx context.Context,
	spec MCPRegistrationSpec,
	config clientRegistrationConfig,
) (clientRegistrationMutation, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationMutation{}, err
	}
	if err := a.ensureCLIAvailable(); err != nil {
		return clientRegistrationMutation{}, err
	}
	if err := a.validateSpec(spec); err != nil {
		return clientRegistrationMutation{}, err
	}
	if len(config.bytes) > geminiCLIRegistrationMaxBytes {
		return clientRegistrationMutation{}, errClientRegistrationConfigTooLarge
	}

	root, err := decodeGeminiSettings(config.bytes, config.exists)
	if err != nil {
		return clientRegistrationMutation{}, err
	}
	servers, err := decodeGeminiMCPServers(root)
	if err != nil {
		return clientRegistrationMutation{}, err
	}
	if rawEntry, exists := servers[spec.DisplayName]; exists {
		if geminiEntryMatches(rawEntry, spec.Command) {
			return clientRegistrationMutation{
				config:  config,
				changes: []string{mcpRegistrationChangeUnchanged},
			}, nil
		}
		return clientRegistrationMutation{}, errClientRegistrationNameConflict
	}

	entry, err := json.Marshal(geminiCLIRegistrationEntry{
		Command: spec.Command,
		Args:    []string{"--mcp"},
	})
	if err != nil {
		return clientRegistrationMutation{}, errGeminiCLISettingsInvalid
	}
	servers[spec.DisplayName] = entry
	encodedServers, err := json.Marshal(servers)
	if err != nil {
		return clientRegistrationMutation{}, errGeminiCLISettingsInvalid
	}
	root["mcpServers"] = encodedServers
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return clientRegistrationMutation{}, errGeminiCLISettingsInvalid
	}
	encoded = append(encoded, '\n')
	if len(encoded) > geminiCLIRegistrationMaxBytes {
		return clientRegistrationMutation{}, errClientRegistrationConfigTooLarge
	}
	return clientRegistrationMutation{
		config: clientRegistrationConfig{
			exists: true,
			bytes:  encoded,
		},
		changes: []string{mcpRegistrationChangeAdded},
	}, nil
}

func (a *geminiCLIRegistrationAdapter) writeConfig(
	ctx context.Context,
	expected clientRegistrationConfig,
	replacement clientRegistrationConfig,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.ensureCLIAvailable(); err != nil {
		return err
	}
	if len(expected.bytes) > geminiCLIRegistrationMaxBytes || len(replacement.bytes) > geminiCLIRegistrationMaxBytes {
		return errClientRegistrationConfigTooLarge
	}
	if (!expected.exists && len(expected.bytes) > 0) || (!replacement.exists && len(replacement.bytes) > 0) {
		return errGeminiCLISettingsInvalid
	}
	if replacement.exists {
		if _, err := decodeGeminiSettings(replacement.bytes, replacement.exists); err != nil {
			return err
		}
	}

	current, err := a.readConfig(ctx)
	if err != nil {
		return err
	}
	if !geminiConfigsEqual(current, expected) {
		return errClientRegistrationStaleSettings
	}
	if geminiConfigsEqual(expected, replacement) {
		return nil
	}

	directory := filepath.Dir(a.configPath)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errGeminiCLISettingsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !replacement.exists {
		return a.removeConfigAtomically(ctx, expected)
	}
	return a.replaceConfigAtomically(ctx, expected, replacement.bytes, directory)
}

func (a *geminiCLIRegistrationAdapter) verify(ctx context.Context) (MCPClientStatus, error) {
	if err := ctx.Err(); err != nil {
		return MCPClientStatus{}, err
	}
	if err := a.ensureCLIAvailable(); err != nil {
		return geminiCLIUnobservedStatus(), nil
	}
	config, err := a.readConfig(ctx)
	if err != nil {
		return MCPClientStatus{}, err
	}
	return a.statusForConfig(config, mcpClientRegistrationDisplayName)
}

func (a *geminiCLIRegistrationAdapter) validateSpec(spec MCPRegistrationSpec) error {
	if spec.Transport != MCPTransportStdio {
		return errClientRegistrationUnsupportedTransport
	}
	if spec.ClientID != mcpClientIDGemini ||
		spec.Endpoint != "" ||
		!filepath.IsAbs(spec.Command) ||
		!sameGeminiCommand(spec.Command, a.command) ||
		!sameGeminiStrings(spec.Args, []string{"--mcp"}) ||
		!sameGeminiStrings(spec.Scope, []string{"user"}) ||
		!geminiCLIRegistrationNamePattern.MatchString(spec.DisplayName) ||
		spec.DisplayName != mcpClientRegistrationDisplayName {
		return errClientRegistrationInvalidSpec
	}
	return nil
}

func (a *geminiCLIRegistrationAdapter) statusForConfig(
	config clientRegistrationConfig,
	name string,
) (MCPClientStatus, error) {
	status := MCPClientStatus{
		ClientID:       mcpClientIDGemini,
		Installed:      "installed",
		Registration:   "not_registered",
		Connection:     "unverified",
		ToolCall:       "unverified",
		Backup:         "not_created",
		EvidenceSource: "config_observed",
	}
	root, err := decodeGeminiSettings(config.bytes, config.exists)
	if err != nil {
		return MCPClientStatus{}, err
	}
	servers, err := decodeGeminiMCPServers(root)
	if err != nil {
		return MCPClientStatus{}, err
	}
	rawEntry, exists := servers[name]
	if !exists {
		return status, nil
	}
	if geminiEntryMatches(rawEntry, a.command) {
		status.Registration = "registered"
		return status, nil
	}
	status.Registration = "conflict"
	return status, nil
}

func geminiCLIUnobservedStatus() MCPClientStatus {
	return MCPClientStatus{
		ClientID:       mcpClientIDGemini,
		Installed:      "installed_cli_not_observed",
		Registration:   "not_observed",
		Connection:     "unverified",
		ToolCall:       "unverified",
		Backup:         "not_created",
		EvidenceSource: "not_observed",
	}
}

func (a *geminiCLIRegistrationAdapter) ensureCLIAvailable() error {
	// LookPath is discovery only; the adapter never starts the Gemini CLI.
	if a.lookPath == nil {
		return errClientRegistrationClientUnavailable
	}
	path, err := a.lookPath(geminiCLIExecutableName)
	if err != nil || strings.TrimSpace(path) == "" {
		return errClientRegistrationClientUnavailable
	}
	return nil
}

func (a *geminiCLIRegistrationAdapter) readConfig(ctx context.Context) (clientRegistrationConfig, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationConfig{}, err
	}
	info, err := os.Lstat(a.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return clientRegistrationConfig{}, nil
	}
	if err != nil {
		return clientRegistrationConfig{}, errGeminiCLISettingsUnavailable
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return clientRegistrationConfig{}, errGeminiCLISettingsInvalid
	}
	if info.Size() > geminiCLIRegistrationMaxBytes {
		return clientRegistrationConfig{}, errClientRegistrationConfigTooLarge
	}

	file, err := os.Open(a.configPath)
	if err != nil {
		return clientRegistrationConfig{}, errGeminiCLISettingsUnavailable
	}
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		if closeErr := file.Close(); closeErr != nil {
			return clientRegistrationConfig{}, errGeminiCLISettingsUnavailable
		}
		return clientRegistrationConfig{}, errGeminiCLISettingsUnavailable
	}
	contents, err := io.ReadAll(io.LimitReader(file, geminiCLIRegistrationMaxBytes+1))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return clientRegistrationConfig{}, errGeminiCLISettingsUnavailable
	}
	if len(contents) > geminiCLIRegistrationMaxBytes {
		return clientRegistrationConfig{}, errClientRegistrationConfigTooLarge
	}
	if err := ctx.Err(); err != nil {
		return clientRegistrationConfig{}, err
	}
	return clientRegistrationConfig{exists: true, bytes: contents}, nil
}

func (a *geminiCLIRegistrationAdapter) replaceConfigAtomically(
	ctx context.Context,
	expected clientRegistrationConfig,
	replacement []byte,
	directory string,
) (resultErr error) {
	temporary, err := os.CreateTemp(directory, ".lah-gemini-settings-*")
	if err != nil {
		return errGeminiCLISettingsUnavailable
	}
	temporaryPath := temporary.Name()
	defer func() {
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errGeminiCLISettingsUnavailable
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return errGeminiCLISettingsUnavailable
		}
		return errGeminiCLISettingsUnavailable
	}
	if _, err := temporary.Write(replacement); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return errGeminiCLISettingsUnavailable
		}
		return errGeminiCLISettingsUnavailable
	}
	if err := temporary.Sync(); err != nil {
		if closeErr := temporary.Close(); closeErr != nil {
			return errGeminiCLISettingsUnavailable
		}
		return errGeminiCLISettingsUnavailable
	}
	if err := temporary.Close(); err != nil {
		return errGeminiCLISettingsUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := a.readConfig(ctx)
	if err != nil {
		return err
	}
	if !geminiConfigsEqual(current, expected) {
		return errClientRegistrationStaleSettings
	}
	if err := os.Rename(temporaryPath, a.configPath); err != nil {
		return errGeminiCLISettingsUnavailable
	}
	return nil
}

func (a *geminiCLIRegistrationAdapter) removeConfigAtomically(
	ctx context.Context,
	expected clientRegistrationConfig,
) error {
	if !expected.exists {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	current, err := a.readConfig(ctx)
	if err != nil {
		return err
	}
	if !geminiConfigsEqual(current, expected) {
		return errClientRegistrationStaleSettings
	}
	if err := os.Remove(a.configPath); err != nil {
		return errGeminiCLISettingsUnavailable
	}
	return nil
}

func decodeGeminiSettings(contents []byte, exists bool) (map[string]json.RawMessage, error) {
	if !exists {
		if len(contents) != 0 {
			return nil, errGeminiCLISettingsInvalid
		}
		return map[string]json.RawMessage{}, nil
	}
	if len(contents) > geminiCLIRegistrationMaxBytes {
		return nil, errClientRegistrationConfigTooLarge
	}
	if !utf8.Valid(contents) {
		return nil, errGeminiCLISettingsInvalid
	}
	if err := validateGeminiJSON(contents); err != nil {
		return nil, errGeminiCLISettingsInvalid
	}
	root := map[string]json.RawMessage{}
	if err := json.Unmarshal(contents, &root); err != nil || root == nil {
		return nil, errGeminiCLISettingsInvalid
	}
	return root, nil
}

func decodeGeminiMCPServers(root map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, exists := root["mcpServers"]
	if !exists {
		return map[string]json.RawMessage{}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errGeminiCLISettingsInvalid
	}
	servers := map[string]json.RawMessage{}
	if err := json.Unmarshal(trimmed, &servers); err != nil || servers == nil {
		return nil, errGeminiCLISettingsInvalid
	}
	return servers, nil
}

func geminiEntryMatches(raw json.RawMessage, command string) bool {
	entry := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &entry); err != nil || entry == nil || len(entry) != 2 {
		return false
	}
	var actualCommand string
	if err := json.Unmarshal(entry["command"], &actualCommand); err != nil || actualCommand != command {
		return false
	}
	var args []string
	if err := json.Unmarshal(entry["args"], &args); err != nil {
		return false
	}
	return sameGeminiStrings(args, []string{"--mcp"})
}

func validateGeminiJSON(contents []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if _, err := consumeGeminiJSONValue(decoder, token, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing json data")
	}
	return nil
}

func consumeGeminiJSONValue(decoder *json.Decoder, token json.Token, depth int) (json.Token, error) {
	if depth > geminiCLIRegistrationMaxDepth {
		return nil, errors.New("json nesting limit exceeded")
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return token, nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, errors.New("invalid object key")
			}
			if _, exists := seen[key]; exists {
				return nil, errors.New("duplicate object key")
			}
			seen[key] = struct{}{}
			valueToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			if _, err := consumeGeminiJSONValue(decoder, valueToken, depth+1); err != nil {
				return nil, err
			}
		}
		closingToken, err := decoder.Token()
		if err != nil || closingToken != json.Delim('}') {
			return nil, errors.New("invalid object termination")
		}
	case '[':
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			if _, err := consumeGeminiJSONValue(decoder, valueToken, depth+1); err != nil {
				return nil, err
			}
		}
		closingToken, err := decoder.Token()
		if err != nil || closingToken != json.Delim(']') {
			return nil, errors.New("invalid array termination")
		}
	default:
		return nil, errors.New("unexpected json delimiter")
	}
	return token, nil
}

func geminiConfigsEqual(left clientRegistrationConfig, right clientRegistrationConfig) bool {
	return left.exists == right.exists && bytes.Equal(left.bytes, right.bytes)
}

func sameGeminiStrings(left []string, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sameGeminiCommand(left string, right string) bool {
	leftPath, leftErr := filepath.Abs(left)
	rightPath, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	leftPath = filepath.Clean(leftPath)
	rightPath = filepath.Clean(rightPath)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(leftPath, rightPath)
	}
	return leftPath == rightPath
}
