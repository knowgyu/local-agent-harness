//go:build !windows

package main

type systemMCPGatewayTokenCredentialProvider struct{}

func newSystemMCPGatewayTokenCredentialProvider() gatewayTokenCredentialProvider {
	return systemMCPGatewayTokenCredentialProvider{}
}

func (systemMCPGatewayTokenCredentialProvider) LoadGatewayToken() ([]byte, error) {
	return nil, errMCPGatewayTokenStore
}

func (systemMCPGatewayTokenCredentialProvider) SaveGatewayToken([]byte) error {
	return errMCPGatewayTokenStore
}

func (systemMCPGatewayTokenCredentialProvider) DeleteGatewayToken() error {
	return errMCPGatewayTokenStore
}
