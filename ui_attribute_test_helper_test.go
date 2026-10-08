package main

import (
	"errors"
	"strings"
	"testing"
)

const maxValidationHTMLBytes = 1 << 20

type htmlValidationAttributeCounts struct {
	autofocusElements       int
	ariaInvalidTrueElements int
}

func countHTMLValidationAttributes(source string) (htmlValidationAttributeCounts, error) {
	if len(source) > maxValidationHTMLBytes {
		return htmlValidationAttributeCounts{}, errors.New("HTML exceeds validation test helper size limit")
	}

	var counts htmlValidationAttributeCounts
	for offset := 0; offset < len(source); {
		relativeStart := strings.IndexByte(source[offset:], '<')
		if relativeStart < 0 {
			break
		}
		start := offset + relativeStart

		if strings.HasPrefix(source[start:], "<!--") {
			commentEnd := strings.Index(source[start+4:], "-->")
			if commentEnd < 0 {
				return htmlValidationAttributeCounts{}, errors.New("unterminated HTML comment")
			}
			offset = start + 4 + commentEnd + len("-->")
			continue
		}

		if start+1 >= len(source) {
			break
		}
		if source[start+1] == '!' || source[start+1] == '?' || source[start+1] == '/' {
			next, err := skipHTMLMarkup(source, start+1)
			if err != nil {
				return htmlValidationAttributeCounts{}, err
			}
			offset = next
			continue
		}
		if !isHTMLASCIIAlpha(source[start+1]) {
			offset = start + 1
			continue
		}

		tagStart := start + 1
		position := tagStart
		for position < len(source) && !isHTMLTagNameEnd(source[position]) {
			position++
		}
		if position == tagStart {
			offset = start + 1
			continue
		}
		tagName := source[tagStart:position]
		hasAutofocus := false
		hasAriaInvalidTrue := false
		closed := false

		for position < len(source) {
			for position < len(source) && isHTMLSpace(source[position]) {
				position++
			}
			if position >= len(source) {
				break
			}
			if source[position] == '>' {
				position++
				closed = true
				break
			}
			if source[position] == '/' {
				position++
				continue
			}

			attributeStart := position
			for position < len(source) && !isHTMLAttributeNameEnd(source[position]) {
				position++
			}
			if position == attributeStart {
				position++
				continue
			}
			attributeName := source[attributeStart:position]
			if strings.EqualFold(attributeName, "autofocus") {
				hasAutofocus = true
			}

			for position < len(source) && isHTMLSpace(source[position]) {
				position++
			}
			if position >= len(source) || source[position] != '=' {
				continue
			}
			position++
			for position < len(source) && isHTMLSpace(source[position]) {
				position++
			}
			if position >= len(source) {
				break
			}

			quote := source[position]
			attributeValue := ""
			if quote == '\'' || quote == '"' {
				position++
				valueStart := position
				valueEnd := strings.IndexByte(source[position:], quote)
				if valueEnd < 0 {
					return htmlValidationAttributeCounts{}, errors.New("unterminated quoted HTML attribute")
				}
				attributeValue = source[valueStart : position+valueEnd]
				position += valueEnd + 1
			} else {
				valueStart := position
				for position < len(source) && source[position] != '>' && !isHTMLSpace(source[position]) {
					position++
				}
				attributeValue = source[valueStart:position]
			}
			if strings.EqualFold(attributeName, "aria-invalid") && strings.EqualFold(attributeValue, "true") {
				hasAriaInvalidTrue = true
			}
		}

		if !closed {
			return htmlValidationAttributeCounts{}, errors.New("unterminated HTML start tag")
		}
		if hasAutofocus {
			counts.autofocusElements++
		}
		if hasAriaInvalidTrue {
			counts.ariaInvalidTrueElements++
		}
		offset = position

		switch strings.ToLower(tagName) {
		case "plaintext":
			return counts, nil
		case "script", "style", "textarea", "title", "xmp", "iframe", "noembed", "noframes":
			closingTag, err := findHTMLRawTextClosingTag(source, offset, tagName)
			if err != nil {
				return htmlValidationAttributeCounts{}, err
			}
			offset = closingTag
		}
	}
	return counts, nil
}

func skipHTMLMarkup(source string, position int) (int, error) {
	var quote byte
	for position < len(source) {
		current := source[position]
		if quote != 0 {
			if current == quote {
				quote = 0
			}
			position++
			continue
		}
		if current == '\'' || current == '"' {
			quote = current
			position++
			continue
		}
		if current == '>' {
			return position + 1, nil
		}
		position++
	}
	return 0, errors.New("unterminated HTML markup")
}

func findHTMLRawTextClosingTag(source string, start int, tagName string) (int, error) {
	needle := "</" + strings.ToLower(tagName)
	lowerTail := strings.ToLower(source[start:])
	searchFrom := 0
	for searchFrom < len(lowerTail) {
		relative := strings.Index(lowerTail[searchFrom:], needle)
		if relative < 0 {
			break
		}
		candidate := searchFrom + relative
		afterName := candidate + len(needle)
		if afterName == len(lowerTail) || isHTMLSpace(lowerTail[afterName]) || lowerTail[afterName] == '>' || lowerTail[afterName] == '/' {
			return start + candidate, nil
		}
		searchFrom = candidate + 1
	}
	return 0, errors.New("unterminated HTML raw-text element")
}

func isHTMLASCIIAlpha(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func isHTMLTagNameEnd(value byte) bool {
	return isHTMLSpace(value) || value == '/' || value == '>'
}

func isHTMLAttributeNameEnd(value byte) bool {
	return isHTMLSpace(value) || value == '=' || value == '/' || value == '>'
}

func isHTMLSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\n', '\r', '\f':
		return true
	default:
		return false
	}
}

func TestCountHTMLValidationAttributes(t *testing.T) {
	tests := []struct {
		name                string
		page                string
		wantAutofocus       int
		wantAriaInvalidTrue int
	}{
		{
			name: "ignores script comments text and quoted values",
			page: `<!doctype html><!-- <input autofocus> -->
<p>autofocus</p>
<div data-autofocus="autofocus" title='autofocus="true"' data-state='aria-invalid="true"' srcdoc="<input autofocus aria-invalid=true>"></div>
<script>const sample = '[autofocus], [aria-invalid="true"]'; const name = "autofocus";</script>
<style>.demo::after { content: "<input autofocus aria-invalid=true>"; }</style>`,
			wantAutofocus:       0,
			wantAriaInvalidTrue: 0,
		},
		{
			name:                "counts one boolean attribute",
			page:                `<input id="first" autofocus aria-invalid="true"><div data-note="autofocus" title='aria-invalid="true"'></div>`,
			wantAutofocus:       1,
			wantAriaInvalidTrue: 1,
		},
		{
			name:                "counts two case-insensitive attributes",
			page:                `<INPUT AUTOFOCUS="false" ARIA-INVALID=true><textarea autofocus></textarea><button aria-invalid='true'>Save</button>`,
			wantAutofocus:       2,
			wantAriaInvalidTrue: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := countHTMLValidationAttributes(test.page)
			if err != nil {
				t.Fatalf("countHTMLValidationAttributes returned an error: %v", err)
			}
			if got.autofocusElements != test.wantAutofocus || got.ariaInvalidTrueElements != test.wantAriaInvalidTrue {
				t.Fatalf("countHTMLValidationAttributes = (%d autofocus, %d invalid), want (%d, %d)", got.autofocusElements, got.ariaInvalidTrueElements, test.wantAutofocus, test.wantAriaInvalidTrue)
			}
		})
	}
}

func TestCountHTMLValidationAttributesRejectsOversizedInput(t *testing.T) {
	page := strings.Repeat("x", maxValidationHTMLBytes+1)
	if _, err := countHTMLValidationAttributes(page); err == nil {
		t.Fatal("countHTMLValidationAttributes accepted input over its size limit")
	}
}
