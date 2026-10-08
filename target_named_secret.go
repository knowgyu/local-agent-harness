package main

import (
	"errors"
	"net/http"
)

var errTargetNamedSecret = errors.New("Choose one saved named secret or enter a new credential.")

func targetNamedSecretIDFromPost(r *http.Request) (string, error) {
	if r == nil {
		return "", errTargetNamedSecret
	}
	values, exists := r.PostForm["named_secret_id"]
	if !exists {
		return "", nil
	}
	if len(values) != 1 {
		return "", errTargetNamedSecret
	}
	id := values[0]
	if id == "" {
		return "", nil
	}
	if !namedSecretIDPattern.MatchString(id) {
		return "", errTargetNamedSecret
	}
	return id, nil
}

func resolveTargetNamedSecret(cfg config, id, raw string) (string, bool, error) {
	if id != "" && raw != "" {
		return "", false, errTargetNamedSecret
	}
	if id == "" {
		return "", false, nil
	}
	ref, ok := namedSecretReference(cfg, id)
	if !ok || !secretRefPattern.MatchString(ref) {
		return "", false, errTargetNamedSecret
	}
	return ref, true, nil
}

func namedSecretIDForReference(cfg config, ref string) string {
	if !secretRefPattern.MatchString(ref) {
		return ""
	}
	for _, secret := range cfg.NamedSecrets {
		if secret.CredentialRef == ref && namedSecretIDPattern.MatchString(secret.ID) {
			return secret.ID
		}
	}
	return ""
}
