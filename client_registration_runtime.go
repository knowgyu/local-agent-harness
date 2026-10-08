package main

// newProductionClientRegistrationController builds the installed-client
// adapters and lets the controller create its current-user private backup
// store. Adapter inspection remains a separate request and is never performed
// during construction.
func newProductionClientRegistrationController() (MCPClientRegistrationController, error) {
	adapters, err := newProductionClientRegistrationAdapters()
	if err != nil {
		return nil, errClientRegistrationClientUnavailable
	}
	controller, err := newMCPClientRegistrationController(mcpClientRegistrationControllerOptions{
		adapters: adapters,
	})
	if err != nil {
		return nil, safeClientRegistrationError(err, errClientRegistrationBackupFailed)
	}
	return controller, nil
}

func newProductionClientRegistrationAdapters() ([]clientRegistrationAdapter, error) {
	codex, err := newCodexClientRegistrationAdapter()
	if err != nil {
		return nil, errClientRegistrationClientUnavailable
	}
	claude := newClaudeRegistrationAdapter()
	gemini, err := newGeminiCLIRegistrationAdapter()
	if err != nil {
		return nil, errClientRegistrationClientUnavailable
	}
	return []clientRegistrationAdapter{codex, claude, gemini}, nil
}

// newClientRegistrationControllerWithAdapters is the injected construction
// seam for tests and isolated callers. Requiring a store prevents accidental
// creation of the real current-user backup directory in tests.
func newClientRegistrationControllerWithAdapters(
	adapters []clientRegistrationAdapter,
	backupStore clientRegistrationBackupStore,
) (MCPClientRegistrationController, error) {
	if backupStore == nil {
		return nil, errClientRegistrationControllerOptions
	}
	controller, err := newMCPClientRegistrationController(mcpClientRegistrationControllerOptions{
		adapters:    adapters,
		backupStore: backupStore,
	})
	if err != nil {
		return nil, safeClientRegistrationError(err, errClientRegistrationControllerOptions)
	}
	return controller, nil
}
