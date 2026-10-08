package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxGitImportRemotes          = 32
	maxGitImportMatchesPerRemote = 8
	maxGitImportPreviewBytes     = 64 << 10
)

var (
	gitSectionPattern    = regexp.MustCompile(`^\[\s*([A-Za-z][A-Za-z0-9-]*)(?:\s+"([^"\\\r\n]+)")?\s*\]\s*(?:[#;].*)?$`)
	gitRemoteNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	gitDNSNamePattern    = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
)

type parsedGitRemote struct {
	name       string
	origin     string
	repository string
}

type gitImportTarget struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type gitImportCandidate struct {
	Source           string            `json:"source"`
	Remote           string            `json:"remote"`
	Repository       string            `json:"repository"`
	Confidence       string            `json:"confidence"`
	GitHubMatches    []gitImportTarget `json:"github_matches"`
	MatchesTruncated bool              `json:"matches_truncated"`
	Unverified       []string          `json:"unverified"`
}

type gitImportPreview struct {
	Candidates []gitImportCandidate `json:"candidates"`
	Error      string               `json:"error,omitempty"`
}

type gitConfigRemote struct {
	name   string
	url    string
	hasURL bool
}

func (a *app) handleGitRemotePreview(w http.ResponseWriter, r *http.Request) {
	if !a.checkPostLimit(w, r, maxGitImportRequest) {
		return
	}
	if r.MultipartForm == nil {
		writeGitImportError(w, http.StatusBadRequest, "Choose one file named config for preview.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["git_config"]) != 1 || len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["csrf"]) != 1 {
		writeGitImportError(w, http.StatusBadRequest, "Choose one file named config for preview.")
		return
	}
	fileHeader := r.MultipartForm.File["git_config"][0]
	if fileHeader.Filename != "config" || fileHeader.Size < 1 || fileHeader.Size > maxGitConfigSize {
		writeGitImportError(w, http.StatusBadRequest, "The selected file must be named config and no larger than 64 KiB.")
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeGitImportError(w, http.StatusBadRequest, "The selected file could not be read.")
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxGitConfigSize+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxGitConfigSize {
		writeGitImportError(w, http.StatusBadRequest, "The selected file could not be read within the 64 KiB limit.")
		return
	}
	remotes, err := parseSelectedGitConfig(data)
	if err != nil {
		writeGitImportError(w, http.StatusBadRequest, "The selected file contains an unsupported or unsafe remote entry.")
		return
	}
	cfg, err := readConfig(a.configPath)
	if err != nil {
		writeGitImportError(w, http.StatusInternalServerError, "Registered targets could not be read.")
		return
	}
	preview := gitImportPreview{Candidates: make([]gitImportCandidate, 0, len(remotes))}
	for _, remote := range remotes {
		candidate := gitImportCandidate{
			Source:        "selected file named config; actual path unverified",
			Remote:        cleanOutput(remote.name, "", 80),
			Repository:    cleanOutput(remote.repository, "", 400),
			Confidence:    "medium",
			GitHubMatches: []gitImportTarget{},
			Unverified:    []string{"Jenkins", "Harbor", "Dashboard", "permissions"},
		}
		for _, target := range cfg.GitHubTargets {
			if !target.Disabled && target.Origin == remote.origin && target.Repository == remote.repository {
				if len(candidate.GitHubMatches) == maxGitImportMatchesPerRemote {
					candidate.MatchesTruncated = true
					break
				}
				candidate.GitHubMatches = append(candidate.GitHubMatches, gitImportTarget{
					ID:   target.ID,
					Name: cleanOutput(target.Name, "", 80),
				})
			}
		}
		if candidate.MatchesTruncated || len(candidate.GitHubMatches) > 1 {
			candidate.Confidence = "low"
		} else if len(candidate.GitHubMatches) == 1 {
			candidate.Confidence = "high"
		}
		preview.Candidates = append(preview.Candidates, candidate)
	}
	writeGitImportJSON(w, http.StatusOK, preview)
}

func writeGitImportError(w http.ResponseWriter, status int, message string) {
	writeGitImportJSON(w, status, gitImportPreview{Error: message})
}

func writeGitImportJSON(w http.ResponseWriter, status int, value gitImportPreview) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Preview could not be encoded."}`)
	}
	if len(data)+1 > maxGitImportPreviewBytes {
		status = http.StatusRequestEntityTooLarge
		data = []byte(`{"error":"Preview was not returned because it exceeds the 64 KiB response limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func parseSelectedGitConfig(data []byte) ([]parsedGitRemote, error) {
	if len(data) == 0 || len(data) > maxGitConfigSize || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
		return nil, errors.New("unsupported git config")
	}
	remotes := make([]*gitConfigRemote, 0, 4)
	byName := make(map[string]*gitConfigRemote)
	currentRemote := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.ContainsRune(line, '\r') {
			return nil, errors.New("unsupported git config")
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			matches := gitSectionPattern.FindStringSubmatch(trimmed)
			if matches == nil {
				return nil, errors.New("unsupported git config")
			}
			currentRemote = ""
			if !strings.EqualFold(matches[1], "remote") {
				continue // Includes and all other sections are data only; they are never followed.
			}
			name := matches[2]
			if !gitRemoteNamePattern.MatchString(name) {
				return nil, errors.New("unsupported remote name")
			}
			remote, ok := byName[name]
			if !ok {
				if len(remotes) == maxGitImportRemotes {
					return nil, errors.New("too many remotes")
				}
				remote = &gitConfigRemote{name: name}
				byName[name] = remote
				remotes = append(remotes, remote)
			}
			currentRemote = name
			continue
		}
		if currentRemote == "" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			if fields := strings.Fields(trimmed); len(fields) > 0 && strings.EqualFold(fields[0], "url") {
				return nil, errors.New("unsupported remote URL assignment")
			}
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), "url") {
			continue
		}
		remote := byName[currentRemote]
		if remote.hasURL {
			return nil, errors.New("multiple remote URLs")
		}
		remoteURL, err := parseSimpleGitConfigValue(value)
		if err != nil {
			return nil, errors.New("unsupported remote URL value")
		}
		remote.url, remote.hasURL = remoteURL, true
	}

	parsed := make([]parsedGitRemote, 0, len(remotes))
	for _, remote := range remotes {
		if !remote.hasURL {
			continue
		}
		origin, repository, err := parseSafeGitRemoteURL(remote.url)
		if err != nil {
			return nil, errors.New("unsafe remote URL")
		}
		parsed = append(parsed, parsedGitRemote{name: remote.name, origin: origin, repository: repository})
	}
	return parsed, nil
}

func parseSimpleGitConfigValue(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("empty value")
	}
	if value[0] == '"' {
		end := strings.IndexByte(value[1:], '"')
		if end < 0 {
			return "", errors.New("unterminated quoted value")
		}
		valueEnd := end + 1
		parsed := value[1:valueEnd]
		if strings.ContainsAny(parsed, `"\`) {
			return "", errors.New("escaped values are unsupported")
		}
		rest := strings.TrimSpace(value[valueEnd+1:])
		if rest != "" && !strings.HasPrefix(rest, "#") && !strings.HasPrefix(rest, ";") {
			return "", errors.New("trailing value data")
		}
		return parsed, nil
	}
	for index, char := range value {
		if char == ' ' || char == '\t' {
			rest := strings.TrimSpace(value[index:])
			if !strings.HasPrefix(rest, "#") && !strings.HasPrefix(rest, ";") {
				return "", errors.New("unsupported unquoted value")
			}
			value = value[:index]
			break
		}
	}
	if value == "" || strings.ContainsAny(value, `"\`) {
		return "", errors.New("unsupported unquoted value")
	}
	return value, nil
}

func parseSafeGitRemoteURL(raw string) (string, string, error) {
	if raw == "" || strings.ContainsAny(raw, "?#%\\\r\n\t ") {
		return "", "", errors.New("unsafe remote URL")
	}
	if strings.HasPrefix(raw, "git@") && !strings.Contains(raw, "://") {
		separator := strings.IndexByte(raw, ':')
		if separator < len("git@")+1 || strings.Contains(raw[len("git@"):separator], ":") {
			return "", "", errors.New("unsupported scp remote")
		}
		origin, err := safeGitRemoteOrigin(raw[len("git@"):separator])
		if err != nil {
			return "", "", err
		}
		repository, err := repositoryFromRemotePath("/" + raw[separator+1:])
		return origin, repository, err
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", "", errors.New("unsafe remote URL")
	}
	switch u.Scheme {
	case "https":
		if u.User != nil {
			return "", "", errors.New("HTTPS remote userinfo is unsupported")
		}
		origin, err := safeGitRemoteOrigin(u.Host)
		if err != nil {
			return "", "", err
		}
		repository, err := repositoryFromRemotePath(u.Path)
		return origin, repository, err
	case "ssh":
		if u.User == nil || u.User.Username() != "git" {
			return "", "", errors.New("unsupported ssh user")
		}
		if _, hasPassword := u.User.Password(); hasPassword {
			return "", "", errors.New("ssh passwords are unsupported")
		}
		origin, err := safeGitRemoteOrigin(u.Host)
		if err != nil {
			return "", "", err
		}
		repository, err := repositoryFromRemotePath(u.Path)
		return origin, repository, err
	default:
		return "", "", errors.New("unsupported remote scheme")
	}
}

func safeGitRemoteOrigin(host string) (string, error) {
	origin, err := validateOrigin("https://" + host)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return "", errors.New("invalid remote host")
	}
	hostname := parsed.Hostname()
	if net.ParseIP(hostname) == nil && !gitDNSNamePattern.MatchString(hostname) {
		return "", errors.New("invalid remote host")
	}
	return origin, nil
}

func repositoryFromRemotePath(path string) (string, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return "", errors.New("unsupported repository path")
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 2 {
		return "", errors.New("unsupported repository path")
	}
	parts[1] = strings.TrimSuffix(parts[1], ".git")
	repository := parts[0] + "/" + parts[1]
	if !validRepository(repository) {
		return "", errors.New("invalid repository path")
	}
	return repository, nil
}
