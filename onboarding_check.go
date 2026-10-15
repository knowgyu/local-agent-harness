package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	onboardingCheckTimeout = serviceRequestTimeout
	onboardingCheckMaxBody = maxAPIBytes
)

var errOnboardingCheckUnavailable = errors.New("registration connection check is unavailable")

// onboardingCheckController performs the explicit read for a saved draft. It
// has no app dependency so tests and the aggregate onboarding controller can
// inject the HTTP client, secret store, and durable draft store.
type onboardingCheckController struct {
	store      onboardingDraftStore
	configPath string
	secrets    secretStore
	client     *http.Client
	proofs     onboardingCheckProofRegistry
	now        func() time.Time
}

func newOnboardingCheckController(
	store onboardingDraftStore,
	configPath string,
	secrets secretStore,
	client *http.Client,
	proofs onboardingCheckProofRegistry,
) *onboardingCheckController {
	return &onboardingCheckController{
		store: store, configPath: configPath, secrets: secrets, client: client,
		proofs: proofs, now: time.Now,
	}
}

// CheckDraft validates a current saved draft, makes exactly one bounded GET,
// stores a secret-free classified receipt, and returns a process-local permit
// only for a successful read. The permit binds the later activation request to
// these exact candidate, policy, config, source, named-secret metadata, and
// credential bytes.
func (c *onboardingCheckController) CheckDraft(ctx context.Context, req onboardingCheckRequest) (onboardingCheckResult, error) {
	if req.Credential != nil {
		defer clear(req.Credential)
	}
	if ctx == nil || c == nil || c.store == nil || c.configPath == "" || c.client == nil || c.proofs == nil ||
		!onboardingDraftIDPattern.MatchString(req.DraftID) || req.ExpectedRevision == 0 {
		return onboardingCheckResult{}, errOnboardingInvalid
	}
	if ctx.Err() != nil {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	draft, err := c.store.GetDraft(ctx, req.DraftID)
	if err != nil {
		if errors.Is(err, errOnboardingDraftNotFound) {
			return onboardingCheckResult{}, errOnboardingDraftNotFound
		}
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	if draft.ID != req.DraftID || draft.Revision != req.ExpectedRevision {
		return onboardingCheckResult{}, errOnboardingDraftStale
	}
	if validateOnboardingCandidate(draft.Service, draft.Fields) != nil {
		return onboardingCheckResult{}, errOnboardingInvalid
	}
	checkGeneration, err := c.store.StartCheck(ctx, req.DraftID, req.ExpectedRevision)
	if err != nil {
		switch {
		case errors.Is(err, errOnboardingDraftNotFound):
			return onboardingCheckResult{}, errOnboardingDraftNotFound
		case errors.Is(err, errOnboardingDraftStale):
			return onboardingCheckResult{}, errOnboardingDraftStale
		case ctx.Err() != nil:
			return onboardingCheckResult{}, errOnboardingUnavailable
		case errors.Is(err, errOnboardingActivationRecoveryRequired):
			return onboardingCheckResult{}, errOnboardingActivationRecoveryRequired
		default:
			return onboardingCheckResult{}, errOnboardingUnavailable
		}
	}
	if checkGeneration == 0 {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	if err := c.proofs.BeginDraftCheck(req.DraftID, checkGeneration); err != nil {
		if errors.Is(err, errOnboardingDraftStale) {
			return onboardingCheckResult{}, errOnboardingDraftStale
		}
		return onboardingCheckResult{}, errOnboardingCheckUnavailable
	}
	draft, err = c.store.GetDraft(ctx, req.DraftID)
	if err != nil {
		if errors.Is(err, errOnboardingDraftNotFound) {
			return onboardingCheckResult{}, errOnboardingDraftNotFound
		}
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	if draft.ID != req.DraftID || draft.Revision != req.ExpectedRevision || draft.CheckGeneration != checkGeneration {
		return onboardingCheckResult{}, errOnboardingDraftStale
	}
	if validateOnboardingCandidate(draft.Service, draft.Fields) != nil {
		return onboardingCheckResult{}, errOnboardingInvalid
	}
	if (len(req.Credential) == 0) == (req.NamedSecretID == "") {
		return onboardingCheckResult{}, errOnboardingInvalid
	}

	configRevision, metadata, credential, err := c.loadCheckSnapshot(ctx, req)
	if err != nil {
		return onboardingCheckResult{}, err
	}
	defer clear(credential)
	if ctx.Err() != nil {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	if !validOnboardingCredential(draft.Service, credential) {
		return onboardingCheckResult{}, errOnboardingInvalid
	}

	candidateDigest, err := onboardingCandidateDigest(draft.Service, draft.Fields)
	if err != nil {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	policyDigest := onboardingPolicyDigest(draft.Service, draft.Fields)
	if !validOnboardingDigest(policyDigest, "policy:sha256:") {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}

	checkCtx, cancel := context.WithTimeout(ctx, onboardingCheckTimeout)
	failureKind := checkOnboardingCandidate(checkCtx, c.client, draft.Service, draft.Fields, credential)
	cancel()

	if ctx.Err() != nil {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	currentConfigRevision, err := c.readConfigRevision(ctx)
	if err != nil {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	if currentConfigRevision != configRevision {
		return onboardingCheckResult{}, errOnboardingReviewChanged
	}

	checkedAt := c.clock().UTC()
	receipt := onboardingCheckReceipt{
		DraftID: req.DraftID, DraftRevision: draft.Revision,
		CheckGeneration: checkGeneration,
		CandidateDigest: candidateDigest, PolicyDigest: policyDigest,
		ConfigRevision: configRevision, CheckedAt: checkedAt,
		Outcome: onboardingCheckSucceeded,
	}
	if failureKind != "" {
		receipt.Outcome = onboardingCheckFailed
		receipt.FailureKind = failureKind
	}
	if err := c.store.SaveCheckReceipt(ctx, receipt); err != nil {
		if ctx.Err() != nil || errors.Is(err, errOnboardingDraftStale) {
			return onboardingCheckResult{}, errOnboardingDraftStale
		}
		if errors.Is(err, errOnboardingCheckRequired) {
			return onboardingCheckResult{}, errOnboardingCheckRequired
		}
		return onboardingCheckResult{}, errOnboardingUnavailable
	}

	result := onboardingCheckResult{Receipt: receipt}
	if receipt.Outcome == onboardingCheckSucceeded {
		source := onboardingCredentialTemporary
		namedSecretID := ""
		metadataDigest := ""
		if req.NamedSecretID != "" {
			source = onboardingCredentialNamed
			namedSecretID = metadata.ID
			metadataDigest, err = onboardingSecretMetadataDigest(metadata)
			if err != nil {
				return onboardingCheckResult{}, errOnboardingUnavailable
			}
		}
		result.Permit, err = c.proofs.Issue(onboardingCheckProofInput{
			DraftID: req.DraftID, DraftRevision: draft.Revision,
			CheckGeneration: checkGeneration,
			CandidateDigest: candidateDigest, PolicyDigest: policyDigest,
			ConfigRevision: configRevision, Credential: credential,
			CredentialSource: source, NamedSecretID: namedSecretID,
			SecretMetadataDigest: metadataDigest,
		}, checkedAt)
		if err != nil || result.Permit == "" {
			return onboardingCheckResult{}, errOnboardingCheckUnavailable
		}
	}
	return result, nil
}

func (c *onboardingCheckController) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *onboardingCheckController) loadCheckSnapshot(ctx context.Context, req onboardingCheckRequest) (string, namedSecretMetadata, []byte, error) {
	var cfg config
	var revision string
	var metadata namedSecretMetadata
	var credential []byte
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errOnboardingUnavailable
		}
		loaded, err := readConfig(c.configPath)
		if err != nil {
			return errOnboardingUnavailable
		}
		cfg = loaded
		fullRevision, err := setupDraftConfigRevision(cfg)
		if err != nil {
			return errOnboardingUnavailable
		}
		if !validOnboardingDigest(fullRevision, "config:sha256:") {
			return errOnboardingUnavailable
		}
		revision = fullRevision
		if req.NamedSecretID == "" {
			credential = req.Credential
			return nil
		}
		if !namedSecretIDPattern.MatchString(req.NamedSecretID) || c.secrets == nil {
			return errOnboardingInvalid
		}
		index := namedSecretIndex(cfg.NamedSecrets, req.NamedSecretID)
		if index < 0 {
			return errOnboardingInvalid
		}
		metadata = cfg.NamedSecrets[index]
		value, loadErr := c.secrets.Load(metadata.CredentialRef)
		if loadErr != nil || len(value) == 0 || len(value) > maxSecretSize {
			clear(value)
			return errOnboardingInvalid
		}
		credential = value
		return nil
	})
	if err != nil {
		clear(credential)
		if errors.Is(err, errOnboardingInvalid) {
			return "", namedSecretMetadata{}, nil, errOnboardingInvalid
		}
		return "", namedSecretMetadata{}, nil, errOnboardingUnavailable
	}
	return revision, metadata, credential, nil
}

func (c *onboardingCheckController) readConfigRevision(ctx context.Context) (string, error) {
	var revision string
	err := withConfigLock(func() error {
		if ctx.Err() != nil {
			return errOnboardingUnavailable
		}
		cfg, err := readConfig(c.configPath)
		if err != nil {
			return errOnboardingUnavailable
		}
		fullRevision, err := setupDraftConfigRevision(cfg)
		if err != nil || !validOnboardingDigest(fullRevision, "config:sha256:") {
			return errOnboardingUnavailable
		}
		revision = fullRevision
		return nil
	})
	if err != nil {
		return "", errOnboardingUnavailable
	}
	return revision, nil
}

func onboardingCandidateDigest(service onboardingServiceKind, fields onboardingDraftFields) (string, error) {
	encoded, err := json.Marshal(struct {
		Domain  string                `json:"domain"`
		Service onboardingServiceKind `json:"service"`
		Fields  onboardingDraftFields `json:"fields"`
	}{Domain: "local-agent-harness/onboarding-candidate/v1", Service: service, Fields: fields})
	if err != nil {
		return "", errOnboardingUnavailable
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func onboardingSecretMetadataDigest(metadata namedSecretMetadata) (string, error) {
	if !namedSecretIDPattern.MatchString(metadata.ID) || !secretRefPattern.MatchString(metadata.CredentialRef) {
		return "", errOnboardingInvalid
	}
	encoded, err := json.Marshal(struct {
		Domain        string `json:"domain"`
		ID            string `json:"id"`
		Name          string `json:"name"`
		Purpose       string `json:"purpose"`
		CredentialRef string `json:"credential_ref"`
	}{
		Domain: "local-agent-harness/onboarding-named-secret-metadata/v1",
		ID:     metadata.ID, Name: metadata.Name, Purpose: metadata.Purpose,
		CredentialRef: metadata.CredentialRef,
	})
	if err != nil {
		return "", errOnboardingUnavailable
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func validateOnboardingCandidate(service onboardingServiceKind, fields onboardingDraftFields) error {
	if fields.Name == "" || strings.TrimSpace(fields.Name) != fields.Name || len(fields.Name) > 80 || strings.ContainsAny(fields.Name, "\r\n\x00") {
		return errOnboardingInvalid
	}
	switch service {
	case onboardingServiceGitHub:
		origin, err := validateOrigin(fields.Origin)
		if err != nil || origin != fields.Origin || !validRepository(fields.Repository) {
			return errOnboardingInvalid
		}
	case onboardingServiceJenkins:
		baseURL, err := validateJenkinsBaseURL(fields.BaseURL)
		if err != nil || baseURL != fields.BaseURL || !validJenkinsUsername(fields.Username) || !validJenkinsJobPath(fields.JobPath) || !validJenkinsEnvironmentLabel(fields.Environment) {
			return errOnboardingInvalid
		}
	case onboardingServiceDashboard:
		baseURL, err := validateJenkinsBaseURL(fields.BaseURL)
		if err != nil || baseURL != fields.BaseURL {
			return errOnboardingInvalid
		}
	default:
		return errOnboardingInvalid
	}
	return nil
}

func validOnboardingCredential(service onboardingServiceKind, credential []byte) bool {
	if len(credential) == 0 || len(credential) > maxSecretSize || strings.ContainsAny(string(credential), "\r\n\x00") {
		return false
	}
	if service == onboardingServiceDashboard && strings.TrimSpace(string(credential)) != string(credential) {
		return false
	}
	return true
}

func validOnboardingDigest(value, prefix string) bool {
	if prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return false
		}
		value = strings.TrimPrefix(value, prefix)
	}
	if len(value) != sha256.Size*2 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func checkOnboardingCandidate(ctx context.Context, client *http.Client, service onboardingServiceKind, fields onboardingDraftFields, credential []byte) connectionFailureKind {
	if ctx == nil || client == nil || !validOnboardingCredential(service, credential) {
		return connectionFailureOther
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	var err error
	switch service {
	case onboardingServiceGitHub:
		err = checkOnboardingGitHub(ctx, &clientCopy, fields, string(credential))
	case onboardingServiceJenkins:
		err = checkOnboardingJenkins(ctx, &clientCopy, fields, string(credential))
	case onboardingServiceDashboard:
		err = checkOnboardingDashboard(ctx, &clientCopy, fields, string(credential))
	default:
		err = errOnboardingInvalid
	}
	return connectionFailureFor(err)
}

func checkOnboardingGitHub(ctx context.Context, client *http.Client, fields onboardingDraftFields, credential string) error {
	endpoint, err := onboardingGitHubRepositoryURL(fields)
	if err != nil {
		return errOnboardingInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errOnboardingInvalid
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("User-Agent", "local-agent-harness")
	resp, err := client.Do(req)
	if err != nil {
		return connectionTransportFailure(err, "Could not reach the registered GitHub API.")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return withConnectionDiagnostic(connectionFailureEndpoint, errors.New("GitHub redirected the request."))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case http.StatusRequestTimeout:
			return withConnectionDiagnostic(connectionFailureTimeout, errors.New("GitHub request timed out."))
		case http.StatusUnauthorized:
			return withConnectionDiagnostic(connectionFailureAuthentication, errors.New("GitHub rejected the credential."))
		case http.StatusForbidden:
			if githubForbiddenIsRateLimited(resp) {
				return withConnectionDiagnostic(connectionFailureRateLimit, errors.New("GitHub rate limited the request."))
			}
			return withConnectionDiagnostic(connectionFailureAccess, errors.New("GitHub denied repository access."))
		case http.StatusNotFound:
			return withConnectionDiagnostic(connectionFailureEndpoint, errors.New("GitHub could not find the repository."))
		case http.StatusTooManyRequests:
			return withConnectionDiagnostic(connectionFailureRateLimit, errors.New("GitHub rate limited the request."))
		default:
			return errors.New("GitHub returned an unexpected response.")
		}
	}
	body, err := readLimited(resp.Body, onboardingCheckMaxBody)
	if err != nil {
		return errors.New("GitHub response could not be read safely.")
	}
	defer clear(body)
	var repository repository
	if json.Unmarshal(body, &repository) != nil || repository.Name == "" || !validRepository(repository.FullName) || !strings.EqualFold(repository.FullName, fields.Repository) {
		return errors.New("GitHub returned an invalid repository response.")
	}
	return nil
}

func onboardingGitHubRepositoryURL(fields onboardingDraftFields) (string, error) {
	origin, err := validateOrigin(fields.Origin)
	if err != nil || origin != fields.Origin || !validRepository(fields.Repository) {
		return "", errOnboardingInvalid
	}
	return repositoryURL(target{Origin: origin, Repository: fields.Repository})
}

func checkOnboardingJenkins(ctx context.Context, client *http.Client, fields onboardingDraftFields, credential string) error {
	endpoint, err := onboardingJenkinsJobURL(fields)
	if err != nil {
		return errOnboardingInvalid
	}
	endpoint.RawQuery = "tree=name,fullName,buildable,lastBuild[number,result,building]"
	resp, err := jenkinsRequest(ctx, client, http.MethodGet, endpoint, fields.Username, credential)
	if err != nil {
		return connectionTransportFailure(err, "Could not contact the registered Jenkins job.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return jenkinsStatusError(resp.StatusCode)
	}
	body, err := readLimited(resp.Body, onboardingCheckMaxBody)
	if err != nil {
		return errors.New("Jenkins response could not be read safely.")
	}
	defer clear(body)
	var job jenkinsJobResponse
	if json.Unmarshal(body, &job) != nil || job.FullName != fields.JobPath || job.Name != lastJobSegment(fields.JobPath) {
		return errors.New("Jenkins returned an invalid job response.")
	}
	return nil
}

func onboardingJenkinsJobURL(fields onboardingDraftFields) (*url.URL, error) {
	baseURL, err := validateJenkinsBaseURL(fields.BaseURL)
	if err != nil || baseURL != fields.BaseURL || !validJenkinsJobPath(fields.JobPath) {
		return nil, errOnboardingInvalid
	}
	return jenkinsJobURL(jenkinsTarget{BaseURL: baseURL, JobPath: fields.JobPath}, "api", "json")
}

func checkOnboardingDashboard(ctx context.Context, client *http.Client, fields onboardingDraftFields, credential string) error {
	endpoint, err := onboardingDashboardNamespacesURL(fields)
	if err != nil {
		return errOnboardingInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return errOnboardingInvalid
	}
	// Keep the saved Dashboard contract unchanged: this is a single Bearer GET.
	// It does not infer a deployment track, exchange a login token, or retry.
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return connectionTransportFailure(err, "Could not connect to the registered Dashboard target.")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Dashboard redirected the request."))
	}
	switch resp.StatusCode {
	case http.StatusRequestTimeout:
		return withConnectionDiagnostic(connectionFailureTimeout, errors.New("Dashboard request timed out."))
	case http.StatusUnauthorized:
		return withConnectionDiagnostic(connectionFailureAuthentication, errors.New("Dashboard rejected the API Bearer."))
	case http.StatusForbidden:
		return withConnectionDiagnostic(connectionFailureAccess, errors.New("Dashboard denied namespace access."))
	case http.StatusNotFound:
		return withConnectionDiagnostic(connectionFailureEndpoint, errors.New("Dashboard could not find the namespace endpoint."))
	case http.StatusTooManyRequests:
		return withConnectionDiagnostic(connectionFailureRateLimit, errors.New("Dashboard rate limited the request."))
	case http.StatusOK:
	default:
		return errors.New("Dashboard returned an unexpected response.")
	}
	body, err := readLimited(resp.Body, onboardingCheckMaxBody)
	if err != nil {
		return errors.New("Dashboard response could not be read safely.")
	}
	defer clear(body)
	var payload struct {
		ListMeta *struct {
			TotalItems *int `json:"totalItems"`
		} `json:"listMeta"`
		Namespaces []struct {
			ObjectMeta struct {
				Name string `json:"name"`
			} `json:"objectMeta"`
		} `json:"namespaces"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.ListMeta == nil || payload.ListMeta.TotalItems == nil || payload.Namespaces == nil || *payload.ListMeta.TotalItems < len(payload.Namespaces) {
		return errors.New("Dashboard returned an invalid namespace response.")
	}
	for _, namespace := range payload.Namespaces {
		if !validDashboardNamespace(namespace.ObjectMeta.Name) {
			return errors.New("Dashboard returned an invalid namespace response.")
		}
	}
	return nil
}

func onboardingDashboardNamespacesURL(fields onboardingDraftFields) (*url.URL, error) {
	baseURL, err := validateJenkinsBaseURL(fields.BaseURL)
	if err != nil || baseURL != fields.BaseURL {
		return nil, errOnboardingInvalid
	}
	endpoint, err := url.Parse(baseURL)
	if err != nil {
		return nil, errOnboardingInvalid
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/api/v1/namespace"
	endpoint.RawPath = ""
	query := url.Values{}
	query.Set("itemsPerPage", strconv.Itoa(dashboardNamespaceLimit))
	endpoint.RawQuery = query.Encode()
	endpoint.Fragment = ""
	return endpoint, nil
}

// onboardingCheckProofRegistryMemory keeps only a keyed fingerprint of the
// exact credential and transient bindings. It is never serialized; creating a
// new controller creates a new key and invalidates every outstanding permit.
type onboardingCheckProofRegistryMemory struct {
	mu          sync.Mutex
	key         []byte
	entries     map[string]onboardingCheckProofEntry
	generations map[string]uint64
	ttl         time.Duration
}

type onboardingCheckProofEntry struct {
	draftID     string
	fingerprint [sha256.Size]byte
	expiresAt   time.Time
}

func newOnboardingCheckProofRegistryMemory() *onboardingCheckProofRegistryMemory {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		clear(key)
		return nil
	}
	return &onboardingCheckProofRegistryMemory{
		key: key, entries: make(map[string]onboardingCheckProofEntry),
		generations: make(map[string]uint64), ttl: onboardingCheckPermitTTL,
	}
}

// BeginDraftCheck advances the current attempt generation and revokes every
// older permit. A generation from an older concurrent StartCheck cannot move
// the registry backward or issue a permit after a newer attempt has begun.
func (r *onboardingCheckProofRegistryMemory) BeginDraftCheck(draftID string, generation uint64) error {
	if r == nil || len(r.key) != 32 || !onboardingDraftIDPattern.MatchString(draftID) || generation == 0 {
		return errOnboardingCheckUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation <= r.generations[draftID] {
		return errOnboardingDraftStale
	}
	for permit, entry := range r.entries {
		if entry.draftID == draftID {
			delete(r.entries, permit)
		}
	}
	r.generations[draftID] = generation
	return nil
}

func (r *onboardingCheckProofRegistryMemory) Issue(input onboardingCheckProofInput, now time.Time) (string, error) {
	if r == nil || len(r.key) != 32 || !validOnboardingCheckProofInput(input) || now.IsZero() {
		return "", errOnboardingCheckUnavailable
	}
	permitBytes := make([]byte, 32)
	if _, err := rand.Read(permitBytes); err != nil {
		clear(permitBytes)
		return "", errOnboardingCheckUnavailable
	}
	permit := hex.EncodeToString(permitBytes)
	clear(permitBytes)
	fingerprint := onboardingCheckProofFingerprint(r.key, input)
	r.mu.Lock()
	if r.generations[input.DraftID] != input.CheckGeneration {
		r.mu.Unlock()
		return "", errOnboardingCheckRequired
	}
	r.pruneLocked(now)
	for existingPermit, entry := range r.entries {
		if entry.draftID == input.DraftID {
			delete(r.entries, existingPermit)
		}
	}
	r.entries[permit] = onboardingCheckProofEntry{draftID: input.DraftID, fingerprint: fingerprint, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return permit, nil
}

// Validate consumes a valid permit exactly once. A wrong credential or stale
// binding leaves the permit unusable for this call but does not consume it, so
// a corrected activation request can still use the fresh proof.
func (r *onboardingCheckProofRegistryMemory) Validate(permit string, input onboardingCheckProofInput, now time.Time) error {
	if r == nil || len(r.key) != 32 || len(permit) != 64 || !validOnboardingCheckProofInput(input) || now.IsZero() {
		return errOnboardingCheckRequired
	}
	if _, err := hex.DecodeString(permit); err != nil {
		return errOnboardingCheckRequired
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.entries[permit]
	if !ok {
		return errOnboardingCheckRequired
	}
	if r.generations[input.DraftID] != input.CheckGeneration {
		delete(r.entries, permit)
		return errOnboardingCheckRequired
	}
	if !now.Before(entry.expiresAt) {
		delete(r.entries, permit)
		return errOnboardingCheckRequired
	}
	if entry.draftID != input.DraftID {
		return errOnboardingCheckRequired
	}
	fingerprint := onboardingCheckProofFingerprint(r.key, input)
	if !hmac.Equal(entry.fingerprint[:], fingerprint[:]) {
		return errOnboardingCheckRequired
	}
	delete(r.entries, permit)
	return nil
}

func (r *onboardingCheckProofRegistryMemory) InvalidateDraft(id string) {
	if r == nil || id == "" {
		return
	}
	r.mu.Lock()
	delete(r.generations, id)
	for permit, entry := range r.entries {
		if entry.draftID == id {
			delete(r.entries, permit)
		}
	}
	r.mu.Unlock()
}

func (r *onboardingCheckProofRegistryMemory) pruneLocked(now time.Time) {
	for permit, entry := range r.entries {
		if !now.Before(entry.expiresAt) {
			delete(r.entries, permit)
		}
	}
}

func validOnboardingCheckProofInput(input onboardingCheckProofInput) bool {
	if !onboardingDraftIDPattern.MatchString(input.DraftID) || input.DraftRevision == 0 || input.CheckGeneration == 0 ||
		!validOnboardingDigest(input.CandidateDigest, "sha256:") || !validOnboardingDigest(input.PolicyDigest, "policy:sha256:") ||
		!validOnboardingDigest(input.ConfigRevision, "config:sha256:") || len(input.Credential) == 0 || len(input.Credential) > maxSecretSize {
		return false
	}
	switch input.CredentialSource {
	case onboardingCredentialTemporary:
		return input.NamedSecretID == "" && input.SecretMetadataDigest == ""
	case onboardingCredentialNamed:
		return namedSecretIDPattern.MatchString(input.NamedSecretID) && validOnboardingDigest(input.SecretMetadataDigest, "sha256:")
	default:
		return false
	}
}

func onboardingCheckProofFingerprint(key []byte, input onboardingCheckProofInput) [sha256.Size]byte {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, "local-agent-harness/onboarding-check-proof/v1\x00")
	writeProofString(mac, input.DraftID)
	writeProofUint64(mac, input.DraftRevision)
	writeProofUint64(mac, input.CheckGeneration)
	writeProofString(mac, input.CandidateDigest)
	writeProofString(mac, input.PolicyDigest)
	writeProofString(mac, input.ConfigRevision)
	writeProofString(mac, string(input.CredentialSource))
	writeProofString(mac, input.NamedSecretID)
	writeProofString(mac, input.SecretMetadataDigest)
	writeProofBytes(mac, input.Credential)
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func writeProofString(mac io.Writer, value string) {
	writeProofBytes(mac, []byte(value))
}

func writeProofUint64(mac io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = mac.Write(encoded[:])
}

func writeProofBytes(mac io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = mac.Write(length[:])
	_, _ = mac.Write(value)
}
