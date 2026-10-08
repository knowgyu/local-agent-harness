package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type harborProjectQuota struct {
	Hard *map[string]int64 `json:"hard,omitempty"`
	Used *map[string]int64 `json:"used,omitempty"`
}

type harborProjectQuotaResult struct {
	Target  string              `json:"target"`
	Project string              `json:"project"`
	Quota   *harborProjectQuota `json:"quota,omitempty"`
}

func (a *app) registeredHarborProjectQuota(ctx context.Context, requestedName string) (harborProjectQuotaResult, error) {
	target, secret, err := a.loadHarborTarget(requestedName)
	if err != nil {
		return harborProjectQuotaResult{}, err
	}
	defer clear(secret)

	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	return fetchHarborProjectQuota(ctx, target, string(secret), a.client)
}

func fetchHarborProjectQuota(ctx context.Context, target harborTarget, secret string, client *http.Client) (harborProjectQuotaResult, error) {
	endpoint, err := harborProjectSummaryURL(target)
	if err != nil {
		return harborProjectQuotaResult{}, errors.New("The registered Harbor target is invalid.")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return harborProjectQuotaResult{}, errors.New("Could not create Harbor project quota request.")
	}
	req.SetBasicAuth(target.Username, secret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "local-agent-harness")
	if client == nil {
		client = newGitHubClient()
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirect.Do(req)
	if err != nil {
		return harborProjectQuotaResult{}, errors.New("Could not reach registered Harbor base URL. Check address, VPN, TLS certificate.")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return harborProjectQuotaResult{}, errors.New("Harbor redirected project quota request. Check registered base URL.")
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return harborProjectQuotaResult{}, errors.New("Harbor rejected credential (401). Check registered username credential.")
	case http.StatusForbidden:
		return harborProjectQuotaResult{}, errors.New("Harbor denied project quota read access (403).")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return harborProjectQuotaResult{}, fmt.Errorf("Harbor returned HTTP %d.", resp.StatusCode)
	}
	body, err := readLimited(resp.Body, maxAPIBytes)
	if err != nil {
		return harborProjectQuotaResult{}, errors.New("Harbor project summary response exceeded 1 MiB or could not be read.")
	}
	defer clear(body)

	var summary map[string]json.RawMessage
	if err := json.Unmarshal(body, &summary); err != nil || summary == nil {
		return harborProjectQuotaResult{}, errors.New("Harbor returned an invalid project summary response.")
	}
	result := harborProjectQuotaResult{
		Target:  cleanOutput(target.Name, secret, 80),
		Project: cleanOutput(target.Project, secret, 128),
	}
	if raw, ok := summary["quota"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil || values == nil {
			return harborProjectQuotaResult{}, errors.New("Harbor returned an invalid project quota response.")
		}
		quota := &harborProjectQuota{}
		quota.Hard, err = decodeHarborQuotaValues(values["hard"], secret)
		if err != nil {
			return harborProjectQuotaResult{}, err
		}
		quota.Used, err = decodeHarborQuotaValues(values["used"], secret)
		if err != nil {
			return harborProjectQuotaResult{}, err
		}
		if quota.Hard != nil || quota.Used != nil {
			result.Quota = quota
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > maxAPIBytes {
		return harborProjectQuotaResult{}, errors.New("Harbor project quota result exceeded 1 MiB output limit.")
	}
	return result, nil
}

func decodeHarborQuotaValues(raw json.RawMessage, secret string) (*map[string]int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var values map[string]int64
	if err := json.Unmarshal(trimmed, &values); err != nil || values == nil {
		return nil, errors.New("Harbor returned invalid project quota values.")
	}
	for key := range values {
		if key == "" || cleanOutput(key, secret, 128) != key {
			return nil, errors.New("Harbor returned invalid project quota values.")
		}
	}
	return &values, nil
}

func harborProjectSummaryURL(target harborTarget) (*url.URL, error) {
	if validateHarborTarget(target) != nil {
		return nil, errors.New("invalid Harbor target")
	}
	base, err := url.Parse(target.BaseURL)
	if err != nil {
		return nil, err
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/api/v2.0/projects/" + url.PathEscape(target.Project) + "/summary"
	base.RawPath = ""
	base.RawQuery = ""
	base.Fragment = ""
	return base, nil
}
