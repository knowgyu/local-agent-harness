package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

const firstReadDefinitionDigestPrefix = "sha256:"

const (
	firstReadStateObserved    = "observed"
	firstReadStateNotObserved = "not_observed"
	firstReadStateUnavailable = "unavailable"
	firstReadRecordTimeout    = 2 * time.Second
)

var errFirstReadReceiptUnavailable = errors.New("first read receipt is unavailable")

// firstReadDefinitionSelector carries names only while checking whether a
// captured receipt digest still matches an active definition. Selectors are
// never persisted or used to create an observed receipt.
type firstReadDefinitionSelector struct {
	TargetName        string
	ServiceBundleName string
	EnvironmentName   string
}

type firstReadCallObserver struct {
	service string
	tool    string

	mu     sync.Mutex
	digest string
}

type firstReadCallObserverContextKey struct{}

type firstReadDashboardBindingDefinition struct {
	Target        dashboardTarget `json:"target"`
	ServiceBundle string          `json:"service_bundle"`
	Environment   string          `json:"environment"`
	Namespace     string          `json:"namespace"`
	Deployment    string          `json:"deployment"`
}

// firstReadReadinessDTO contains only app-observed evidence. User-reported
// confirmation is kept in its existing separate browser-local state.
type firstReadReadinessDTO struct {
	State      string     `json:"state"`
	Source     string     `json:"source,omitempty"`
	Service    string     `json:"service,omitempty"`
	Tool       string     `json:"tool,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

func withFirstReadCallObserver(ctx context.Context, service, tool string) (context.Context, *firstReadCallObserver) {
	if ctx == nil {
		ctx = context.Background()
	}
	observer := &firstReadCallObserver{service: service, tool: tool}
	if !validFirstReadServiceTool(service, tool) {
		return ctx, observer
	}
	return context.WithValue(ctx, firstReadCallObserverContextKey{}, observer), observer
}

// observeFirstReadDefinition captures only a digest of the exact loaded target
// supplied by a registered-read method. It is a no-op outside an MCP read call.
func observeFirstReadDefinition(ctx context.Context, definition any) {
	observer := firstReadCallObserverFromContext(ctx)
	if observer == nil {
		return
	}

	digest := ""
	switch observer.service {
	case "github":
		if target, ok := definition.(target); ok && validateTarget(target) == nil && !target.Disabled {
			digest = digestFirstReadDefinition(target)
		}
	case "jenkins":
		if target, ok := definition.(jenkinsTarget); ok && validateJenkinsTarget(target) == nil && !target.Disabled {
			digest = digestFirstReadDefinition(target)
		}
	case "harbor":
		if target, ok := definition.(harborTarget); ok && validateHarborTarget(target) == nil && !target.Disabled {
			digest = digestFirstReadDefinition(target)
		}
	case "dashboard":
		if observer.tool == "dashboard_namespaces" {
			if target, ok := definition.(dashboardTarget); ok && validateDashboardTarget(target) == nil && !target.Disabled {
				digest = digestFirstReadDefinition(target)
			}
		}
	}
	observer.setDigest(digest)
}

// observeFirstReadDashboardBinding captures the Dashboard target together with
// the selected service-bundle environment mapping for mapped reads.
func observeFirstReadDashboardBinding(ctx context.Context, binding dashboardServiceBinding) {
	observer := firstReadCallObserverFromContext(ctx)
	if observer == nil || observer.service != "dashboard" || observer.tool == "dashboard_namespaces" ||
		!validBundleName(binding.ServiceBundle) || !validBundleName(binding.Environment) ||
		!validServiceDashboardNamespace(binding.Namespace) || !validServiceDashboardDeployment(binding.Deployment) ||
		binding.Target.Disabled || validateDashboardTarget(binding.Target) != nil {
		return
	}
	observer.setDigest(digestFirstReadDefinition(firstReadDashboardBindingDefinition{
		Target: binding.Target, ServiceBundle: binding.ServiceBundle, Environment: binding.Environment,
		Namespace: binding.Namespace, Deployment: binding.Deployment,
	}))
}

func (o *firstReadCallObserver) Digest() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.digest
}

func (a *app) recordCapturedMCPRead(ctx context.Context, observer *firstReadCallObserver) {
	if observer == nil {
		return
	}
	_ = a.recordObservedMCPRead(ctx, observer.service, observer.tool, observer.Digest())
}

func (o *firstReadCallObserver) setDigest(digest string) {
	if o == nil || !validFirstReadDefinitionDigest(digest) {
		return
	}
	o.mu.Lock()
	if o.digest == "" {
		o.digest = digest
	}
	o.mu.Unlock()
}

func firstReadCallObserverFromContext(ctx context.Context) *firstReadCallObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(firstReadCallObserverContextKey{}).(*firstReadCallObserver)
	return observer
}

func firstReadServiceForTool(tool string) (string, bool) {
	switch tool {
	case "github_repository", "github_pull_request":
		return "github", true
	case "jenkins_registered_job", "jenkins_registered_queue_item", "jenkins_registered_build_log":
		return "jenkins", true
	case "harbor_repository_artifacts", "harbor_project_quota":
		return "harbor", true
	case "dashboard_namespaces", "dashboard_deployment_status", "dashboard_deployment_diagnosis",
		"dashboard_deployment_events", "dashboard_deployment_pods", "dashboard_deployment_pod_logs":
		return "dashboard", true
	default:
		return "", false
	}
}

func validFirstReadServiceTool(service, tool string) bool {
	toolService, ok := firstReadServiceForTool(tool)
	return ok && service != "" && toolService == service
}

func (a *app) recordObservedMCPRead(_ context.Context, service, tool, definitionDigest string) error {
	if a == nil || !validFirstReadServiceTool(service, tool) || !validFirstReadDefinitionDigest(definitionDigest) {
		return errOnboardingInvalid
	}
	controller, err := a.ensureOnboardingController()
	if err != nil || controller == nil {
		return errFirstReadReceiptUnavailable
	}
	writeContext, cancel := context.WithTimeout(context.Background(), firstReadRecordTimeout)
	defer cancel()
	_, err = controller.RecordFirstRead(writeContext, onboardingFirstReadReceipt{
		Source: onboardingFirstReadSource, Service: onboardingServiceKind(service), Tool: tool,
		ObservedAt: time.Now().UTC(), DefinitionDigest: definitionDigest,
	})
	if err != nil {
		return errFirstReadReceiptUnavailable
	}
	return nil
}

func firstReadReadinessFromConfig(cfg config, receipts []onboardingFirstReadReceipt) firstReadReadinessDTO {
	result := firstReadReadinessDTO{State: firstReadStateNotObserved}
	var earliest *onboardingFirstReadReceipt
	activeDigests := make(map[string]map[string]struct{})
	for index := range receipts {
		receipt := &receipts[index]
		service := string(receipt.Service)
		if receipt.Source != onboardingFirstReadSource || !validFirstReadServiceTool(service, receipt.Tool) ||
			!validFirstReadDefinitionDigest(receipt.DefinitionDigest) || !validFirstReadReceiptTime(receipt.ObservedAt) {
			continue
		}
		key := service + "\x00" + receipt.Tool
		current, exists := activeDigests[key]
		if !exists {
			current = firstReadDefinitionDigestSet(cfg, service, receipt.Tool)
			activeDigests[key] = current
		}
		if _, active := current[receipt.DefinitionDigest]; !active {
			continue
		}
		if earliest == nil || receipt.ObservedAt.Before(earliest.ObservedAt) ||
			receipt.ObservedAt.Equal(earliest.ObservedAt) && (service < string(earliest.Service) ||
				service == string(earliest.Service) && receipt.Tool < earliest.Tool) {
			earliest = receipt
		}
	}
	if earliest == nil {
		return result
	}
	observedAt := earliest.ObservedAt.UTC()
	return firstReadReadinessDTO{
		State: firstReadStateObserved, Source: onboardingFirstReadSource,
		Service: string(earliest.Service), Tool: earliest.Tool, ObservedAt: &observedAt,
	}
}

func validFirstReadReceiptTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC && !value.After(time.Now().UTC())
}

// firstReadDefinitionDigestForCall resolves one active configuration entry
// for readiness matching. Actual call receipts use the immutable loaded
// definition captured by firstReadCallObserver before the network request.
// Encoded settings are hashed in memory and discarded.
func firstReadDefinitionDigestForCall(
	cfg config,
	service, tool string,
	selector firstReadDefinitionSelector,
) string {
	if !validFirstReadServiceTool(service, tool) {
		return ""
	}

	var definition any
	switch service {
	case "github":
		for _, target := range cfg.GitHubTargets {
			if target.Name == selector.TargetName && !target.Disabled && validateTarget(target) == nil {
				definition = target
				break
			}
		}
	case "jenkins":
		for _, target := range cfg.JenkinsTargets {
			if target.Name == selector.TargetName && !target.Disabled && validateJenkinsTarget(target) == nil {
				definition = target
				break
			}
		}
	case "harbor":
		for _, target := range cfg.HarborTargets {
			if target.Name == selector.TargetName && !target.Disabled && validateHarborTarget(target) == nil {
				definition = target
				break
			}
		}
	case "dashboard":
		if tool == "dashboard_namespaces" {
			for _, target := range cfg.DashboardTargets {
				if target.Name == selector.TargetName && !target.Disabled && validateDashboardTarget(target) == nil {
					definition = target
					break
				}
			}
		} else {
			definition = dashboardFirstReadDefinition(cfg, selector)
		}
	}
	return digestFirstReadDefinition(definition)
}

func dashboardFirstReadDefinition(cfg config, selector firstReadDefinitionSelector) any {
	for _, bundle := range cfg.ServiceBundles {
		if bundle.Name != selector.ServiceBundleName {
			continue
		}
		for _, environment := range bundle.Environments {
			if environment.Name != selector.EnvironmentName || environment.DashboardTargetID == "" ||
				environment.DashboardNamespace == "" || environment.DashboardDeployment == "" {
				continue
			}
			for _, target := range cfg.DashboardTargets {
				if target.ID != environment.DashboardTargetID || target.Disabled || validateDashboardTarget(target) != nil {
					continue
				}
				return firstReadDashboardBindingDefinition{
					Target: target, ServiceBundle: bundle.Name, Environment: environment.Name,
					Namespace: environment.DashboardNamespace, Deployment: environment.DashboardDeployment,
				}
			}
		}
	}
	return nil
}

func digestFirstReadDefinition(definition any) string {
	if definition == nil {
		return ""
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return firstReadDefinitionDigestPrefix + hex.EncodeToString(digest[:])
}

// firstReadDefinitionDigestIsActive prevents an old observation from being
// presented as current after the saved target or mapped resource definition
// changes. The receipt itself contains only this digest, never the definition.
func firstReadDefinitionDigestIsActive(
	cfg config,
	service, tool, digest string,
) bool {
	if !validFirstReadServiceTool(service, tool) || !validFirstReadDefinitionDigest(digest) {
		return false
	}
	_, active := firstReadDefinitionDigestSet(cfg, service, tool)[digest]
	return active
}

func firstReadDefinitionDigestSet(cfg config, service, tool string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, digest := range firstReadActiveDefinitionDigests(cfg, service, tool) {
		set[digest] = struct{}{}
	}
	return set
}

func firstReadActiveDefinitionDigests(cfg config, service, tool string) []string {
	if !validFirstReadServiceTool(service, tool) {
		return nil
	}
	digests := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(selector firstReadDefinitionSelector) {
		digest := firstReadDefinitionDigestForCall(cfg, service, tool, selector)
		if digest == "" {
			return
		}
		if _, exists := seen[digest]; exists {
			return
		}
		seen[digest] = struct{}{}
		digests = append(digests, digest)
	}
	switch service {
	case "github":
		for _, target := range cfg.GitHubTargets {
			add(firstReadDefinitionSelector{TargetName: target.Name})
		}
	case "jenkins":
		for _, target := range cfg.JenkinsTargets {
			add(firstReadDefinitionSelector{TargetName: target.Name})
		}
	case "harbor":
		for _, target := range cfg.HarborTargets {
			add(firstReadDefinitionSelector{TargetName: target.Name})
		}
	case "dashboard":
		if tool == "dashboard_namespaces" {
			for _, target := range cfg.DashboardTargets {
				add(firstReadDefinitionSelector{TargetName: target.Name})
			}
		} else {
			for _, bundle := range cfg.ServiceBundles {
				for _, environment := range bundle.Environments {
					add(firstReadDefinitionSelector{
						ServiceBundleName: bundle.Name,
						EnvironmentName:   environment.Name,
					})
				}
			}
		}
	}
	return digests
}

func validFirstReadDefinitionDigest(value string) bool {
	if len(value) != len(firstReadDefinitionDigestPrefix)+sha256.Size*2 ||
		!strings.HasPrefix(value, firstReadDefinitionDigestPrefix) {
		return false
	}
	encoded := strings.TrimPrefix(value, firstReadDefinitionDigestPrefix)
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == encoded
}
