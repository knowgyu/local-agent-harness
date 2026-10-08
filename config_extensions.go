package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxNamedSecrets       = 256
	maxNamedSecretName    = 80
	maxNamedSecretPurpose = 120
)

var namedSecretIDPattern = regexp.MustCompile(`^secret:[a-f0-9]{32}$`)

// namedSecretMetadata stores only a logical name and an opaque reference. The
// credential value remains in the operating-system credential store.
type namedSecretMetadata struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Purpose       string `json:"purpose,omitempty"`
	CredentialRef string `json:"credential_ref"`
}

type namedSecretReferenceSource struct {
	ref   string
	label string
}

func validateNamedSecrets(secrets []namedSecretMetadata) error {
	if len(secrets) > maxNamedSecrets {
		return errors.New("too many named secrets")
	}
	ids := make(map[string]bool, len(secrets))
	names := make(map[string]bool, len(secrets))
	refs := make(map[string]bool, len(secrets))
	for _, secret := range secrets {
		nameKey := strings.ToLower(secret.Name)
		validName := secret.Name != "" && strings.TrimSpace(secret.Name) == secret.Name &&
			len(secret.Name) <= maxNamedSecretName && !strings.ContainsAny(secret.Name, "\r\n\x00") && utf8.ValidString(secret.Name)
		validPurpose := len(secret.Purpose) <= maxNamedSecretPurpose && utf8.ValidString(secret.Purpose) &&
			!strings.ContainsAny(secret.Purpose, "\r\n\x00")
		validIdentity := namedSecretIDPattern.MatchString(secret.ID) && !ids[secret.ID]
		validReference := secretRefPattern.MatchString(secret.CredentialRef) && !refs[secret.CredentialRef]
		uniqueName := !names[nameKey]
		if !validName || !validPurpose || !validIdentity || !validReference || !uniqueName {
			return errors.New("invalid or duplicate named secret")
		}
		for _, r := range secret.Name {
			if unicode.IsControl(r) {
				return errors.New("invalid or duplicate named secret")
			}
		}
		for _, r := range secret.Purpose {
			if unicode.IsControl(r) {
				return errors.New("invalid or duplicate named secret")
			}
		}
		ids[secret.ID], names[nameKey], refs[secret.CredentialRef] = true, true, true
	}
	return nil
}

func validateNamedSecretReferenceCoverage(cfg config) error {
	if err := validateNamedSecrets(cfg.NamedSecrets); err != nil {
		return err
	}
	refs := make(map[string]bool, len(cfg.NamedSecrets))
	for _, secret := range cfg.NamedSecrets {
		refs[secret.CredentialRef] = true
	}
	for _, source := range configCredentialReferenceSources(cfg) {
		if !refs[source.ref] {
			return errors.New("credential reference has no named metadata")
		}
	}
	return nil
}

func ensureNamedSecretMetadata(cfg *config) error {
	if err := validateNamedSecrets(cfg.NamedSecrets); err != nil {
		return err
	}
	byRef := make(map[string]bool, len(cfg.NamedSecrets))
	usedNames := make(map[string]bool, len(cfg.NamedSecrets))
	for _, secret := range cfg.NamedSecrets {
		byRef[secret.CredentialRef] = true
		usedNames[strings.ToLower(secret.Name)] = true
	}
	sources := configCredentialReferenceSources(*cfg)
	for _, source := range sources {
		if byRef[source.ref] {
			continue
		}
		name := uniqueNamedSecretName(source.label, usedNames)
		cfg.NamedSecrets = append(cfg.NamedSecrets, namedSecretMetadata{
			ID:            legacyNamedSecretID(source.ref),
			Name:          name,
			Purpose:       "Credential retained from an existing registered item",
			CredentialRef: source.ref,
		})
		byRef[source.ref] = true
		usedNames[strings.ToLower(name)] = true
	}
	if len(cfg.NamedSecrets) > maxNamedSecrets {
		return errors.New("too many named secrets")
	}
	return validateNamedSecrets(cfg.NamedSecrets)
}

func configCredentialReferenceSources(cfg config) []namedSecretReferenceSource {
	labels := make(map[string]string)
	add := func(ref, label string) {
		if secretRefPattern.MatchString(ref) && labels[ref] == "" {
			labels[ref] = label
		}
	}
	for _, target := range cfg.GitHubTargets {
		add(target.SecretRef, "GitHub - "+target.Name+" credential")
	}
	for _, target := range cfg.JenkinsTargets {
		add(target.SecretRef, "Jenkins - "+target.Name+" credential")
	}
	for _, target := range cfg.HarborTargets {
		add(target.SecretRef, "Harbor - "+target.Name+" credential")
	}
	for _, target := range cfg.DashboardTargets {
		add(target.SecretRef, "Dashboard - "+target.Name+" credential")
	}
	refs := make([]string, 0, len(labels))
	for ref := range labels {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	sources := make([]namedSecretReferenceSource, 0, len(refs))
	for _, ref := range refs {
		sources = append(sources, namedSecretReferenceSource{ref: ref, label: truncateNamedSecretUTF8(labels[ref], maxNamedSecretName)})
	}
	return sources
}

func legacyNamedSecretID(ref string) string {
	digest := sha256.Sum256([]byte("LocalAgentHarness/named-secret/v1\x00" + ref))
	return "secret:" + hex.EncodeToString(digest[:16])
}

func uniqueNamedSecretName(base string, used map[string]bool) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "Imported credential"
	}
	for suffix := 1; ; suffix++ {
		candidate := base
		if suffix > 1 {
			tail := " (" + strconv.Itoa(suffix) + ")"
			candidate = truncateNamedSecretUTF8(base, maxNamedSecretName-len(tail)) + tail
		}
		if !used[strings.ToLower(candidate)] {
			return candidate
		}
	}
}

func truncateNamedSecretUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}
