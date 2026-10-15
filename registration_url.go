package main

import (
	"errors"
	"net/url"
	"strings"
)

// registrationService uses the setup draft service kind so URL candidates can
// be copied into the existing setup draft contract without translating kinds.
type registrationService = SetupServiceKind

const (
	registrationServiceGitHub    registrationService = SetupServiceGitHub
	registrationServiceJenkins   registrationService = SetupServiceJenkins
	registrationServiceDashboard registrationService = SetupServiceDashboard
)

// registrationURLCandidate contains only normalized, non-secret values that
// can be reviewed before they are used to create a service target.
type registrationURLCandidate struct {
	Origin        string
	Repository    string
	BaseURL       string
	JobPath       string
	MissingFields []string
	ReviewFields  []string
}

func parseRegistrationURL(service registrationService, raw string) (registrationURLCandidate, error) {
	if len(raw) == 0 || len(raw) > 2048 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "?#\\\r\n\t") {
		return registrationURLCandidate{}, errors.New("invalid registration URL")
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" || strings.HasSuffix(u.Host, ":") || u.RawQuery != "" || u.Fragment != "" {
		return registrationURLCandidate{}, errors.New("registration URL must be HTTPS without credentials, query, or fragment")
	}

	// safeGitRemoteOrigin validates the origin using validateOrigin and also
	// rejects malformed DNS names. Keep that check common to all service kinds.
	origin, err := safeGitRemoteOrigin(u.Host)
	if err != nil {
		return registrationURLCandidate{}, errors.New("invalid registration URL host")
	}

	segments, err := registrationURLPathSegments(u)
	if err != nil {
		return registrationURLCandidate{}, errors.New("invalid registration URL path")
	}

	switch service {
	case registrationServiceGitHub:
		return parseGitHubRegistrationURL(origin, segments)
	case registrationServiceJenkins:
		return parseJenkinsRegistrationURL(origin, segments)
	case registrationServiceDashboard:
		return parseDashboardRegistrationURL(origin, segments)
	default:
		return registrationURLCandidate{}, errors.New("unsupported registration service")
	}
}

func parseGitHubRegistrationURL(origin string, segments []string) (registrationURLCandidate, error) {
	if len(segments) == 0 {
		return registrationURLCandidate{
			Origin:        origin,
			MissingFields: []string{"repository"},
		}, nil
	}

	repository, err := repositoryFromRemotePath("/" + strings.Join(segments, "/"))
	if err != nil {
		return registrationURLCandidate{}, errors.New("invalid GitHub repository URL")
	}
	return registrationURLCandidate{Origin: origin, Repository: repository}, nil
}

func parseJenkinsRegistrationURL(origin string, segments []string) (registrationURLCandidate, error) {
	if !containsRegistrationJobMarker(segments) {
		baseURL, err := registrationBaseURL(origin, segments)
		if err != nil {
			return registrationURLCandidate{}, err
		}
		return registrationURLCandidate{
			BaseURL:       baseURL,
			MissingFields: []string{"job_path"},
		}, nil
	}

	start, jobNames, matched := registrationJenkinsJobSuffix(segments)
	if !matched {
		// A marker without a canonical suffix does not identify which part is
		// Jenkins context and which part is a job. Preserve no guessed split.
		return registrationURLCandidate{
			BaseURL:       origin,
			MissingFields: []string{"job_path"},
			ReviewFields:  []string{"base_url", "job_path"},
		}, nil
	}

	baseURL, err := registrationBaseURL(origin, segments[:start])
	if err != nil || !validJenkinsJobPath(strings.Join(jobNames, "/")) {
		return registrationURLCandidate{}, errors.New("invalid Jenkins registration URL")
	}
	return registrationURLCandidate{
		BaseURL:      baseURL,
		JobPath:      strings.Join(jobNames, "/"),
		ReviewFields: []string{"base_url", "job_path"},
	}, nil
}

func parseDashboardRegistrationURL(origin string, segments []string) (registrationURLCandidate, error) {
	baseURL, err := registrationBaseURL(origin, segments)
	if err != nil {
		return registrationURLCandidate{}, err
	}
	return registrationURLCandidate{
		BaseURL:      baseURL,
		ReviewFields: []string{"base_url"},
	}, nil
}

func registrationBaseURL(origin string, segments []string) (string, error) {
	raw := origin
	if len(segments) > 0 {
		raw += "/" + strings.Join(segments, "/")
	}
	baseURL, err := validateJenkinsBaseURL(raw)
	if err != nil {
		return "", errors.New("invalid registration URL base path")
	}
	return baseURL, nil
}

func containsRegistrationJobMarker(segments []string) bool {
	for _, segment := range segments {
		if segment == "job" {
			return true
		}
	}
	return false
}

// registrationJenkinsJobSuffix finds the longest canonical sequence of one or
// more Jenkins /job/<name> pairs at the end of the path. Shorter matching
// suffixes are tails of that same chain, so choosing the longest chain gives a
// consistent prefill. It does not prove that the preceding path is not itself
// part of a deployment-specific Jenkins context path; callers must flag both
// inferred fields for review.
func registrationJenkinsJobSuffix(segments []string) (int, []string, bool) {
	var foundStart int
	var foundNames []string
	found := false
	for start := 0; start < len(segments); start++ {
		if segments[start] != "job" || (len(segments)-start)%2 != 0 {
			continue
		}

		names := make([]string, 0, (len(segments)-start)/2)
		matches := true
		for marker := start; marker < len(segments); marker += 2 {
			if segments[marker] != "job" {
				matches = false
				break
			}
			name := segments[marker+1]
			if !safeJenkinsSegment(name) {
				matches = false
				break
			}
			names = append(names, name)
		}
		if matches {
			if !found || start < foundStart {
				found = true
				foundStart = start
				foundNames = names
			}
		}
	}
	return foundStart, foundNames, found
}

// registrationURLPathSegments decodes each escaped path component separately.
// This permits harmless encodings of characters accepted by the existing
// service validators while rejecting encoded separators and traversal.
func registrationURLPathSegments(u *url.URL) ([]string, error) {
	escapedPath := u.EscapedPath()
	if escapedPath == "" || escapedPath == "/" {
		return nil, nil
	}
	if !strings.HasPrefix(escapedPath, "/") {
		return nil, errors.New("invalid path")
	}

	path := strings.TrimPrefix(escapedPath, "/")
	if strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	if path == "" {
		return nil, nil
	}

	escapedSegments := strings.Split(path, "/")
	segments := make([]string, 0, len(escapedSegments))
	for _, escapedSegment := range escapedSegments {
		if escapedSegment == "" {
			return nil, errors.New("empty path segment")
		}
		segment, err := url.PathUnescape(escapedSegment)
		if err != nil || segment == "" || strings.ContainsAny(segment, "/\\") ||
			segment == "." || segment == ".." || !safeJenkinsSegment(segment) {
			return nil, errors.New("invalid path segment")
		}
		segments = append(segments, segment)
	}
	return segments, nil
}
