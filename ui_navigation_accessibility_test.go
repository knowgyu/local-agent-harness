package main

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestUIShellNoScriptLeavesSettingsFormsReachable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, csrf: "ui-navigation-test-csrf"}
	page := uiNavigationRenderRootPage(t, a)

	navTag := uiNavigationOpeningTagByID(t, page, "lah-shell-nav")
	if !strings.HasPrefix(strings.ToLower(navTag), "<nav ") || !uiNavigationHasBooleanAttribute(navTag, "hidden") {
		t.Fatalf("progressive-enhancement navigation is not initially hidden: %s", navTag)
	}
	shellTag := uiNavigationOpeningTagByID(t, page, "lah-shell")
	if !uiNavigationHasBooleanAttribute(shellTag, "hidden") {
		t.Fatalf("navigation shell should remain inactive until enhancement initializes: %s", shellTag)
	}
	if strings.Contains(strings.ToLower(navTag), "role=\"tablist\"") || strings.Contains(strings.ToLower(navTag), "role=\"tab\"") {
		t.Fatalf("primary navigation must retain native link semantics, not ARIA tabs: %s", navTag)
	}

	bodyTag := uiNavigationFirstOpeningTag(t, page, "body")
	if uiNavigationHasBooleanAttribute(bodyTag, "hidden") || strings.Contains(strings.ToLower(bodyTag), "display:none") {
		t.Fatalf("page content is hidden before scripts initialize: %s", bodyTag)
	}
	for _, id := range []string{
		"lah-secrets-environment",
		"lah-named-secret-form",
		"lah-user-environment-form",
		"dashboard-diagnosis-form",
		"ssh-settings-entry",
	} {
		tag := uiNavigationOpeningTagByID(t, page, id)
		if uiNavigationHasBooleanAttribute(tag, "hidden") {
			t.Errorf("settings content %q is hidden before JavaScript runs: %s", id, tag)
		}
	}
	for _, secretRef := range []string{
		"cred:0123456789abcdef0123456789abcdef",
		"cred:1123456789abcdef0123456789abcdef",
		"cred:2123456789abcdef0123456789abcdef",
		"cred:3123456789abcdef0123456789abcdef",
		"cred:4123456789abcdef0123456789abcdef",
	} {
		if strings.Contains(page, secretRef) {
			t.Fatal("server-rendered settings page exposed a synthetic credential reference")
		}
	}
}

func TestUIShellNavigationMarkupHasNamedNativeDestinations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(path, serviceBundleConfig()); err != nil {
		t.Fatal(err)
	}
	page := uiNavigationRenderRootPage(t, &app{configPath: path, csrf: "ui-navigation-test-csrf"})
	nav := uiNavigationElementByID(t, page, "lah-shell-nav", "nav")
	anchors := regexp.MustCompile("(?is)<a\\b[^>]*\\bdata-pane-link(?:\\s|=)[^>]*>.*?</a>").FindAllString(nav, -1)
	if len(anchors) == 0 {
		t.Fatal("shell navigation has no destinations")
	}

	seen := make(map[string]bool, len(anchors))
	currentLinks := 0
	for _, anchor := range anchors {
		openEnd := strings.Index(anchor, ">")
		if openEnd < 0 {
			t.Fatalf("navigation anchor has no opening-tag boundary: %s", anchor)
		}
		openTag := anchor[:openEnd+1]
		href := uiNavigationAttributeValue(openTag, "href")
		if !strings.HasPrefix(href, "#") || len(href) == 1 {
			t.Errorf("navigation destination %q is not a same-page hash link", href)
		}
		if seen[href] {
			t.Errorf("navigation has duplicate destination %q", href)
		}
		seen[href] = true
		if uiNavigationAccessibleName(anchor) == "" {
			t.Errorf("navigation link %q has no accessible name", href)
		}
		if strings.EqualFold(uiNavigationAttributeValue(openTag, "aria-current"), "page") {
			currentLinks++
		}
		if strings.Contains(strings.ToLower(openTag), "role=\"tab\"") ||
			regexp.MustCompile("(?i)\\btabindex=[\"']-1[\"']").MatchString(openTag) ||
			regexp.MustCompile("(?i)\\btabindex=[\"']?[1-9][0-9]*[\"']?").MatchString(openTag) {
			t.Errorf("navigation link %q is removed from native sequential keyboard navigation: %s", href, openTag)
		}
		targetID := strings.TrimPrefix(href, "#")
		if regexp.MustCompile("(?i)\\bid=[\"']"+regexp.QuoteMeta(targetID)+"[\"']").FindStringIndex(page) == nil {
			t.Errorf("navigation link %q points to a missing page target", href)
		}
	}
	if currentLinks > 1 {
		t.Errorf("server-rendered navigation marks %d links as the current page", currentLinks)
	}

	for _, href := range []string{
		"#lah-pane-connections",
		"#lah-pane-groups",
		"#lah-pane-secrets",
		"#lah-pane-environment",
		"#lah-pane-diagnosis",
		"#lah-pane-ssh",
		"#lah-pane-draft",
		"#lah-pane-clients",
		"#lah-pane-tasks",
		"#lah-pane-advanced",
	} {
		if !seen[href] {
			t.Errorf("navigation does not expose required direct destination %q", href)
		}
	}

	paneKeys := []string{"connections", "groups", "secrets", "environment", "ssh", "draft", "clients", "tasks", "advanced", "diagnosis"}
	paneCounts := make(map[string]int, len(paneKeys))
	contentCounts := make(map[string]int, len(paneKeys))
	for _, match := range regexp.MustCompile(`(?is)<section\b[^>]*\bclass=["'][^"']*\blah-shell-pane\b[^"']*["'][^>]*>`).FindAllString(page, -1) {
		key := uiNavigationAttributeValue(match, "data-pane")
		paneCounts[key]++
		if uiNavigationAttributeValue(match, "id") != "lah-pane-"+key {
			t.Errorf("pane %q id does not match its navigation key: %s", key, match)
		}
	}
	for _, match := range regexp.MustCompile(`(?is)\bdata-pane-content=["']([^"']+)["']`).FindAllStringSubmatch(page, -1) {
		if len(match) == 2 {
			contentCounts[match[1]]++
		}
	}
	for _, key := range paneKeys {
		if paneCounts[key] != 1 || contentCounts[key] != 1 {
			t.Errorf("pane %q must have exactly one pane and one content destination; panes=%d contents=%d", key, paneCounts[key], contentCounts[key])
		}
	}

	panes := regexp.MustCompile("(?is)<section\\b[^>]*\\bclass=[\"'][^\"']*\\blah-shell-pane\\b[^\"']*[\"'][^>]*>").FindAllString(page, -1)
	if len(panes) < 5 {
		t.Fatalf("shell has %d named pane sections", len(panes))
	}
	for _, pane := range panes {
		labelledBy := strings.Fields(uiNavigationAttributeValue(pane, "aria-labelledby"))
		if len(labelledBy) == 0 {
			t.Errorf("shell pane has no accessible name: %s", pane)
			continue
		}
		heading := uiNavigationOpeningTagByID(t, page, labelledBy[0])
		if !strings.HasPrefix(strings.ToLower(heading), "<h2 ") || uiNavigationAttributeValue(heading, "tabindex") != "-1" {
			t.Errorf("shell pane heading %q must be an h2 focus destination: %s", labelledBy[0], heading)
		}
		headingText := uiNavigationElementByID(t, page, labelledBy[0], "h2")
		if uiNavigationTextContent(headingText) == "" && uiNavigationAttributeValue(heading, "aria-label") == "" {
			t.Errorf("shell pane heading %q has no accessible name", labelledBy[0])
		}
	}

	skipLink := regexp.MustCompile("(?is)<a\\b[^>]*\\bclass=[\"'][^\"']*\\bskip\\b[^\"']*[\"'][^>]*>.*?</a>").FindString(page)
	if skipLink == "" || uiNavigationAccessibleName(skipLink) == "" ||
		uiNavigationAttributeValue(skipLink[:strings.Index(skipLink, ">")+1], "href") != "#connections" {
		t.Fatal("navigation-heavy page does not preserve its named skip-to-main link")
	}
}

func TestUILanguageSwitchPreservesDirtyFormValues(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js unavailable; skipping browserless language-switch state fixture")
	}

	page, err := os.ReadFile("ui.html")
	if err != nil {
		t.Fatal(err)
	}
	pageText := string(page)
	dictionary := uiNavigationExtractSection(t, pageText,
		"      const koText = {",
		"\n      };",
		true,
	)
	constraintHandlers := uiNavigationExtractSection(t, pageText,
		"      function localizedConstraintMessage(control)",
		"      function clearDynamicChildren",
		false,
	)
	applyLanguage := uiNavigationExtractSection(t, pageText,
		"      function applyLanguage(root = document.documentElement)",
		"      languageSelect.addEventListener('change'",
		false,
	)
	languageChange := uiNavigationExtractSection(t, pageText,
		"      languageSelect.addEventListener('change'",
		"      for (const client of clientProgressClients)",
		false,
	)

	fixture := strings.Join([]string{
		"const NodeFilter = {SHOW_TEXT: 1};",
		"let language = 'en';",
		"const originalText = new WeakMap();",
		"const originalAria = new WeakMap();",
		"const disabledOptionNames = new WeakMap();",
		"const dynamicNodes = new Map();",
		"const localizedConstraintControls = new WeakSet();",
		"const listeners = new Map();",
		"const languageSelect = {value: 'en', ariaLabel: 'Language', dataset: {}, isConnected: true,",
		"  addEventListener(name, callback) { listeners.set(name, callback); },",
		"  matches() { return false; },",
		"  getAttribute(name) { return name === 'aria-label' ? this.ariaLabel : null; },",
		"  setAttribute(name, value) { if (name === 'aria-label') this.ariaLabel = value; }};",
		"const nameInput = {value: 'unsaved dashboard label', type: 'text',",
		"  validity: {}, listeners: new Map(),",
		"  addEventListener(name, callback) { this.listeners.set(name, callback); },",
		"  setCustomValidity() {}};",
		"const targetSelect = {value: 'registered-target-7', type: 'select-one',",
		"  validity: {}, selectedIndex: 2, listeners: new Map(),",
		"  addEventListener(name, callback) { this.listeners.set(name, callback); },",
		"  setCustomValidity() {}};",
		"const labelText = {textContent: 'Language', parentElement: {closest() { return null; }}, isConnected: true};",
		"const root = {lang: 'en', nodes: [labelText],",
		"  querySelectorAll(selector) {",
		"    if (selector === 'input, select, textarea') return [nameInput, targetSelect];",
		"    if (selector === '[aria-label]') return [languageSelect];",
		"    return []; }};",
		"const document = {documentElement: root, cookie: 'lah_lang=en',",
		"  createTreeWalker(treeRoot) { let index = 0; return {nextNode() { return treeRoot.nodes[index++] || null; }}; }};",
		"const localStorage = {values: new Map(), setItem(key, value) { this.values.set(key, value); }};",
		"function translated(key) { return key; }",
	}, "\n")
	checks := strings.Join([]string{
		"languageSelect.value = 'ko';",
		"const onLanguageChange = listeners.get('change');",
		"if (typeof onLanguageChange !== 'function') throw new Error('language change handler was not registered');",
		"onLanguageChange();",
		"if (labelText.textContent !== '언어' || root.lang !== 'ko') throw new Error('Korean language state was not applied');",
		"if (nameInput.value !== 'unsaved dashboard label') throw new Error('language switch lost dirty text input');",
		"if (targetSelect.value !== 'registered-target-7' || targetSelect.selectedIndex !== 2) throw new Error('language switch changed selected registered target');",
		"if (document.cookie !== 'lah_lang=ko; Path=/; SameSite=Strict' || localStorage.values.get('lah-lang') !== 'ko') throw new Error('language choice was not persisted through the normal UI path');",
		"languageSelect.value = 'en';",
		"onLanguageChange();",
		"if (labelText.textContent !== 'Language' || root.lang !== 'en') throw new Error('English language state was not restored');",
		"if (nameInput.value !== 'unsaved dashboard label' || targetSelect.value !== 'registered-target-7' || targetSelect.selectedIndex !== 2) throw new Error('switching back lost the dirty form state');",
	}, "\n")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, node, "-")
	command.Stdin = strings.NewReader(dictionary + fixture + constraintHandlers + applyLanguage + languageChange + checks)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("production language-switch behavior fixture failed: %v\n%s", err, output)
	}
}

func uiNavigationRenderRootPage(t *testing.T, a *app) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	a.handleRoot(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("root UI response status = %d, want %d", response.Code, http.StatusOK)
	}
	return response.Body.String()
}

func uiNavigationOpeningTagByID(t *testing.T, source, id string) string {
	t.Helper()
	marker := "id=\"" + id + "\""
	index := strings.Index(source, marker)
	if index < 0 {
		t.Fatalf("rendered UI is missing element id %q", id)
	}
	return uiNavigationOpeningTagAt(t, source, index)
}

func uiNavigationElementByID(t *testing.T, source, id, tagName string) string {
	t.Helper()
	openTag := uiNavigationOpeningTagByID(t, source, id)
	if !strings.HasPrefix(strings.ToLower(openTag), "<"+strings.ToLower(tagName)+" ") {
		t.Fatalf("element %q uses %q, want <%s>", id, openTag, tagName)
	}
	start := strings.Index(source, openTag)
	closeTag := "</" + tagName + ">"
	closeOffset := strings.Index(strings.ToLower(source[start:]), closeTag)
	if closeOffset < 0 {
		t.Fatalf("element %q has no closing </%s>", id, tagName)
	}
	return source[start : start+closeOffset+len(closeTag)]
}

func uiNavigationFirstOpeningTag(t *testing.T, source, name string) string {
	t.Helper()
	marker := "<" + name
	index := strings.Index(strings.ToLower(source), marker)
	if index < 0 {
		t.Fatalf("rendered UI is missing opening <%s> tag", name)
	}
	return uiNavigationOpeningTagAt(t, source, index+1)
}

func uiNavigationOpeningTagAt(t *testing.T, source string, index int) string {
	t.Helper()
	start := strings.LastIndex(source[:index], "<")
	if start < 0 {
		t.Fatalf("could not locate opening tag before byte %d", index)
	}
	endOffset := strings.Index(source[index:], ">")
	if endOffset < 0 {
		t.Fatalf("opening tag at byte %d has no closing angle bracket", index)
	}
	return source[start : index+endOffset+1]
}

func uiNavigationHasBooleanAttribute(tag, name string) bool {
	pattern := regexp.MustCompile("(?i)(?:^|\\s)" + regexp.QuoteMeta(name) + "(?:\\s|=|/?>)")
	return pattern.MatchString(tag)
}

func uiNavigationAttributeValue(tag, name string) string {
	pattern := regexp.MustCompile("(?i)\\b" + regexp.QuoteMeta(name) + "=[\"']([^\"']*)[\"']")
	match := pattern.FindStringSubmatch(tag)
	if len(match) != 2 {
		return ""
	}
	return html.UnescapeString(match[1])
}

func uiNavigationAccessibleName(anchor string) string {
	label := regexp.MustCompile("(?is)\\baria-label=[\"']([^\"']+)[\"']").FindStringSubmatch(anchor)
	if len(label) == 2 && strings.TrimSpace(html.UnescapeString(label[1])) != "" {
		return strings.TrimSpace(html.UnescapeString(label[1]))
	}
	openEnd := strings.Index(anchor, ">")
	closeStart := strings.LastIndex(strings.ToLower(anchor), "</a>")
	if openEnd < 0 || closeStart <= openEnd {
		return ""
	}
	return uiNavigationTextContent(anchor[openEnd+1 : closeStart])
}

func uiNavigationTextContent(fragment string) string {
	text := regexp.MustCompile("(?is)<[^>]*>").ReplaceAllString(fragment, " ")
	text = html.UnescapeString(text)
	return strings.Join(strings.Fields(text), " ")
}

func uiNavigationExtractSection(t *testing.T, source, startMarker, endMarker string, includeEnd bool) string {
	t.Helper()
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("UI source is missing section marker %q", startMarker)
	}
	endOffset := strings.Index(source[start:], endMarker)
	if endOffset < 0 {
		t.Fatalf("UI source is missing section end marker %q", endMarker)
	}
	if includeEnd {
		endOffset += len(endMarker)
	}
	return source[start : start+endOffset]
}
