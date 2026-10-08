package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"unicode/utf8"
)

const maxPythonTaskOutputBytes = 8 << 10

var errPythonTaskOutputUnavailable = errors.New("registered Python task output is unavailable")
var errPythonTaskOutputDrainTimeout = errors.New("registered Python task output pipes did not close")

type pythonTaskOutputSanitizer func(raw, secret []byte) (string, bool, error)

type pythonTaskProcessOutput struct {
	Stdout          []byte
	Stderr          []byte
	StdoutTruncated bool
	StderrTruncated bool
}

type pythonTaskOutputResult struct {
	Stdout          string
	Stderr          string
	StdoutTruncated bool
	StderrTruncated bool
	OutputRedacted  bool
}

type pythonTaskOutputCollector struct {
	mu        sync.Mutex
	value     []byte
	limit     int
	truncated bool
}

func newPythonTaskOutputCollector() *pythonTaskOutputCollector {
	return &pythonTaskOutputCollector{limit: maxPythonTaskOutputBytes}
}

func (c *pythonTaskOutputCollector) Write(value []byte) (int, error) {
	if c == nil {
		return len(value), nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		return len(value), nil
	}
	if len(value) > c.limit-len(c.value) {
		clear(c.value)
		c.value = nil
		c.truncated = true
		return len(value), nil
	}
	c.value = append(c.value, value...)
	return len(value), nil
}

func (c *pythonTaskOutputCollector) take() ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.truncated {
		clear(c.value)
		c.value = nil
		return nil, true
	}
	value := c.value
	c.value = nil
	return value, false
}

func (c *pythonTaskOutputCollector) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	clear(c.value)
	c.value = nil
	c.truncated = false
	c.mu.Unlock()
}

func pythonTaskOutputCollectors(commandStdout, commandStderr io.Writer) (*pythonTaskOutputCollector, *pythonTaskOutputCollector, error) {
	stdout, stdoutOK := commandStdout.(*pythonTaskOutputCollector)
	stderr, stderrOK := commandStderr.(*pythonTaskOutputCollector)
	if !stdoutOK || stdout == nil || !stderrOK || stderr == nil {
		return nil, nil, errPythonTaskOutputUnavailable
	}
	return stdout, stderr, nil
}

func pythonTaskProcessOutputFromCollectors(stdout, stderr *pythonTaskOutputCollector) pythonTaskProcessOutput {
	output := pythonTaskProcessOutput{}
	output.Stdout, output.StdoutTruncated = stdout.take()
	output.Stderr, output.StderrTruncated = stderr.take()
	return output
}

func clearPythonTaskProcessOutput(output *pythonTaskProcessOutput) {
	if output == nil {
		return
	}
	clear(output.Stdout)
	clear(output.Stderr)
	output.Stdout = nil
	output.Stderr = nil
	output.StdoutTruncated = false
	output.StderrTruncated = false
}

func sanitizePythonTaskProcessOutput(output *pythonTaskProcessOutput, secret []byte, sanitizer pythonTaskOutputSanitizer) pythonTaskOutputResult {
	result := pythonTaskOutputResult{
		StdoutTruncated: output != nil && output.StdoutTruncated,
		StderrTruncated: output != nil && output.StderrTruncated,
	}
	if output == nil {
		return result
	}
	defer clearPythonTaskProcessOutput(output)
	if sanitizer == nil {
		sanitizer = sanitizePythonTaskOutput
	}
	if len(output.Stdout) > 0 {
		stdout, truncated, err := sanitizer(output.Stdout, secret)
		if err != nil {
			result.OutputRedacted = true
			return result
		}
		result.Stdout = stdout
		result.StdoutTruncated = result.StdoutTruncated || truncated
	}
	if len(output.Stderr) > 0 {
		stderr, truncated, err := sanitizer(output.Stderr, secret)
		if err != nil {
			result.Stdout = ""
			result.Stderr = ""
			result.StdoutTruncated = output.StdoutTruncated
			result.StderrTruncated = output.StderrTruncated
			result.OutputRedacted = true
			return result
		}
		result.Stderr = stderr
		result.StderrTruncated = result.StderrTruncated || truncated
	}
	return result
}

func sanitizePythonTaskOutput(raw, secret []byte) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if len(raw) > maxPythonTaskOutputBytes || !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return "", false, errPythonTaskOutputUnavailable
	}
	value := string(raw)
	if strings.Contains(value, "\uFEFF") {
		return "", false, errPythonTaskOutputUnavailable
	}
	marker, displayMarker, err := pythonTaskRedactionMarker(value, string(secret))
	if err != nil {
		return "", false, err
	}
	cleaned := cleanOutputWithReplacement(value, string(secret), maxPythonTaskOutputBytes*16, true, marker)
	if len(secret) > 0 && strings.Contains(cleaned, string(secret)) {
		return "", false, errPythonTaskOutputUnavailable
	}
	cleaned = strings.ReplaceAll(cleaned, marker, displayMarker)
	if len(cleaned) <= maxPythonTaskOutputBytes {
		return cleaned, false, nil
	}
	return truncateUTF8Bytes(cleaned, maxPythonTaskOutputBytes), true, nil
}

func pythonTaskRedactionMarker(output, secret string) (string, string, error) {
	for codepoint := rune(0xF0000); codepoint <= 0xF3FFF; codepoint++ {
		marker := string(codepoint)
		if !strings.Contains(output, marker) && !strings.Contains(secret, marker) {
			for _, display := range []string{"[REDACTED]", "[MASKED]", "***", "•••", "◼◼◼", "<<hidden>>"} {
				if secret == "" || !strings.Contains(display, secret) {
					return marker, display, nil
				}
			}
			for displayCodepoint := rune(0x2580); displayCodepoint <= 0x259F; displayCodepoint++ {
				display := string(displayCodepoint)
				if !strings.Contains(secret, display) {
					return marker, display, nil
				}
			}
		}
	}
	return "", "", errPythonTaskOutputUnavailable
}

func truncateUTF8Bytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}
