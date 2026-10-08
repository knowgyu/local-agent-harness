package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	maxRunbookFileBytes     = 64 << 10
	maxRunbookRequestBytes  = maxRunbookFileBytes + (8 << 10)
	maxRunbookResponseBytes = 64 << 10
	maxRunbookLines         = 512
	maxRunbookReferences    = 32
)

const runbookRepositoryDirective = "lah:repository"

type parsedRunbookReference struct {
	origin      string
	repository  string
	occurrences int
}

type runbookImportCandidate struct {
	Source           string            `json:"source"`
	Repository       string            `json:"repository"`
	Confidence       string            `json:"confidence"`
	Occurrences      int               `json:"occurrences"`
	GitHubMatches    []gitImportTarget `json:"github_matches"`
	MatchesTruncated bool              `json:"matches_truncated"`
	Unverified       []string          `json:"unverified"`
}

type runbookImportPreview struct {
	Candidates []runbookImportCandidate `json:"candidates"`
	Error      string                   `json:"error,omitempty"`
}

func (a *app) handleRunbookPreview(w http.ResponseWriter, r *http.Request) {
	postValid := a.checkPostLimit(w, r, maxRunbookRequestBytes)
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !postValid {
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 ||
		len(r.MultipartForm.File["runbook"]) != 1 ||
		len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["csrf"]) != 1 {
		writeRunbookImportError(w, http.StatusBadRequest, "Choose one Markdown or text file for preview.")
		return
	}
	fileHeader := r.MultipartForm.File["runbook"][0]
	filename := strings.ReplaceAll(fileHeader.Filename, "\\", "/")
	extension := path.Ext(filename)
	if (!strings.EqualFold(extension, ".md") && !strings.EqualFold(extension, ".txt")) || fileHeader.Size < 1 || fileHeader.Size > maxRunbookFileBytes {
		writeRunbookImportError(w, http.StatusBadRequest, "Choose one Markdown or text file no larger than 64 KiB.")
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeRunbookImportError(w, http.StatusBadRequest, "The selected document could not be read.")
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxRunbookFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxRunbookFileBytes {
		writeRunbookImportError(w, http.StatusBadRequest, "The selected document could not be read within the 64 KiB limit.")
		return
	}
	references, err := parseSelectedRunbook(data)
	if err != nil {
		writeRunbookImportError(w, http.StatusBadRequest, "The selected document contains an unsupported or unsafe repository reference.")
		return
	}
	cfg, err := readConfig(a.configPath)
	if err != nil {
		writeRunbookImportError(w, http.StatusInternalServerError, "Registered targets could not be read.")
		return
	}

	preview := runbookImportPreview{Candidates: make([]runbookImportCandidate, 0, len(references))}
	for _, reference := range references {
		candidate := runbookImportCandidate{
			Source:        "selected document file; actual path unverified",
			Repository:    reference.repository,
			Confidence:    "medium",
			Occurrences:   reference.occurrences,
			GitHubMatches: []gitImportTarget{},
			Unverified:    []string{"Jenkins", "Harbor", "Dashboard", "permissions"},
		}
		for _, target := range cfg.GitHubTargets {
			if target.Disabled || target.Origin != reference.origin || target.Repository != reference.repository {
				continue
			}
			if len(candidate.GitHubMatches) == maxGitImportMatchesPerRemote {
				candidate.MatchesTruncated = true
				break
			}
			candidate.GitHubMatches = append(
				candidate.GitHubMatches,
				gitImportTarget{ID: target.ID, Name: cleanOutput(target.Name, "", 80)},
			)
		}
		if candidate.MatchesTruncated || len(candidate.GitHubMatches) > 1 {
			candidate.Confidence = "low"
		} else if len(candidate.GitHubMatches) == 1 {
			candidate.Confidence = "high"
		}
		preview.Candidates = append(preview.Candidates, candidate)
	}
	writeRunbookImportJSON(w, http.StatusOK, preview)
}

func parseSelectedRunbook(data []byte) ([]parsedRunbookReference, error) {
	if len(data) == 0 || len(data) > maxRunbookFileBytes || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
		return nil, errors.New("unsupported runbook")
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > maxRunbookLines {
		return nil, errors.New("too many runbook lines")
	}
	references := make([]parsedRunbookReference, 0, 4)
	byRepository := make(map[string]int)
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		if strings.ContainsRune(line, '\r') {
			return nil, errors.New("unsupported runbook line ending")
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, runbookRepositoryDirective) {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) != 2 || fields[0] != runbookRepositoryDirective {
			return nil, errors.New("invalid repository directive")
		}
		origin, repository, err := parseSafeGitRemoteURL(fields[1])
		if err != nil {
			return nil, errors.New("unsafe repository reference")
		}
		if cleanOutput(repository, "", 400) != repository {
			return nil, errors.New("sensitive repository reference")
		}
		key := origin + "\x00" + repository
		if index, exists := byRepository[key]; exists {
			references[index].occurrences++
			continue
		}
		if len(references) == maxRunbookReferences {
			return nil, errors.New("too many repository references")
		}
		byRepository[key] = len(references)
		references = append(references, parsedRunbookReference{origin: origin, repository: repository, occurrences: 1})
	}
	return references, nil
}

func writeRunbookImportError(w http.ResponseWriter, status int, message string) {
	writeRunbookImportJSON(w, status, runbookImportPreview{Error: message})
}

func writeRunbookImportJSON(w http.ResponseWriter, status int, value runbookImportPreview) {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Preview could not be encoded."}`)
	}
	if len(data)+1 > maxRunbookResponseBytes {
		status = http.StatusRequestEntityTooLarge
		data = []byte(`{"error":"Preview was not returned because it exceeds the 64 KiB response limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}
