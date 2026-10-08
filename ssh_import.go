package main

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxSSHConfigFileBytes     = 64 << 10
	maxSSHConfigRequestBytes  = maxSSHConfigFileBytes + (8 << 10)
	maxSSHConfigResponseBytes = 64 << 10
	maxSSHConfigLines         = 512
	maxSSHConfigAliases       = 32
)

const sshConfigPreviewSource = "selected SSH config file; actual path unverified"

var errUnsupportedSSHConfig = errors.New("unsupported SSH config")

type sshConfigPreviewAlias struct {
	Name string `json:"name"`
}

type sshConfigPreview struct {
	Source  string                  `json:"source,omitempty"`
	Aliases []sshConfigPreviewAlias `json:"aliases,omitempty"`
	Error   string                  `json:"error,omitempty"`
}

type sshConfigPreviewSuccess struct {
	Source  string                  `json:"source"`
	Aliases []sshConfigPreviewAlias `json:"aliases"`
}

type sshConfigPreviewFailure struct {
	Error string `json:"error"`
}

// handleSSHConfigPreview previews literal Host aliases from one user-selected
// config file. It deliberately does not read application config or contact any
// registered target.
func (a *app) handleSSHConfigPreview(w http.ResponseWriter, r *http.Request) {
	checkResponse := newPostResponseCapture()
	postValid := a.checkPostLimit(checkResponse, r, maxSSHConfigRequestBytes)
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
		writeSSHConfigJSON(w, status, sshConfigPreview{Error: "Request rejected."})
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File) != 1 ||
		len(r.MultipartForm.File["ssh_config"]) != 1 ||
		len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.Value["csrf"]) != 1 {
		writeSSHConfigJSON(w, http.StatusBadRequest, sshConfigPreview{Error: "Choose one file named config for preview."})
		return
	}

	fileHeader := r.MultipartForm.File["ssh_config"][0]
	if !isExactSSHConfigFilePart(fileHeader.Header.Values("Content-Disposition"), fileHeader.Filename) || fileHeader.Size < 1 || fileHeader.Size > maxSSHConfigFileBytes {
		writeSSHConfigJSON(w, http.StatusBadRequest, sshConfigPreview{Error: "Choose one nonempty file named config no larger than 64 KiB."})
		return
	}
	file, err := fileHeader.Open()
	if err != nil {
		writeSSHConfigJSON(w, http.StatusBadRequest, sshConfigPreview{Error: "The selected config file could not be read."})
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSSHConfigFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxSSHConfigFileBytes {
		writeSSHConfigJSON(w, http.StatusBadRequest, sshConfigPreview{Error: "The selected config file could not be read within the 64 KiB limit."})
		return
	}

	aliases, err := parseSelectedSSHConfig(data)
	if err != nil {
		writeSSHConfigJSON(w, http.StatusBadRequest, sshConfigPreview{Error: "The selected config contains unsupported or unsafe SSH directives."})
		return
	}
	preview := sshConfigPreview{Source: sshConfigPreviewSource, Aliases: make([]sshConfigPreviewAlias, 0, len(aliases))}
	for _, alias := range aliases {
		preview.Aliases = append(preview.Aliases, sshConfigPreviewAlias{Name: alias})
	}
	writeSSHConfigJSON(w, http.StatusOK, preview)
}

func isExactSSHConfigFilePart(dispositions []string, normalizedFilename string) bool {
	return len(dispositions) == 1 && isExactSSHConfigDisposition(dispositions[0], normalizedFilename)
}

func isExactSSHConfigDisposition(disposition, normalizedFilename string) bool {
	mediaType, params, err := mime.ParseMediaType(disposition)
	return err == nil && strings.EqualFold(mediaType, "form-data") && len(params) == 2 &&
		params["name"] == "ssh_config" && params["filename"] == "config" && normalizedFilename == "config"
}

// checkPostLimit writes plain-text errors itself. Capture those writes so this
// endpoint can keep the shared method, body-size, multipart, and CSRF checks
// while returning JSON for the checks handled here.
type postResponseCapture struct {
	header http.Header
	status int
}

func newPostResponseCapture() *postResponseCapture {
	return &postResponseCapture{header: make(http.Header)}
}

func (c *postResponseCapture) Header() http.Header { return c.header }

func (c *postResponseCapture) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

func (c *postResponseCapture) Write(data []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return len(data), nil
}

func writeSSHConfigJSON(w http.ResponseWriter, status int, value sshConfigPreview) {
	var data []byte
	var err error
	if value.Error != "" {
		data, err = json.Marshal(sshConfigPreviewFailure{Error: value.Error})
	} else {
		data, err = json.Marshal(sshConfigPreviewSuccess{Source: value.Source, Aliases: value.Aliases})
	}
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"Preview could not be encoded."}`)
	}
	if len(data)+1 > maxSSHConfigResponseBytes {
		status = http.StatusRequestEntityTooLarge
		data = []byte(`{"error":"Preview was not returned because it exceeds the 64 KiB response limit."}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// parseSelectedSSHConfig accepts only literal Host aliases and literal
// HostName values. Every other directive is treated as opaque text and never
// interpreted or returned.
func parseSelectedSSHConfig(data []byte) ([]string, error) {
	if len(data) == 0 || len(data) > maxSSHConfigFileBytes || !utf8.Valid(data) {
		return nil, errUnsupportedSSHConfig
	}
	text := string(data)
	if strings.ContainsRune(text, '\uFEFF') {
		return nil, errUnsupportedSSHConfig
	}
	for index, r := range text {
		if r == '\r' {
			if index+1 >= len(text) || text[index+1] != '\n' {
				return nil, errUnsupportedSSHConfig
			}
			continue
		}
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return nil, errUnsupportedSSHConfig
		}
	}

	lineCount := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		lineCount++
	}
	if lineCount > maxSSHConfigLines {
		return nil, errUnsupportedSSHConfig
	}

	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	aliases := make([]string, 0, 4)
	seenAliases := make(map[string]struct{})
	inHostBlock := false
	hostNameSeen := false
	for _, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		line = stripSSHInlineComment(line)
		code := strings.Trim(line, " \t")
		if code == "" {
			continue
		}
		if strings.HasSuffix(strings.TrimRight(code, " \t"), "\\") {
			return nil, errUnsupportedSSHConfig
		}
		fields := splitSSHFields(code)
		if len(fields) == 0 {
			continue
		}
		key, args := parseSSHDirective(fields)
		switch strings.ToLower(key) {
		case "include", "match":
			return nil, errUnsupportedSSHConfig
		case "host":
			if len(args) == 0 || len(aliases)+len(args) > maxSSHConfigAliases {
				return nil, errUnsupportedSSHConfig
			}
			for _, alias := range args {
				if !isSafeSSHConfigAlias(alias) || cleanOutput(alias, "", 80) != alias {
					return nil, errUnsupportedSSHConfig
				}
				key := strings.ToLower(alias)
				if _, exists := seenAliases[key]; exists {
					return nil, errUnsupportedSSHConfig
				}
				seenAliases[key] = struct{}{}
				aliases = append(aliases, alias)
			}
			inHostBlock = true
			hostNameSeen = false
		case "hostname":
			if !inHostBlock || hostNameSeen || len(args) != 1 || !isLiteralSSHHostName(args[0]) {
				return nil, errUnsupportedSSHConfig
			}
			hostNameSeen = true
		}
	}
	return aliases, nil
}

func stripSSHInlineComment(line string) string {
	for index := 0; index < len(line); index++ {
		if line[index] == '#' && (index == 0 || line[index-1] == ' ' || line[index-1] == '\t') {
			return line[:index]
		}
	}
	return line
}

func splitSSHFields(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
}

func parseSSHDirective(fields []string) (string, []string) {
	first := fields[0]
	if equal := strings.IndexByte(first, '='); equal >= 0 {
		key := first[:equal]
		args := make([]string, 0, len(fields))
		if value := first[equal+1:]; value != "" {
			args = append(args, value)
		}
		args = append(args, fields[1:]...)
		return key, args
	}
	args := append([]string(nil), fields[1:]...)
	if len(args) > 0 && args[0] == "=" {
		args = args[1:]
	} else if len(args) > 0 && strings.HasPrefix(args[0], "=") {
		args[0] = strings.TrimPrefix(args[0], "=")
	}
	return first, args
}

func isSafeSSHConfigAlias(alias string) bool {
	if len(alias) < 1 || len(alias) > 80 || !isASCIIAlphaNumeric(alias[0]) {
		return false
	}
	for index := 1; index < len(alias); index++ {
		character := alias[index]
		if !isASCIIAlphaNumeric(character) && character != '.' && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9'
}

func isLiteralSSHHostName(host string) bool {
	if net.ParseIP(host) != nil {
		return true
	}
	if len(host) == 0 || len(host) > 254 {
		return false
	}
	name := strings.TrimSuffix(host, ".")
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || !isASCIIAlphaNumeric(label[0]) || !isASCIIAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for index := 1; index < len(label)-1; index++ {
			if !isASCIIAlphaNumeric(label[index]) && label[index] != '-' {
				return false
			}
		}
	}
	return true
}
