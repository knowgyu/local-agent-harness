package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSSHTargetCount          = 64
	maxSSHTargetOperations     = 64
	maxSSHOperationParameters  = 32
	maxSSHOperationFixedArgs   = 64
	maxSSHOperationOutputBytes = 64 << 10
	maxSSHStreamOutputBytes    = maxSSHOperationOutputBytes / 2
	sshOperationTimeout        = 30 * time.Second
)

var (
	sshParameterNamePattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	sshParameterIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	sshNumericStringPattern       = regexp.MustCompile(`^[0-9]+$`)
	sshProgramTokenPattern        = regexp.MustCompile(`^[A-Za-z0-9_./:+-]{1,256}$`)
	sshArgumentTokenPattern       = regexp.MustCompile(`^[A-Za-z0-9_./:=+-]{1,256}$`)
	sshPlaceholderPattern         = regexp.MustCompile(`\{\{([a-z][a-z0-9_]{0,31})\}\}`)
	sshSensitiveParamWords        = []string{"token", "password", "secret", "credential", "authorization", "privatekey", "apikey"}
	sshEndpointParamWords         = []string{"host", "hostname", "url", "uri", "endpoint", "server", "address", "destination", "origin", "remote", "target", "port", "path", "filepath", "directory", "command", "cmd", "shell", "program", "executable", "exec", "ip"}
)

type sshPreparedInvocation struct {
	command       string
	parameterMap  map[string]string
	approvalItems []sshApprovalParameter
	scope         string
}

func (c *sshOperationController) RunSSHOperation(ctx context.Context, input SSHOperationInput) (SSHOperationResult, error) {
	if err := checkSSHContext(ctx); err != nil {
		return sshOperationResult("cancelled", SSHRemoteOutcomeNotNeeded, "The request ended before SSH started."), nil
	}
	snapshot, err := c.loadSnapshot(ctx)
	if err != nil {
		return sshOperationResult("unavailable", SSHRemoteOutcomeNotNeeded, "SSH operation settings could not be read."), nil
	}
	targetIndex := findSSHTargetIndex(snapshot.Targets, input.TargetID)
	if targetIndex < 0 {
		return sshOperationResult("rejected", SSHRemoteOutcomeNotNeeded, "Choose a registered SSH target and operation."), nil
	}
	target := snapshot.Targets[targetIndex]
	var operation SSHOperationDefinition
	for _, candidate := range target.Operations {
		if candidate.ID == input.OperationID {
			operation = cloneSSHOperation(candidate)
			break
		}
	}
	if operation.ID == "" {
		return sshOperationResult("rejected", SSHRemoteOutcomeNotNeeded, "Choose a registered SSH target and operation."), nil
	}
	if !isStoredSSHOperationValid(operation, target.ID) {
		return sshOperationResult("rejected", SSHRemoteOutcomeNotNeeded, "The saved SSH operation is stale or invalid. Review it in the local UI."), nil
	}
	prepared, err := prepareSSHInvocation(target.ID, target.Alias, operation, input.Parameters)
	if err != nil {
		return sshOperationResult("rejected", SSHRemoteOutcomeNotNeeded, "The supplied parameters do not match the registered operation."), nil
	}
	if !sshApprovalAllows(operation, prepared.scope) {
		return sshOperationResult("rejected", SSHRemoteOutcomeNotNeeded, "This exact SSH operation scope is not approved."), nil
	}
	if operation.Approval.Mode == OperationApprovalPerCall {
		if c.approver == nil {
			return sshOperationResult("approval_unavailable", SSHRemoteOutcomeNotNeeded, "The local approval page is unavailable. No SSH command was started."), nil
		}
		approved, err := c.approver.Confirm(ctx, sshApprovalRequest{
			TargetName: target.Name,
			Operation:  operation.Name,
			Summary:    operation.Summary,
			Parameters: prepared.approvalItems,
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return sshOperationResult("cancelled", SSHRemoteOutcomeNotNeeded, "Approval ended before SSH started."), nil
			}
			return sshOperationResult("approval_unavailable", SSHRemoteOutcomeNotNeeded, "The local approval page did not complete. No SSH command was started."), nil
		}
		if !approved {
			return sshOperationResult("denied", SSHRemoteOutcomeNotNeeded, "The SSH operation was not approved."), nil
		}
	}
	if c == nil || c.store == nil || c.runner == nil {
		return sshOperationResult("unavailable", SSHRemoteOutcomeNotNeeded, "SSH execution is unavailable. No command was started."), nil
	}
	runCtx, cancel := context.WithTimeout(ctx, sshOperationTimeout)
	defer cancel()
	var process sshRunningProcess
	err = withConfigLock(func() error {
		if err := checkSSHContext(runCtx); err != nil {
			return err
		}
		current, err := c.store.Snapshot()
		if err != nil {
			return errSSHConfigurationUnavailable
		}
		currentTargetIndex := findSSHTargetIndex(current.Targets, target.ID)
		if currentTargetIndex < 0 {
			return errSSHConfigurationStale
		}
		currentTarget := current.Targets[currentTargetIndex]
		if currentTarget.Name != target.Name || currentTarget.Alias != target.Alias {
			return errSSHConfigurationStale
		}
		currentOperation, found := findSSHOperation(currentTarget, operation.ID)
		if !found || currentOperation.Revision != operation.Revision || !isStoredSSHOperationValid(currentOperation, currentTarget.ID) {
			return errSSHConfigurationStale
		}
		process, err = c.runner.Start(runCtx, currentTarget.Alias, prepared.command)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil || process == nil {
		if errors.Is(err, errSSHConfigurationStale) {
			return sshOperationResult("stale", SSHRemoteOutcomeNotNeeded, "The registered target or operation changed after review. Review it again before running."), nil
		}
		if runCtx.Err() != nil {
			return sshOperationResult("cancelled", SSHRemoteOutcomeNotNeeded, "SSH did not start before the request ended."), nil
		}
		return sshOperationResult("not_started", SSHRemoteOutcomeNotNeeded, "The local OpenSSH client could not be started. Check its installation and the registered alias."), nil
	}
	processResult := process.Wait()
	if processResult.TimedOut || processResult.Canceled || runCtx.Err() != nil || !processResult.ExitKnown {
		result := sshOperationResult("unknown", SSHRemoteOutcomeUnknown, "The local SSH process ended before the remote result was confirmed. The remote command may still be running; do not automatically retry a change.")
		result.OutputTruncated = processResult.OutputTruncated
		return result, nil
	}
	if processResult.ExitCode == 255 {
		result := sshOperationResult("unknown", SSHRemoteOutcomeUnknown, "OpenSSH reported an error and the remote result is unknown. Check host trust and authentication locally before deciding whether to retry.")
		result.OutputTruncated = processResult.OutputTruncated
		return result, nil
	}
	output, truncated, safe := sanitizeSSHOperationOutput(processResult)
	if !safe {
		result := sshOperationResult("output_unavailable", SSHRemoteOutcomeConfirmed, "The remote command completed, but its output failed safety checks and was withheld.")
		result.OutputTruncated = processResult.OutputTruncated
		return result, nil
	}
	status := "succeeded"
	if processResult.ExitCode != 0 {
		status = "remote_failed"
	}
	return SSHOperationResult{
		Status:          status,
		Output:          output,
		OutputTruncated: truncated || processResult.OutputTruncated,
		RemoteOutcome:   SSHRemoteOutcomeConfirmed,
	}, nil
}

func normalizeSSHOperationDefinition(operation SSHOperationDefinition, targetID string) (SSHOperationDefinition, error) {
	if !validSSHDefinitionID(targetID) || operation.ID == "" {
		return SSHOperationDefinition{}, errSSHDefinitionInvalid
	}
	if !validSSHDefinitionID(operation.ID) || !validSSHDisplayText(operation.Name, 80) || !validSSHDisplayText(operation.Summary, 500) ||
		!sshProgramTokenPattern.MatchString(operation.Program) || len(operation.FixedArgs) > maxSSHOperationFixedArgs || len(operation.Parameters) > maxSSHOperationParameters {
		return SSHOperationDefinition{}, errSSHDefinitionInvalid
	}
	result := cloneSSHOperation(operation)
	parameters := make(map[string]SSHParameterSpec, len(result.Parameters))
	for _, parameter := range result.Parameters {
		if !sshParameterNamePattern.MatchString(parameter.Name) || isSensitiveSSHParameterName(parameter.Name) || isEndpointSSHParameterName(parameter.Name) ||
			(parameter.Type != "string" && parameter.Type != "integer" && parameter.Type != "boolean") ||
			(parameter.Description != "" && !validSSHDisplayText(parameter.Description, 200)) {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
		if _, exists := parameters[parameter.Name]; exists {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
		parameters[parameter.Name] = parameter
	}
	used := make(map[string]bool)
	for _, argument := range result.FixedArgs {
		if len(argument) == 0 || len(argument) > 256 || strings.ContainsAny(argument, "\x00\r\n\t ") {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
		placeholders := sshPlaceholderPattern.FindAllStringSubmatch(argument, -1)
		residual := sshPlaceholderPattern.ReplaceAllString(argument, "x")
		if strings.Contains(residual, "{{") || strings.Contains(residual, "}}") || !sshArgumentTokenPattern.MatchString(residual) {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
		for _, placeholder := range placeholders {
			parameter, exists := parameters[placeholder[1]]
			if !exists || (!parameter.Required && argument != "{{"+parameter.Name+"}}") {
				return SSHOperationDefinition{}, errSSHDefinitionInvalid
			}
			used[parameter.Name] = true
		}
	}
	for _, parameter := range result.Parameters {
		if !used[parameter.Name] {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
	}
	// Read-only operations run without a confirmation step. They therefore
	// cannot accept model-selected values that could be interpreted by the
	// saved remote command as a host, path, option, or other destination.
	if result.Risk == SSHRiskReadOnly && len(result.Parameters) != 0 {
		return SSHOperationDefinition{}, errSSHDefinitionInvalid
	}
	if result.Approval.Mode == "" {
		return SSHOperationDefinition{}, errSSHDefinitionInvalid
	}
	switch result.Risk {
	case SSHRiskReadOnly:
		if result.Approval.Mode != OperationApprovalReadOnly || len(result.Approval.Scope) != 0 {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
		result.Approval.Revision = ""
	case SSHRiskStateChanging:
		switch result.Approval.Mode {
		case OperationApprovalPerCall:
			if len(result.Approval.Scope) != 0 {
				return SSHOperationDefinition{}, errSSHDefinitionInvalid
			}
			result.Approval.Revision = ""
		case OperationApprovalPreapproved:
			if !validSSHApprovalScopes(result.Approval.Scope) {
				return SSHOperationDefinition{}, errSSHDefinitionInvalid
			}
		default:
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
	case SSHRiskDestructive:
		if result.Approval.Mode != OperationApprovalPerCall || len(result.Approval.Scope) != 0 {
			return SSHOperationDefinition{}, errSSHDefinitionInvalid
		}
		result.Approval.Revision = ""
	default:
		return SSHOperationDefinition{}, errSSHDefinitionInvalid
	}
	result.Revision = sshOperationRevision(result)
	if result.Approval.Mode == OperationApprovalPreapproved {
		result.Approval.Revision = result.Revision
	}
	return result, nil
}

func isStoredSSHOperationValid(operation SSHOperationDefinition, targetID string) bool {
	revision := operation.Revision
	approvalRevision := operation.Approval.Revision
	normalized, err := normalizeSSHOperationDefinition(operation, targetID)
	if err != nil || revision == "" || normalized.Revision != revision {
		return false
	}
	if normalized.Approval.Mode == OperationApprovalPreapproved && approvalRevision != revision {
		return false
	}
	if normalized.Approval.Mode != OperationApprovalPreapproved && approvalRevision != "" {
		return false
	}
	return true
}

func sshOperationRevision(operation SSHOperationDefinition) string {
	copy := cloneSSHOperation(operation)
	copy.Revision = ""
	copy.Approval.Revision = ""
	sort.Strings(copy.Approval.Scope)
	encoded, err := json.Marshal(copy)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func validSSHApprovalScopes(scopes []string) bool {
	if len(scopes) == 0 || len(scopes) > maxSSHTargetOperations {
		return false
	}
	seen := make(map[string]struct{}, len(scopes))
	for _, scope := range scopes {
		if len(scope) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(scope, "sha256:") {
			return false
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(scope, "sha256:")); err != nil {
			return false
		}
		if _, exists := seen[scope]; exists {
			return false
		}
		seen[scope] = struct{}{}
	}
	return true
}

func prepareSSHInvocation(targetID, targetAlias string, operation SSHOperationDefinition, input map[string]json.RawMessage) (sshPreparedInvocation, error) {
	if len(input) > len(operation.Parameters) {
		return sshPreparedInvocation{}, errSSHOperationRejected
	}
	specs := make(map[string]SSHParameterSpec, len(operation.Parameters))
	values := make(map[string]string, len(operation.Parameters))
	items := make([]sshApprovalParameter, 0, len(operation.Parameters))
	for _, parameter := range operation.Parameters {
		specs[parameter.Name] = parameter
		raw, present := input[parameter.Name]
		if !present {
			if parameter.Required {
				return sshPreparedInvocation{}, errSSHOperationRejected
			}
			continue
		}
		value, err := parseSSHParameter(parameter, raw)
		if err != nil {
			return sshPreparedInvocation{}, errSSHOperationRejected
		}
		values[parameter.Name] = value
		items = append(items, sshApprovalParameter{Name: parameter.Name, Value: cleanOutput(value, "", 128)})
	}
	for name := range input {
		if _, exists := specs[name]; !exists {
			return sshPreparedInvocation{}, errSSHOperationRejected
		}
	}

	arguments := make([]string, 0, len(operation.FixedArgs))
	for _, argument := range operation.FixedArgs {
		matches := sshPlaceholderPattern.FindAllStringSubmatch(argument, -1)
		if len(matches) == 1 && argument == matches[0][0] {
			if value, exists := values[matches[0][1]]; exists {
				arguments = append(arguments, value)
			}
			continue
		}
		resolved := argument
		for _, match := range matches {
			value, exists := values[match[1]]
			if !exists {
				return sshPreparedInvocation{}, errSSHOperationRejected
			}
			resolved = strings.ReplaceAll(resolved, match[0], value)
		}
		if !sshArgumentTokenPattern.MatchString(resolved) {
			return sshPreparedInvocation{}, errSSHOperationRejected
		}
		arguments = append(arguments, resolved)
	}
	commandParts := make([]string, 0, len(arguments)+1)
	commandParts = append(commandParts, operation.Program)
	commandParts = append(commandParts, arguments...)
	command := strings.Join(commandParts, " ")
	return sshPreparedInvocation{
		command:       command,
		parameterMap:  values,
		approvalItems: items,
		scope:         sshApprovalScopeFingerprint(targetID, targetAlias, operation.ID, values),
	}, nil
}

func parseSSHParameter(spec SSHParameterSpec, raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errSSHOperationRejected
	}
	switch spec.Type {
	case "string":
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || !sshParameterIdentifierPattern.MatchString(value) || sshNumericStringPattern.MatchString(value) || looksLikeSSHEndpointValue(value) || looksLikeSSHCredential(value) {
			return "", errSSHOperationRejected
		}
		return value, nil
	case "integer":
		value := strings.TrimSpace(string(raw))
		parsed, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return "", errSSHOperationRejected
		}
		return strconv.FormatUint(parsed, 10), nil
	case "boolean":
		var value bool
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", errSSHOperationRejected
		}
		return strconv.FormatBool(value), nil
	default:
		return "", errSSHOperationRejected
	}
}

func looksLikeSSHCredential(value string) bool {
	return privateKeyPEM.MatchString(value) || githubTokenPattern.MatchString(value) || credentialHeader.MatchString(value)
}

func isSensitiveSSHParameterName(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", ""), "-", ""))
	for _, word := range sshSensitiveParamWords {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func isEndpointSSHParameterName(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(name, "_", ""), "-", ""))
	for _, word := range sshEndpointParamWords {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

func looksLikeSSHEndpointValue(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "localhost") {
		return true
	}
	return strings.ContainsAny(value, ".:/\\@?#%[]")
}

func sshApprovalScopeFingerprint(targetID, targetAlias, operationID string, parameters map[string]string) string {
	names := make([]string, 0, len(parameters))
	for name := range parameters {
		names = append(names, name)
	}
	sort.Strings(names)
	type parameterPair struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	pairs := make([]parameterPair, 0, len(names))
	for _, name := range names {
		pairs = append(pairs, parameterPair{Name: name, Value: parameters[name]})
	}
	encoded, err := json.Marshal(struct {
		TargetID    string          `json:"target_id"`
		TargetAlias string          `json:"target_alias"`
		OperationID string          `json:"operation_id"`
		Parameters  []parameterPair `json:"parameters"`
	}{TargetID: targetID, TargetAlias: targetAlias, OperationID: operationID, Parameters: pairs})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func sshApprovalAllows(operation SSHOperationDefinition, scope string) bool {
	switch operation.Risk {
	case SSHRiskReadOnly:
		return operation.Approval.Mode == OperationApprovalReadOnly
	case SSHRiskDestructive:
		return operation.Approval.Mode == OperationApprovalPerCall
	case SSHRiskStateChanging:
		switch operation.Approval.Mode {
		case OperationApprovalPerCall:
			return true
		case OperationApprovalPreapproved:
			if operation.Approval.Revision != operation.Revision || !validSSHApprovalScopes(operation.Approval.Scope) {
				return false
			}
			for _, approvedScope := range operation.Approval.Scope {
				if approvedScope == scope {
					return true
				}
			}
		}
	}
	return false
}

func checkSSHContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("The SSH operation request was canceled.")
	}
	if ctx.Err() != nil {
		return errors.New("The SSH operation request was canceled.")
	}
	return nil
}

func findSSHOperation(target sshTargetDefinition, operationID string) (SSHOperationDefinition, bool) {
	for _, operation := range target.Operations {
		if operation.ID == operationID {
			return cloneSSHOperation(operation), true
		}
	}
	return SSHOperationDefinition{}, false
}

func sshOperationResult(status string, outcome SSHRemoteOutcome, nextAction string) SSHOperationResult {
	return SSHOperationResult{Status: status, RemoteOutcome: outcome, NextAction: nextAction}
}

func sanitizeSSHOperationOutput(process sshProcessResult) (string, bool, bool) {
	stdout, stdoutTruncated, stdoutOK := sanitizeSSHOutputStream(process.Stdout, process.StdoutTruncated)
	stderr, stderrTruncated, stderrOK := sanitizeSSHOutputStream(process.Stderr, process.StderrTruncated)
	if !stdoutOK || !stderrOK {
		return "", stdoutTruncated || stderrTruncated, false
	}
	parts := make([]string, 0, 2)
	if stdout != "" {
		parts = append(parts, "stdout:\n"+stdout)
	}
	if stderr != "" {
		parts = append(parts, "stderr:\n"+stderr)
	}
	combined := strings.Join(parts, "\n")
	if len(combined) > maxSSHOperationOutputBytes {
		combined = limitOutputBytes(combined, maxSSHOperationOutputBytes)
		return combined, true, true
	}
	return combined, stdoutTruncated || stderrTruncated, true
}

func sanitizeSSHOutputStream(raw []byte, truncated bool) (string, bool, bool) {
	if len(raw) == 0 {
		return "", truncated, true
	}
	if len(raw) > maxSSHStreamOutputBytes || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return "", truncated, false
	}
	value := string(raw)
	cleaned := cleanOutputWithReplacement(value, "", maxSSHOperationOutputBytes, true, "[REDACTED]")
	return limitOutputBytes(cleaned, maxSSHOperationOutputBytes), truncated, true
}
