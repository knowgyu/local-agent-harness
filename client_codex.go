package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	codexClientID          = mcpClientIDCodex
	codexManagedServerName = mcpClientRegistrationDisplayName
	codexConfigFileName    = "config.toml"
	codexMaxConfigBytes    = 4 << 20
)

var (
	errCodexSettingsUnreadable = errors.New("Codex settings could not be read safely")
	errCodexSettingsWrite      = errors.New("Codex settings could not be updated safely")
	codexTOMLScalarPattern     = regexp.MustCompile(
		`^(?:true|false|[+-]?(?:inf|nan)|` +
			`[+-]?(?:0|[1-9](?:_?[0-9])*)(?:\.[0-9](?:_?[0-9])*)?(?:[eE][+-]?[0-9](?:_?[0-9])*)?|` +
			`0x[0-9A-Fa-f](?:_?[0-9A-Fa-f])*|0o[0-7](?:_?[0-7])*|0b[01](?:_?[01])*|` +
			`[0-9]{4}-[0-9]{2}-[0-9]{2}|[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?|` +
			`[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt ]+[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:[Zz]|[+-][0-9]{2}:[0-9]{2})?)$`,
	)
)

type codexClientRegistrationAdapter struct {
	configPath      string
	appExecutable   string
	codexExecutable string
}

type codexClientRegistrationAdapterOptions struct {
	configPath      string
	appExecutable   string
	codexExecutable string
}

type codexConfigEntry struct {
	exists bool
	array  bool
	nested bool
	extra  bool
	values map[string]string
}

type codexTOMLArrayScope struct {
	path       []string
	occurrence int
}

type codexTOMLAssignmentNode struct {
	children map[string]*codexTOMLAssignmentNode
	assigned bool
}

func newCodexClientRegistrationAdapter() (*codexClientRegistrationAdapter, error) {
	configPath, err := codexConfigPath()
	if err != nil {
		return nil, errCodexSettingsUnreadable
	}

	appExecutable, err := os.Executable()
	if err != nil || !filepath.IsAbs(appExecutable) {
		return nil, errCodexSettingsUnreadable
	}

	codexExecutable, _ := exec.LookPath("codex")
	return newCodexClientRegistrationAdapterWithOptions(codexClientRegistrationAdapterOptions{
		configPath:      configPath,
		appExecutable:   appExecutable,
		codexExecutable: codexExecutable,
	})
}

func newCodexClientRegistrationAdapterWithOptions(
	options codexClientRegistrationAdapterOptions,
) (*codexClientRegistrationAdapter, error) {
	if options.configPath == "" || !filepath.IsAbs(options.configPath) {
		return nil, errCodexSettingsUnreadable
	}
	if options.appExecutable == "" || !filepath.IsAbs(options.appExecutable) {
		return nil, errCodexSettingsUnreadable
	}
	if options.codexExecutable != "" && !filepath.IsAbs(options.codexExecutable) {
		return nil, errCodexSettingsUnreadable
	}

	return &codexClientRegistrationAdapter{
		configPath:      filepath.Clean(options.configPath),
		appExecutable:   filepath.Clean(options.appExecutable),
		codexExecutable: filepath.Clean(options.codexExecutable),
	}, nil
}

func codexConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", errCodexSettingsUnreadable
	}

	codexHome := os.Getenv("CODEX_HOME")
	if codexHome != "" {
		resolved, absErr := filepath.Abs(codexHome)
		if absErr == nil {
			if info, statErr := os.Stat(resolved); statErr == nil && info.IsDir() {
				return filepath.Join(resolved, codexConfigFileName), nil
			}
		}
	}

	return filepath.Join(home, ".codex", codexConfigFileName), nil
}

func (a *codexClientRegistrationAdapter) clientID() string {
	return codexClientID
}

func (a *codexClientRegistrationAdapter) inspect(ctx context.Context) (clientRegistrationSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationSnapshot{}, err
	}
	if !a.codexAvailable() {
		return clientRegistrationSnapshot{
			status: codexUnobservedStatus(),
			config: clientRegistrationConfig{},
		}, nil
	}

	config, err := a.readConfig()
	if err != nil {
		return clientRegistrationSnapshot{}, err
	}
	status, err := a.statusForConfig(config)
	if err != nil {
		return clientRegistrationSnapshot{}, err
	}
	return clientRegistrationSnapshot{status: status, config: config}, nil
}

func (a *codexClientRegistrationAdapter) plan(
	ctx context.Context,
	spec MCPRegistrationSpec,
	current clientRegistrationConfig,
) (clientRegistrationMutation, error) {
	if err := ctx.Err(); err != nil {
		return clientRegistrationMutation{}, err
	}
	if err := a.validateSpec(spec); err != nil {
		return clientRegistrationMutation{}, err
	}
	if !a.codexAvailable() {
		return clientRegistrationMutation{}, errClientRegistrationClientUnavailable
	}
	if err := validateCodexConfigSize(current); err != nil {
		return clientRegistrationMutation{}, err
	}

	entry := codexConfigEntry{values: map[string]string{}}
	if current.exists {
		var err error
		entry, err = parseCodexConfig(current.bytes, codexManagedServerName)
		if err != nil {
			return clientRegistrationMutation{}, err
		}
	}
	if entry.exists {
		if codexEntryMatches(entry, a.appExecutable) {
			return clientRegistrationMutation{
				config:  cloneCodexConfig(current),
				changes: []string{mcpRegistrationChangeUnchanged},
			}, nil
		}
		return clientRegistrationMutation{}, errClientRegistrationNameConflict
	}

	replacement, err := appendCodexManagedEntry(current, a.appExecutable)
	if err != nil {
		return clientRegistrationMutation{}, err
	}
	if err := validateCodexConfigSize(replacement); err != nil {
		return clientRegistrationMutation{}, err
	}
	return clientRegistrationMutation{
		config:  replacement,
		changes: []string{mcpRegistrationChangeAdded},
	}, nil
}

func (a *codexClientRegistrationAdapter) writeConfig(
	ctx context.Context,
	expected clientRegistrationConfig,
	replacement clientRegistrationConfig,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !a.codexAvailable() {
		return errClientRegistrationClientUnavailable
	}
	if err := validateCodexConfigSize(expected); err != nil {
		return err
	}
	if err := validateCodexConfigSize(replacement); err != nil {
		return err
	}

	current, err := a.readConfig()
	if err != nil {
		return err
	}
	if !sameCodexConfig(current, expected) {
		return errClientRegistrationStaleSettings
	}
	if sameCodexConfig(current, replacement) {
		return nil
	}
	if !replacement.exists {
		if !current.exists {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		latest, err := a.readConfig()
		if err != nil {
			return err
		}
		if !sameCodexConfig(latest, expected) {
			return errClientRegistrationStaleSettings
		}
		if err := os.Remove(a.configPath); err != nil {
			return errCodexSettingsWrite
		}
		return nil
	}

	return a.atomicReplace(ctx, expected, replacement)
}

func (a *codexClientRegistrationAdapter) verify(ctx context.Context) (MCPClientStatus, error) {
	snapshot, err := a.inspect(ctx)
	if err != nil {
		return MCPClientStatus{}, err
	}
	return snapshot.status, nil
}

func (a *codexClientRegistrationAdapter) validateSpec(spec MCPRegistrationSpec) error {
	if spec.ClientID != codexClientID || spec.DisplayName != codexManagedServerName {
		return errClientRegistrationInvalidSpec
	}
	if spec.Transport != MCPTransportStdio {
		return errClientRegistrationUnsupportedTransport
	}
	if spec.Endpoint != "" || spec.Command != a.appExecutable || !filepath.IsAbs(spec.Command) {
		return errClientRegistrationInvalidSpec
	}
	if len(spec.Args) != 1 || spec.Args[0] != "--mcp" {
		return errClientRegistrationInvalidSpec
	}
	if len(spec.Scope) != 1 || spec.Scope[0] != "user" {
		return errClientRegistrationInvalidSpec
	}
	return nil
}

func (a *codexClientRegistrationAdapter) statusForConfig(config clientRegistrationConfig) (MCPClientStatus, error) {
	status := MCPClientStatus{
		ClientID:       codexClientID,
		Installed:      mcpClientInstalled,
		Registration:   mcpClientRegistrationNotRegistered,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         mcpClientBackupNotCreated,
		EvidenceSource: mcpClientEvidenceConfigObserved,
	}
	if err := validateCodexConfigSize(config); err != nil {
		return MCPClientStatus{}, err
	}
	if !config.exists {
		return status, nil
	}
	entry, err := parseCodexConfig(config.bytes, codexManagedServerName)
	if err != nil {
		return MCPClientStatus{}, err
	}
	if !entry.exists {
		return status, nil
	}
	if codexEntryMatches(entry, a.appExecutable) {
		status.Registration = "registered"
		return status, nil
	}
	status.Registration = "conflict"
	return status, nil
}

func codexUnobservedStatus() MCPClientStatus {
	return MCPClientStatus{
		ClientID:       codexClientID,
		Installed:      mcpClientInstalledNotObserved,
		Registration:   mcpClientRegistrationNotObserved,
		Connection:     mcpClientConnectionUnverified,
		ToolCall:       mcpClientConnectionUnverified,
		Backup:         mcpClientBackupNotCreated,
		EvidenceSource: mcpClientEvidenceNotObserved,
	}
}

func (a *codexClientRegistrationAdapter) codexAvailable() bool {
	if a.codexExecutable == "" {
		return false
	}
	info, err := os.Stat(a.codexExecutable)
	return err == nil && info.Mode().IsRegular()
}

func (a *codexClientRegistrationAdapter) readConfig() (clientRegistrationConfig, error) {
	info, err := os.Lstat(a.configPath)
	if os.IsNotExist(err) {
		return clientRegistrationConfig{}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return clientRegistrationConfig{}, errCodexSettingsUnreadable
	}

	file, err := os.Open(a.configPath)
	if err != nil {
		return clientRegistrationConfig{}, errCodexSettingsUnreadable
	}
	defer file.Close()

	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return clientRegistrationConfig{}, errCodexSettingsUnreadable
	}
	if openedInfo.Size() > codexMaxConfigBytes {
		return clientRegistrationConfig{}, errClientRegistrationConfigTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, codexMaxConfigBytes+1))
	if err != nil {
		return clientRegistrationConfig{}, errCodexSettingsUnreadable
	}
	if len(data) > codexMaxConfigBytes {
		return clientRegistrationConfig{}, errClientRegistrationConfigTooLarge
	}
	return clientRegistrationConfig{exists: true, bytes: data}, nil
}

func (a *codexClientRegistrationAdapter) atomicReplace(
	ctx context.Context,
	expected clientRegistrationConfig,
	replacement clientRegistrationConfig,
) error {
	dir := filepath.Dir(a.configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errCodexSettingsWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	mode := os.FileMode(0o600)
	if expected.exists {
		if info, err := os.Stat(a.configPath); err == nil {
			mode = info.Mode().Perm()
		}
	}
	tmp, err := os.CreateTemp(dir, ".lah-codex-*.tmp")
	if err != nil {
		return errCodexSettingsWrite
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return errCodexSettingsWrite
	}
	if _, err := tmp.Write(replacement.bytes); err != nil {
		tmp.Close()
		return errCodexSettingsWrite
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errCodexSettingsWrite
	}
	if err := tmp.Close(); err != nil {
		return errCodexSettingsWrite
	}

	latest, err := a.readConfig()
	if err != nil {
		return err
	}
	if !sameCodexConfig(latest, expected) {
		return errClientRegistrationStaleSettings
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, a.configPath); err != nil {
		return errCodexSettingsWrite
	}
	return nil
}

func validateCodexConfigSize(config clientRegistrationConfig) error {
	if len(config.bytes) > codexMaxConfigBytes {
		return errClientRegistrationConfigTooLarge
	}
	if !config.exists && len(config.bytes) != 0 {
		return errCodexSettingsUnreadable
	}
	return nil
}

func sameCodexConfig(left, right clientRegistrationConfig) bool {
	return left.exists == right.exists && bytes.Equal(left.bytes, right.bytes)
}

func cloneCodexConfig(config clientRegistrationConfig) clientRegistrationConfig {
	return clientRegistrationConfig{
		exists: config.exists,
		bytes:  append([]byte(nil), config.bytes...),
	}
}

func appendCodexManagedEntry(current clientRegistrationConfig, appExecutable string) (clientRegistrationConfig, error) {
	command, err := quoteTOMLBasicString(appExecutable)
	if err != nil {
		return clientRegistrationConfig{}, errClientRegistrationInvalidSpec
	}
	newline := "\n"
	if bytes.Contains(current.bytes, []byte("\r\n")) {
		newline = "\r\n"
	}

	var result bytes.Buffer
	result.Grow(len(current.bytes) + len(command) + 100)
	result.Write(current.bytes)
	if len(current.bytes) > 0 {
		if !bytes.HasSuffix(current.bytes, []byte("\n")) {
			result.WriteString(newline)
		}
		if !bytes.HasSuffix(current.bytes, []byte(newline+newline)) {
			result.WriteString(newline)
		}
	}
	result.WriteString("[mcp_servers.")
	result.WriteString(codexManagedServerName)
	result.WriteString("]")
	result.WriteString(newline)
	result.WriteString("command = ")
	result.WriteString(command)
	result.WriteString(newline)
	result.WriteString("args = [\"--mcp\"]")
	result.WriteString(newline)
	return clientRegistrationConfig{exists: true, bytes: result.Bytes()}, nil
}

func quoteTOMLBasicString(value string) (string, error) {
	if !utf8.ValidString(value) {
		return "", errCodexSettingsUnreadable
	}
	var quoted strings.Builder
	quoted.Grow(len(value) + 2)
	quoted.WriteByte('"')
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return "", errCodexSettingsUnreadable
		}
		switch char {
		case '\\', '"':
			quoted.WriteByte('\\')
			quoted.WriteRune(char)
		default:
			quoted.WriteRune(char)
		}
	}
	quoted.WriteByte('"')
	return quoted.String(), nil
}

func parseCodexConfig(data []byte, managedName string) (codexConfigEntry, error) {
	entry := codexConfigEntry{values: map[string]string{}}
	if len(data) > codexMaxConfigBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return codexConfigEntry{}, errCodexSettingsUnreadable
	}
	for index, char := range data {
		if char == '\r' && (index+1 == len(data) || data[index+1] != '\n') {
			return codexConfigEntry{}, errCodexSettingsUnreadable
		}
	}
	statements, err := codexTOMLStatements(data)
	if err != nil {
		return codexConfigEntry{}, errCodexSettingsUnreadable
	}

	var activeTable []string
	var arrayScopes []codexTOMLArrayScope
	targetAssignments := false
	seenTables := map[string]bool{}
	arrayOccurrences := map[string]int{}
	seenAssignments := map[string]*codexTOMLAssignmentNode{}
	for _, statement := range statements {
		trimmed := strings.TrimSpace(statement)
		if strings.HasPrefix(trimmed, "[") {
			table, array, err := parseCodexTOMLHeader(trimmed)
			if err != nil {
				return codexConfigEntry{}, errCodexSettingsUnreadable
			}
			activeTable = table
			arrayScopes = updateCodexTOMLArrayScopes(arrayScopes, table, array, arrayOccurrences)
			if !array {
				key := codexTOMLScopedTableKey(arrayScopes, table)
				if seenTables[key] {
					return codexConfigEntry{}, errCodexSettingsUnreadable
				}
				seenTables[key] = true
			}
			if isCodexManagedTable(table, managedName) {
				if entry.exists {
					return codexConfigEntry{}, errCodexSettingsUnreadable
				}
				entry.exists = true
				entry.array = array
				targetAssignments = true
			} else if isCodexManagedDescendant(table, managedName) {
				entry.exists = true
				entry.nested = true
				targetAssignments = false
			} else {
				targetAssignments = false
			}
			continue
		}

		keyText, valueText, err := splitCodexTOMLAssignment(trimmed)
		if err != nil {
			return codexConfigEntry{}, errCodexSettingsUnreadable
		}
		keys, err := parseCodexTOMLKeyPath(keyText)
		if err != nil {
			return codexConfigEntry{}, errCodexSettingsUnreadable
		}
		if !validCodexTOMLValue(valueText) {
			return codexConfigEntry{}, errCodexSettingsUnreadable
		}
		scopeKey := codexTOMLScopedTableKey(arrayScopes, activeTable)
		if !addCodexTOMLAssignment(seenAssignments, scopeKey, keys) {
			return codexConfigEntry{}, errCodexSettingsUnreadable
		}
		if len(activeTable) == 0 && len(keys) >= 2 && keys[0] == "mcp_servers" && keys[1] == managedName {
			entry.exists = true
			entry.extra = true
			continue
		}
		if len(activeTable) == 0 && len(keys) == 1 && keys[0] == "mcp_servers" {
			return codexConfigEntry{}, errCodexSettingsUnreadable
		}
		if len(activeTable) == 1 && activeTable[0] == "mcp_servers" && keys[0] == managedName {
			entry.exists = true
			entry.extra = true
			continue
		}
		if !targetAssignments || !isCodexManagedTable(activeTable, managedName) {
			continue
		}
		if len(keys) != 1 {
			entry.extra = true
			continue
		}
		key := keys[0]
		entry.values[key] = stripCodexTOMLComments(valueText)
	}
	return entry, nil
}

func codexEntryMatches(entry codexConfigEntry, appExecutable string) bool {
	if !entry.exists || entry.array || entry.nested || entry.extra || len(entry.values) != 2 {
		return false
	}
	commandText, hasCommand := entry.values["command"]
	argsText, hasArgs := entry.values["args"]
	if !hasCommand || !hasArgs {
		return false
	}
	command, commandOK := parseCodexTOMLString(strings.TrimSpace(commandText))
	args, argsOK := parseCodexTOMLStringArray(strings.TrimSpace(argsText))
	return commandOK && argsOK && command == appExecutable && len(args) == 1 && args[0] == "--mcp"
}

func isCodexManagedTable(table []string, managedName string) bool {
	return len(table) == 2 && table[0] == "mcp_servers" && table[1] == managedName
}

func isCodexManagedDescendant(table []string, managedName string) bool {
	return len(table) > 2 && table[0] == "mcp_servers" && table[1] == managedName
}

func updateCodexTOMLArrayScopes(
	current []codexTOMLArrayScope,
	table []string,
	array bool,
	occurrences map[string]int,
) []codexTOMLArrayScope {
	updated := make([]codexTOMLArrayScope, 0, len(current)+1)
	for _, scope := range current {
		isPrefix := codexTOMLPathStartsWith(table, scope.path)
		isStrictPrefix := isPrefix && len(scope.path) < len(table)
		if (!array && isPrefix) || (array && isStrictPrefix) {
			updated = append(updated, scope)
		}
	}
	if array {
		key := codexTOMLPathKey(table)
		occurrences[key]++
		updated = append(updated, codexTOMLArrayScope{
			path:       append([]string(nil), table...),
			occurrence: occurrences[key],
		})
	}
	return updated
}

func codexTOMLPathStartsWith(path, prefix []string) bool {
	if len(prefix) > len(path) {
		return false
	}
	for index, part := range prefix {
		if path[index] != part {
			return false
		}
	}
	return true
}

func codexTOMLPathKey(path []string) string {
	var key strings.Builder
	codexTOMLWritePath(&key, path)
	return key.String()
}

func codexTOMLWritePath(destination *strings.Builder, path []string) {
	for _, part := range path {
		destination.WriteString(strconv.Itoa(len(part)))
		destination.WriteByte(':')
		destination.WriteString(part)
		destination.WriteByte(';')
	}
}

func codexTOMLScopedTableKey(scopes []codexTOMLArrayScope, table []string) string {
	var key strings.Builder
	for _, scope := range scopes {
		if !codexTOMLPathStartsWith(table, scope.path) {
			continue
		}
		key.WriteString("array:")
		codexTOMLWritePath(&key, scope.path)
		key.WriteByte('#')
		key.WriteString(strconv.Itoa(scope.occurrence))
		key.WriteByte('|')
	}
	key.WriteString("table:")
	codexTOMLWritePath(&key, table)
	return key.String()
}

func addCodexTOMLAssignment(
	seen map[string]*codexTOMLAssignmentNode,
	scope string,
	keys []string,
) bool {
	root := seen[scope]
	if root == nil {
		root = &codexTOMLAssignmentNode{children: map[string]*codexTOMLAssignmentNode{}}
		seen[scope] = root
	}
	node := root
	for _, key := range keys {
		if node.assigned {
			return false
		}
		child := node.children[key]
		if child == nil {
			child = &codexTOMLAssignmentNode{children: map[string]*codexTOMLAssignmentNode{}}
			node.children[key] = child
		}
		node = child
	}
	if node.assigned || len(node.children) > 0 {
		return false
	}
	node.assigned = true
	return true
}

func codexTOMLStatements(data []byte) ([]string, error) {
	lines := strings.SplitAfter(string(data), "\n")
	statements := make([]string, 0, len(lines))
	var pending strings.Builder
	for _, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\n")
		line = strings.TrimSuffix(line, "\r")
		if pending.Len() == 0 {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.HasPrefix(trimmed, "[") {
				clean := stripCodexTOMLComments(line)
				if _, _, err := parseCodexTOMLHeader(strings.TrimSpace(clean)); err != nil {
					return nil, err
				}
				statements = append(statements, clean)
				continue
			}
		}

		pending.WriteString(line)
		pending.WriteByte('\n')
		statement := pending.String()
		valueStart, err := codexTOMLValueStart(statement)
		if err != nil {
			return nil, err
		}
		complete, err := codexTOMLValueComplete(statement[valueStart:])
		if err != nil {
			return nil, err
		}
		if complete {
			statements = append(statements, strings.TrimSpace(statement))
			pending.Reset()
		}
	}
	if pending.Len() != 0 {
		return nil, errCodexSettingsUnreadable
	}
	return statements, nil
}

func codexTOMLValueStart(statement string) (int, error) {
	quoted := byte(0)
	escaped := false
	for index := 0; index < len(statement); index++ {
		char := statement[index]
		if quoted != 0 {
			if quoted == '"' && escaped {
				escaped = false
				continue
			}
			if quoted == '"' && char == '\\' {
				escaped = true
				continue
			}
			if char == quoted {
				quoted = 0
			}
			continue
		}
		if char == '"' || char == '\'' {
			quoted = char
			continue
		}
		if char == '#' || char == '\n' {
			return 0, errCodexSettingsUnreadable
		}
		if char == '=' {
			return index + 1, nil
		}
	}
	return 0, errCodexSettingsUnreadable
}

func codexTOMLValueComplete(value string) (bool, error) {
	var quote byte
	triple := false
	var squareDepth int
	var braceDepth int
	for index := 0; index < len(value); index++ {
		char := value[index]
		if quote != 0 {
			if quote == '"' && char == '\\' {
				if index+1 >= len(value) {
					return false, nil
				}
				next := value[index+1]
				if next == '\n' && triple {
					index++
					continue
				}
				escapeLength := codexTOMLEscapeLength(value[index:])
				if escapeLength == 0 {
					return false, errCodexSettingsUnreadable
				}
				index += escapeLength - 1
				continue
			}
			if triple {
				if index+2 < len(value) && value[index] == quote && value[index+1] == quote && value[index+2] == quote {
					quote = 0
					triple = false
					index += 2
				}
				continue
			}
			if char == '\n' {
				return false, errCodexSettingsUnreadable
			}
			if char == quote {
				quote = 0
			}
			continue
		}

		switch char {
		case '"', '\'':
			if index+2 < len(value) && value[index+1] == char && value[index+2] == char {
				quote = char
				triple = true
				index += 2
			} else {
				quote = char
			}
		case '#':
			for index < len(value) && value[index] != '\n' {
				index++
			}
			if squareDepth == 0 && braceDepth == 0 {
				return true, nil
			}
		case '[':
			squareDepth++
		case ']':
			squareDepth--
			if squareDepth < 0 {
				return false, errCodexSettingsUnreadable
			}
		case '{':
			braceDepth++
		case '}':
			braceDepth--
			if braceDepth < 0 {
				return false, errCodexSettingsUnreadable
			}
		case '\n':
			if squareDepth == 0 && braceDepth == 0 {
				if strings.TrimSpace(value[index+1:]) != "" {
					return false, errCodexSettingsUnreadable
				}
				return true, nil
			}
		}
	}
	if quote != 0 {
		return false, nil
	}
	if squareDepth != 0 || braceDepth != 0 {
		return false, nil
	}
	return strings.TrimSpace(value) != "", nil
}

func codexTOMLEscapeLength(value string) int {
	if len(value) < 2 || value[0] != '\\' {
		return 0
	}
	switch value[1] {
	case 'b', 't', 'n', 'f', 'r', '"', '\\':
		return 2
	case 'u':
		if len(value) >= 6 && isCodexHex(value[2:6]) {
			return 6
		}
	default:
		if value[1] == 'U' && len(value) >= 10 && isCodexHex(value[2:10]) {
			return 10
		}
	}
	return 0
}

func parseCodexTOMLHeader(statement string) ([]string, bool, error) {
	statement = strings.TrimSpace(stripCodexTOMLComments(statement))
	array := strings.HasPrefix(statement, "[[")
	openCount := 1
	closeCount := 1
	if array {
		openCount = 2
		closeCount = 2
	}
	tooShort := len(statement) < openCount+closeCount
	wrongOpen := !strings.HasPrefix(statement, strings.Repeat("[", openCount))
	wrongClose := !strings.HasSuffix(statement, strings.Repeat("]", closeCount))
	if tooShort || wrongOpen || wrongClose {
		return nil, false, errCodexSettingsUnreadable
	}
	body := strings.TrimSpace(statement[openCount : len(statement)-closeCount])
	keys, err := parseCodexTOMLKeyPath(body)
	if err != nil || len(keys) == 0 {
		return nil, false, errCodexSettingsUnreadable
	}
	return keys, array, nil
}

func splitCodexTOMLAssignment(statement string) (string, string, error) {
	equal := -1
	var quote byte
	escaped := false
	for index := 0; index < len(statement); index++ {
		char := statement[index]
		if quote != 0 {
			if quote == '"' && escaped {
				escaped = false
				continue
			}
			if quote == '"' && char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
			}
			continue
		}
		if char == '"' || char == '\'' {
			quote = char
			continue
		}
		if char == '#' {
			break
		}
		if char == '=' {
			equal = index
			break
		}
	}
	if equal <= 0 {
		return "", "", errCodexSettingsUnreadable
	}
	key := strings.TrimSpace(statement[:equal])
	value := stripCodexTOMLComments(statement[equal+1:])
	if key == "" || strings.TrimSpace(value) == "" {
		return "", "", errCodexSettingsUnreadable
	}
	return key, strings.TrimSpace(value), nil
}

func parseCodexTOMLKeyPath(text string) ([]string, error) {
	keys := make([]string, 0, 3)
	for index := 0; index < len(text); {
		for index < len(text) && (text[index] == ' ' || text[index] == '\t') {
			index++
		}
		if index >= len(text) {
			return nil, errCodexSettingsUnreadable
		}
		var key string
		if text[index] == '"' || text[index] == '\'' {
			quote := text[index]
			start := index
			index++
			escaped := false
			for index < len(text) {
				char := text[index]
				if quote == '"' && char == '\\' && !escaped {
					escaped = true
					index++
					continue
				}
				if char == quote && !escaped {
					index++
					break
				}
				escaped = false
				index++
			}
			if index > len(text) || text[index-1] != quote {
				return nil, errCodexSettingsUnreadable
			}
			key, _ = parseCodexTOMLString(text[start:index])
			if key == "" && text[start:index] != `""` && text[start:index] != `''` {
				return nil, errCodexSettingsUnreadable
			}
		} else {
			start := index
			for index < len(text) {
				char := text[index]
				isUpper := char >= 'A' && char <= 'Z'
				isLower := char >= 'a' && char <= 'z'
				isDigit := char >= '0' && char <= '9'
				if isUpper || isLower || isDigit || char == '_' || char == '-' {
					index++
					continue
				}
				break
			}
			if start == index {
				return nil, errCodexSettingsUnreadable
			}
			key = text[start:index]
		}
		keys = append(keys, key)
		for index < len(text) && (text[index] == ' ' || text[index] == '\t') {
			index++
		}
		if index == len(text) {
			break
		}
		if text[index] != '.' {
			return nil, errCodexSettingsUnreadable
		}
		index++
		if index == len(text) {
			return nil, errCodexSettingsUnreadable
		}
	}
	return keys, nil
}

func parseCodexTOMLString(value string) (string, bool) {
	if len(value) < 2 {
		return "", false
	}
	if value[0] == '"' && value[len(value)-1] == '"' && !strings.HasPrefix(value, `"""`) {
		if !validCodexTOMLStringEscapes(value[1 : len(value)-1]) {
			return "", false
		}
		decoded, err := strconv.Unquote(value)
		return decoded, err == nil
	}
	if value[0] == '\'' && value[len(value)-1] == '\'' && !strings.HasPrefix(value, "'''") {
		if strings.Contains(value[1:len(value)-1], "'") || strings.ContainsAny(value[1:len(value)-1], "\r\n") {
			return "", false
		}
		return value[1 : len(value)-1], true
	}
	return "", false
}

func validCodexTOMLStringEscapes(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			continue
		}
		if index+1 >= len(value) {
			return false
		}
		switch value[index+1] {
		case 'b', 't', 'n', 'f', 'r', '"', '\\':
			index++
		case 'u':
			if index+5 >= len(value) || !isCodexHex(value[index+2:index+6]) {
				return false
			}
			index += 5
		case 'U':
			if index+9 >= len(value) || !isCodexHex(value[index+2:index+10]) {
				return false
			}
			index += 9
		default:
			return false
		}
	}
	return true
}

func isCodexHex(value string) bool {
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') && (char < 'A' || char > 'F') {
			return false
		}
	}
	return true
}

func validCodexTOMLValue(value string) bool {
	parser := codexTOMLValueParser{value: value}
	if !parser.parseValue(0) {
		return false
	}
	parser.skipSpace()
	return parser.index == len(parser.value)
}

type codexTOMLValueParser struct {
	value string
	index int
}

func (p *codexTOMLValueParser) parseValue(depth int) bool {
	if depth > 64 {
		return false
	}
	p.skipSpace()
	if p.index >= len(p.value) {
		return false
	}

	switch p.value[p.index] {
	case '[', '{':
		return p.parseCollection(depth + 1)
	case '"', '\'':
		return p.parseString()
	default:
		return p.parseScalar()
	}
}

func (p *codexTOMLValueParser) parseCollection(depth int) bool {
	opening := p.value[p.index]
	closing := byte(']')
	var inlineAssignments map[string]*codexTOMLAssignmentNode
	if opening == '{' {
		closing = '}'
		inlineAssignments = map[string]*codexTOMLAssignmentNode{}
	}
	p.index++
	p.skipSpace()
	if p.index < len(p.value) && p.value[p.index] == closing {
		p.index++
		return true
	}

	for {
		if opening == '[' {
			if !p.parseValue(depth) {
				return false
			}
		} else if !p.parseInlineTableEntry(depth, inlineAssignments) {
			return false
		}
		p.skipSpace()
		if p.index >= len(p.value) {
			return false
		}
		if p.value[p.index] == closing {
			p.index++
			return true
		}
		if p.value[p.index] != ',' {
			return false
		}
		p.index++
		p.skipSpace()
		if p.index < len(p.value) && p.value[p.index] == closing {
			if opening == '{' {
				return false
			}
			p.index++
			return true
		}
	}
}

func (p *codexTOMLValueParser) parseInlineTableEntry(
	depth int,
	seen map[string]*codexTOMLAssignmentNode,
) bool {
	start := p.index
	var quote byte
	escaped := false
	for p.index < len(p.value) {
		char := p.value[p.index]
		if quote != 0 {
			if quote == '"' && char == '\\' && !escaped {
				escaped = true
				p.index++
				continue
			}
			if char == quote && !escaped {
				quote = 0
			}
			escaped = false
			p.index++
			continue
		}
		if char == '"' || char == '\'' {
			quote = char
			p.index++
			continue
		}
		if char == '=' {
			break
		}
		if char == ',' || char == '}' {
			return false
		}
		p.index++
	}
	if p.index >= len(p.value) || quote != 0 {
		return false
	}
	keyText := strings.TrimSpace(p.value[start:p.index])
	keys, err := parseCodexTOMLKeyPath(keyText)
	if err != nil || !addCodexTOMLAssignment(seen, "inline", keys) {
		return false
	}
	p.index++
	return p.parseValue(depth)
}

func (p *codexTOMLValueParser) parseString() bool {
	quote := p.value[p.index]
	triple := p.index+2 < len(p.value) && p.value[p.index+1] == quote && p.value[p.index+2] == quote
	if triple {
		p.index += 3
	} else {
		p.index++
	}
	start := p.index
	for p.index < len(p.value) {
		char := p.value[p.index]
		if quote == '"' && char == '\\' {
			escapeLength := codexTOMLEscapeLength(p.value[p.index:])
			if escapeLength == 0 {
				return false
			}
			p.index += escapeLength
			continue
		}
		if !triple && (char == '\n' || char == '\r') {
			return false
		}
		if char == quote {
			if triple {
				if p.index+2 < len(p.value) && p.value[p.index+1] == quote && p.value[p.index+2] == quote {
					contents := p.value[start:p.index]
					p.index += 3
					if quote == '"' && !validCodexTOMLStringEscapes(contents) {
						return false
					}
					return true
				}
			} else {
				contents := p.value[start:p.index]
				p.index++
				if quote == '"' {
					return validCodexTOMLStringEscapes(contents)
				}
				return !strings.Contains(contents, "'")
			}
		}
		p.index++
	}
	return false
}

func (p *codexTOMLValueParser) parseScalar() bool {
	start := p.index
	for p.index < len(p.value) {
		char := p.value[p.index]
		if char == ',' || char == ']' || char == '}' || char == ' ' || char == '\t' || char == '\n' || char == '\r' {
			break
		}
		p.index++
	}
	return p.index > start && codexTOMLScalarPattern.MatchString(p.value[start:p.index])
}

func (p *codexTOMLValueParser) skipSpace() {
	for p.index < len(p.value) {
		char := p.value[p.index]
		if char != ' ' && char != '\t' && char != '\n' && char != '\r' {
			return
		}
		p.index++
	}
}

func parseCodexTOMLStringArray(value string) ([]string, bool) {
	if len(value) < 2 || value[0] != '[' || value[len(value)-1] != ']' {
		return nil, false
	}
	contents := strings.TrimSpace(value[1 : len(value)-1])
	if contents == "" {
		return []string{}, true
	}
	values := make([]string, 0, 2)
	for index := 0; index < len(contents); {
		for index < len(contents) && isCodexTOMLSpace(contents[index]) {
			index++
		}
		if index >= len(contents) {
			break
		}
		start := index
		quote := contents[index]
		if quote != '"' && quote != '\'' {
			return nil, false
		}
		index++
		escaped := false
		for index < len(contents) {
			char := contents[index]
			if quote == '"' && char == '\\' && !escaped {
				escaped = true
				index++
				continue
			}
			if char == quote && !escaped {
				index++
				break
			}
			escaped = false
			index++
		}
		item, ok := parseCodexTOMLString(contents[start:index])
		if !ok {
			return nil, false
		}
		values = append(values, item)
		for index < len(contents) && isCodexTOMLSpace(contents[index]) {
			index++
		}
		if index == len(contents) {
			break
		}
		if contents[index] != ',' {
			return nil, false
		}
		index++
		if strings.TrimSpace(contents[index:]) == "" {
			break
		}
	}
	return values, true
}

func stripCodexTOMLComments(text string) string {
	var result strings.Builder
	result.Grow(len(text))
	var quote byte
	triple := false
	escaped := false
	for index := 0; index < len(text); index++ {
		char := text[index]
		if quote != 0 {
			result.WriteByte(char)
			if quote == '"' && char == '\\' && !escaped {
				escaped = true
				continue
			}
			if char == quote && !escaped {
				if triple {
					if index+2 < len(text) && text[index+1] == quote && text[index+2] == quote {
						result.WriteByte(quote)
						result.WriteByte(quote)
						index += 2
						quote = 0
						triple = false
					}
				} else {
					quote = 0
				}
			}
			escaped = false
			continue
		}
		if char == '"' || char == '\'' {
			quote = char
			if index+2 < len(text) && text[index+1] == char && text[index+2] == char {
				triple = true
				result.WriteByte(char)
				result.WriteByte(char)
				index += 2
			}
			result.WriteByte(char)
			continue
		}
		if char == '#' {
			for index < len(text) && text[index] != '\n' {
				index++
			}
			if index < len(text) {
				result.WriteByte('\n')
			}
			continue
		}
		result.WriteByte(char)
	}
	return result.String()
}

func isCodexTOMLSpace(char byte) bool {
	return char == ' ' || char == '\t' || char == '\n' || char == '\r'
}
