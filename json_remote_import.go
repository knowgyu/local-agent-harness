package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxJSONGitRemoteFileBytes     = 1 << 20
	maxJSONGitRemoteRequestBytes  = maxJSONGitRemoteFileBytes + (8 << 10)
	maxJSONGitRemoteResponseBytes = 64 << 10
	maxJSONGitRemoteDepth         = 64
	maxJSONGitRemoteTokens        = 32768
	maxJSONGitRemoteStringBytes   = 4096
	maxJSONGitRemoteCandidates    = 32
)

const jsonGitRemotePreviewSource = "selected JSON file; actual path unverified"

type parsedJSONGitRemote struct {
	Origin     string
	Repository string
}

type jsonGitRemotePreviewCandidate struct {
	Source          string `json:"source"`
	Repository      string `json:"repository"`
	Confidence      string `json:"confidence"`
	RegisteredMatch bool   `json:"registered_match"`
}

type jsonGitRemotePreview struct {
	Candidates []jsonGitRemotePreviewCandidate `json:"candidates"`
	Error      string                          `json:"error,omitempty"`
}

// handleJSONGitRemotePreview previews repository URLs from one explicitly
// selected JSON file. It never imports settings or sends a request to a target.
func (a *app) handleJSONGitRemotePreview(w http.ResponseWriter, r *http.Request) {
	checkResponse := newPostResponseCapture()
	postValid := a.checkPostLimit(checkResponse, r, maxJSONGitRemoteRequestBytes)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !postValid {
		status := checkResponse.status
		if status == 0 {
			status = http.StatusForbidden
		}
		if allow := checkResponse.header.Get("Allow"); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeJSONGitRemoteJSON(w, status, jsonGitRemoteError("Request rejected."))
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 ||
		len(r.MultipartForm.File["json_remotes"]) != 1 ||
		len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["csrf"]) != 1 {
		writeJSONGitRemoteJSON(w, http.StatusBadRequest, jsonGitRemoteError("Choose one JSON file for preview."))
		return
	}

	fileHeader := r.MultipartForm.File["json_remotes"][0]
	filename := strings.ReplaceAll(fileHeader.Filename, "\\", "/")
	if !isExactJSONGitRemoteFilePart(fileHeader.Header.Values("Content-Disposition"), fileHeader.Filename) ||
		!strings.EqualFold(path.Ext(filename), ".json") ||
		fileHeader.Size < 1 || fileHeader.Size > maxJSONGitRemoteFileBytes {
		writeJSONGitRemoteJSON(w, http.StatusBadRequest, jsonGitRemoteError("Choose one nonempty .json file no larger than 1 MiB."))
		return
	}

	file, err := fileHeader.Open()
	if err != nil {
		writeJSONGitRemoteJSON(w, http.StatusBadRequest, jsonGitRemoteError("The selected JSON file could not be read."))
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxJSONGitRemoteFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxJSONGitRemoteFileBytes {
		writeJSONGitRemoteJSON(w, http.StatusBadRequest, jsonGitRemoteError("The selected JSON file could not be read within the 1 MiB limit."))
		return
	}

	remotes, err := parseSelectedJSONGitRemotes(data)
	if err != nil {
		writeJSONGitRemoteJSON(w, http.StatusBadRequest, jsonGitRemoteError("The selected JSON file is malformed or exceeds preview limits."))
		return
	}
	// Active settings are read only after the complete JSON document passed parsing.
	cfg, err := readConfig(a.configPath)
	if err != nil {
		writeJSONGitRemoteJSON(w, http.StatusInternalServerError, jsonGitRemoteError("Registered targets could not be read."))
		return
	}

	preview := jsonGitRemotePreview{Candidates: make([]jsonGitRemotePreviewCandidate, 0, len(remotes))}
	for _, remote := range remotes {
		registeredMatch := false
		for _, target := range cfg.GitHubTargets {
			targetOrigin, originErr := normalizeJSONGitRemoteOrigin(target.Origin)
			if !target.Disabled && originErr == nil && targetOrigin == remote.Origin && target.Repository == remote.Repository {
				registeredMatch = true
				break
			}
		}
		preview.Candidates = append(preview.Candidates, jsonGitRemotePreviewCandidate{
			Source:          jsonGitRemotePreviewSource,
			Repository:      cleanOutput(remote.Repository, "", 400),
			Confidence:      "low",
			RegisteredMatch: registeredMatch,
		})
	}
	writeJSONGitRemoteJSON(w, http.StatusOK, preview)
}

func isExactJSONGitRemoteFilePart(dispositions []string, normalizedFilename string) bool {
	if len(dispositions) != 1 {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(dispositions[0])
	return err == nil && strings.EqualFold(mediaType, "form-data") && len(params) == 2 &&
		params["name"] == "json_remotes" && params["filename"] == normalizedFilename
}

func parseSelectedJSONGitRemotes(data []byte) ([]parsedJSONGitRemote, error) {
	if len(data) == 0 || len(data) > maxJSONGitRemoteFileBytes || !utf8.Valid(data) {
		return nil, errors.New("invalid JSON remote file")
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	parser := jsonGitRemoteParser{decoder: decoder, seen: make(map[string]struct{})}
	if err := parser.parseValue(0); err != nil {
		return nil, errors.New("invalid JSON remote file")
	}
	if _, err := parser.token(); err != io.EOF {
		return nil, errors.New("trailing JSON remote data")
	}
	return parser.remotes, nil
}

type jsonGitRemoteParser struct {
	decoder *json.Decoder
	tokens  int
	remotes []parsedJSONGitRemote
	seen    map[string]struct{}
}

func (p *jsonGitRemoteParser) token() (any, error) {
	token, err := p.decoder.Token()
	if err != nil {
		return nil, err
	}
	p.tokens++
	if p.tokens > maxJSONGitRemoteTokens {
		return nil, errors.New("JSON token limit exceeded")
	}
	return token, nil
}

func (p *jsonGitRemoteParser) parseValue(depth int) error {
	token, err := p.token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if isDelimiter {
		switch delimiter {
		case '{':
			if depth >= maxJSONGitRemoteDepth {
				return errors.New("JSON nesting limit exceeded")
			}
			keys := make(map[string]struct{})
			for p.decoder.More() {
				keyToken, err := p.token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("invalid JSON object key")
				}
				if _, duplicate := keys[key]; duplicate {
					return errors.New("duplicate JSON object key")
				}
				keys[key] = struct{}{}
				if err := p.parseValue(depth + 1); err != nil {
					return err
				}
			}
			closing, err := p.token()
			if err != nil || closing != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
			return nil
		case '[':
			if depth >= maxJSONGitRemoteDepth {
				return errors.New("JSON nesting limit exceeded")
			}
			for p.decoder.More() {
				if err := p.parseValue(depth + 1); err != nil {
					return err
				}
			}
			closing, err := p.token()
			if err != nil || closing != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
			return nil
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}

	value, isString := token.(string)
	if !isString {
		return nil
	}
	if len(value) > maxJSONGitRemoteStringBytes {
		return nil
	}
	origin, repository, err := parseSafeGitRemoteURL(value)
	if err != nil {
		return nil
	}
	// DNS host names are case-insensitive. The repository path remains
	// case-sensitive and parseSafeGitRemoteURL removes a .git suffix.
	origin, err = normalizeJSONGitRemoteOrigin(origin)
	if err != nil {
		return errors.New("invalid normalized remote origin")
	}
	key := origin + "\x00" + repository
	if _, duplicate := p.seen[key]; duplicate {
		return nil
	}
	if len(p.remotes) == maxJSONGitRemoteCandidates {
		return errors.New("too many JSON remote candidates")
	}
	p.seen[key] = struct{}{}
	p.remotes = append(p.remotes, parsedJSONGitRemote{Origin: origin, Repository: repository})
	return nil
}

func normalizeJSONGitRemoteOrigin(raw string) (string, error) {
	normalized, err := validateOrigin(raw)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return "", errors.New("invalid remote origin")
	}
	hostname := strings.ToLower(parsed.Hostname())
	host := hostname
	if strings.Contains(hostname, ":") {
		host = "[" + hostname + "]"
	}
	if port := parsed.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil {
			return "", errors.New("invalid remote port")
		}
		if portNumber != 443 {
			host = net.JoinHostPort(hostname, strconv.Itoa(portNumber))
		}
	}
	return "https://" + host, nil
}

func writeJSONGitRemoteJSON(w http.ResponseWriter, status int, value jsonGitRemotePreview) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Preview could not be encoded."}`)
	}
	if len(data)+1 > maxJSONGitRemoteResponseBytes {
		status = http.StatusRequestEntityTooLarge
		data = []byte(`{"error":"Preview exceeds the 64 KiB response limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func jsonGitRemoteError(message string) jsonGitRemotePreview {
	return jsonGitRemotePreview{Candidates: []jsonGitRemotePreviewCandidate{}, Error: message}
}
