package main

import (
	"context"
	"net/http"
)

// onboardingAppController composes the durable sidecar store with the checker
// and activation controller that share one process-local credential permit
// registry.
type onboardingAppController struct {
	store   onboardingDraftStore
	checker *onboardingCheckController
	proofs  onboardingCheckProofRegistry
}

func newOnboardingAppController(
	store onboardingDraftStore,
	configPath string,
	secrets secretStore,
	client *http.Client,
	proofs onboardingCheckProofRegistry,
) *onboardingAppController {
	checker := newOnboardingCheckController(store, configPath, secrets, client, proofs)
	return &onboardingAppController{
		store:   store,
		checker: checker,
		proofs:  checker.proofs,
	}
}

func (a *app) ensureOnboardingController() (onboardingController, error) {
	if a == nil || a.configPath == "" {
		return nil, errOnboardingUnavailable
	}

	a.onboardingMu.Lock()
	defer a.onboardingMu.Unlock()
	if a.onboarding != nil {
		return a.onboarding, nil
	}

	store, err := newOnboardingDraftStoreForConfigPath(a.configPath)
	if err != nil {
		return nil, errOnboardingUnavailable
	}
	proofs := newOnboardingCheckProofRegistryMemory()
	controller := newOnboardingAppController(store, a.configPath, a.secrets, a.client, proofs)
	a.onboarding = controller
	return a.onboarding, nil
}

func (c *onboardingAppController) ListDrafts(ctx context.Context) ([]onboardingDraft, error) {
	if c == nil || c.store == nil {
		return nil, errOnboardingUnavailable
	}
	return c.store.ListDrafts(ctx)
}

func (c *onboardingAppController) GetDraft(ctx context.Context, id string) (onboardingDraft, error) {
	if c == nil || c.store == nil {
		return onboardingDraft{}, errOnboardingUnavailable
	}
	return c.store.GetDraft(ctx, id)
}

func (c *onboardingAppController) SaveDraft(ctx context.Context, request onboardingDraftSaveRequest) (onboardingDraft, error) {
	if c == nil || c.store == nil || c.proofs == nil || ctx == nil {
		return onboardingDraft{}, errOnboardingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return onboardingDraft{}, errOnboardingUnavailable
	}

	var draft onboardingDraft
	var err error
	if request.ID == "" {
		if request.ExpectedRevision != 0 {
			return onboardingDraft{}, errOnboardingInvalid
		}
		draft, err = c.store.CreateDraft(ctx, request.Service, request.Fields)
	} else {
		if request.ExpectedRevision == 0 {
			return onboardingDraft{}, errOnboardingInvalid
		}
		draft, err = c.store.UpdateDraft(ctx, request.ID, request.ExpectedRevision, request.Fields)
	}
	if err != nil {
		return onboardingDraft{}, err
	}
	c.proofs.InvalidateDraft(draft.ID)
	return draft, nil
}

func (c *onboardingAppController) DeleteDraft(ctx context.Context, id string, expectedRevision uint64) error {
	if c == nil || c.store == nil || c.proofs == nil || ctx == nil {
		return errOnboardingUnavailable
	}
	if err := ctx.Err(); err != nil {
		return errOnboardingUnavailable
	}
	if id == "" || expectedRevision == 0 {
		return errOnboardingInvalid
	}
	if err := c.store.DeleteDraft(ctx, id, expectedRevision); err != nil {
		return err
	}
	c.proofs.InvalidateDraft(id)
	return nil
}

func (c *onboardingAppController) CheckDraft(ctx context.Context, request onboardingCheckRequest) (onboardingCheckResult, error) {
	if c == nil || c.checker == nil {
		return onboardingCheckResult{}, errOnboardingUnavailable
	}
	return c.checker.CheckDraft(ctx, request)
}

func (c *onboardingAppController) ReviewDraft(ctx context.Context, id string, expectedRevision uint64) (onboardingPolicyReview, error) {
	if c == nil || c.checker == nil {
		return onboardingPolicyReview{}, errOnboardingUnavailable
	}
	return c.checker.ReviewDraft(ctx, id, expectedRevision)
}

func (c *onboardingAppController) ActivateDraft(ctx context.Context, request onboardingActivationRequest) (onboardingActivationRecord, error) {
	if c == nil || c.checker == nil {
		return onboardingActivationRecord{}, errOnboardingUnavailable
	}
	return c.checker.ActivateDraft(ctx, request)
}

func (c *onboardingAppController) RecordFirstRead(ctx context.Context, receipt onboardingFirstReadReceipt) (bool, error) {
	if c == nil || c.store == nil {
		return false, errOnboardingUnavailable
	}
	return c.store.RecordFirstRead(ctx, receipt)
}

func (c *onboardingAppController) ListFirstReads(ctx context.Context) ([]onboardingFirstReadReceipt, error) {
	if c == nil || c.store == nil || c.checker == nil || ctx == nil {
		return nil, errOnboardingUnavailable
	}
	receipts, err := c.store.ListFirstReads(ctx)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errOnboardingUnavailable
	}

	var cfg config
	err = withConfigLock(func() error {
		if err := ctx.Err(); err != nil {
			return errOnboardingUnavailable
		}
		var readErr error
		cfg, readErr = readConfig(c.checker.configPath)
		if readErr != nil {
			return errOnboardingUnavailable
		}
		return nil
	})
	if err != nil {
		return nil, errOnboardingUnavailable
	}

	current := make([]onboardingFirstReadReceipt, 0, len(receipts))
	for _, receipt := range receipts {
		service := string(receipt.Service)
		if receipt.Source == onboardingFirstReadSource && validFirstReadServiceTool(service, receipt.Tool) &&
			validFirstReadDefinitionDigest(receipt.DefinitionDigest) && validFirstReadReceiptTime(receipt.ObservedAt) &&
			firstReadDefinitionDigestIsActive(cfg, service, receipt.Tool, receipt.DefinitionDigest) {
			current = append(current, receipt)
		}
	}
	return current, nil
}

var _ onboardingController = (*onboardingAppController)(nil)
