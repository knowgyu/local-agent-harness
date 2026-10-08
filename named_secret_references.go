package main

func configHasNamedSecretReference(cfg config, ref string) bool {
	if ref == "" {
		return false
	}
	for _, secret := range cfg.NamedSecrets {
		if secret.CredentialRef == ref {
			return true
		}
	}
	return false
}

func namedSecretReference(cfg config, id string) (string, bool) {
	for _, secret := range cfg.NamedSecrets {
		if secret.ID == id {
			return secret.CredentialRef, true
		}
	}
	return "", false
}

func replaceCredentialConsumers(cfg *config, oldRef, newRef string) int {
	if cfg == nil || oldRef == "" || newRef == "" || oldRef == newRef {
		return 0
	}
	changed := 0
	for i := range cfg.GitHubTargets {
		if cfg.GitHubTargets[i].SecretRef == oldRef {
			cfg.GitHubTargets[i].SecretRef = newRef
			changed++
		}
	}
	for i := range cfg.JenkinsTargets {
		if cfg.JenkinsTargets[i].SecretRef == oldRef {
			cfg.JenkinsTargets[i].SecretRef = newRef
			changed++
		}
	}
	for i := range cfg.HarborTargets {
		if cfg.HarborTargets[i].SecretRef == oldRef {
			cfg.HarborTargets[i].SecretRef = newRef
			changed++
		}
	}
	for i := range cfg.DashboardTargets {
		if cfg.DashboardTargets[i].SecretRef == oldRef {
			cfg.DashboardTargets[i].SecretRef = newRef
			changed++
		}
	}
	for i := range cfg.PythonTasks {
		if cfg.PythonTasks[i].SecretRef == oldRef {
			cfg.PythonTasks[i].SecretRef = newRef
			changed++
		}
	}
	if cfg.Target != nil && cfg.Target.SecretRef == oldRef {
		cfg.Target.SecretRef = newRef
		changed++
	}
	if cfg.Jenkins != nil && cfg.Jenkins.SecretRef == oldRef {
		cfg.Jenkins.SecretRef = newRef
		changed++
	}
	return changed
}

func rebindRotatedNamedSecret(path string, cfg *config) {
	if cfg == nil || path == "" {
		return
	}
	previous, err := readConfig(path)
	if err != nil {
		return
	}
	oldRefs := credentialRefsByID(previous)
	newRefs := credentialRefsByID(*cfg)
	changes := make(map[string]map[string]bool)
	for id, oldRef := range oldRefs {
		newRef := newRefs[id]
		if oldRef == "" || newRef == "" || oldRef == newRef || configHasCredentialConsumer(*cfg, oldRef) {
			continue
		}
		if changes[oldRef] == nil {
			changes[oldRef] = make(map[string]bool)
		}
		changes[oldRef][newRef] = true
	}
	for i := range cfg.NamedSecrets {
		oldRef := cfg.NamedSecrets[i].CredentialRef
		newRefs := changes[oldRef]
		if len(newRefs) != 1 {
			continue
		}
		for newRef := range newRefs {
			if !configHasNamedSecretReference(*cfg, newRef) {
				cfg.NamedSecrets[i].CredentialRef = newRef
			}
		}
	}
}

func credentialRefsByID(cfg config) map[string]string {
	refs := make(map[string]string, len(cfg.GitHubTargets)+len(cfg.JenkinsTargets)+len(cfg.HarborTargets)+len(cfg.DashboardTargets)+len(cfg.PythonTasks))
	for _, target := range cfg.GitHubTargets {
		refs[target.ID] = target.SecretRef
	}
	for _, target := range cfg.JenkinsTargets {
		refs[target.ID] = target.SecretRef
	}
	for _, target := range cfg.HarborTargets {
		refs[target.ID] = target.SecretRef
	}
	for _, target := range cfg.DashboardTargets {
		refs[target.ID] = target.SecretRef
	}
	for _, task := range cfg.PythonTasks {
		refs[task.ID] = task.SecretRef
	}
	return refs
}
