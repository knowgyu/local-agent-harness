//go:build windows && integration && parallelmanual

package main

import (
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const readinessNoJSBrowserQAOptIn = "LAH_ENABLE_MANUAL_WINDOWS_READINESS_NOJS_BROWSER_QA"

func TestManualWindowsReadinessNoJSBrowserQA(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("no-JavaScript headed acceptance requires native Windows")
	}
	if os.Getenv(readinessNoJSBrowserQAOptIn) != "1" {
		t.Skip("set the explicit no-JavaScript browser QA opt-in to run the headed check")
	}
	if os.Getenv(cleanupBrowserQARole) != "" {
		t.Skip("no-JavaScript browser acceptance runs only in the opted-in parent test process")
	}

	baseURL, configPath, fixture := readinessBrowserQAStartFixture(t)
	fixture.rememberFinalConfig(t, configPath)
	cliConfigPath := filepath.Join(filepath.Dir(configPath), "playwright-nojs.json")
	cliConfig, err := json.Marshal(map[string]any{
		"browser": map[string]any{
			"contextOptions": map[string]any{
				"javaScriptEnabled": false,
				"viewport":          map[string]any{"width": 1440, "height": 1000},
			},
		},
	})
	if err != nil {
		t.Fatal("could not encode the isolated no-JavaScript browser configuration")
	}
	if err := os.WriteFile(cliConfigPath, cliConfig, 0o600); err != nil {
		t.Fatal("could not write the isolated no-JavaScript browser configuration")
	}
	clearBytes(cliConfig)
	globalConfigDir := filepath.Join(filepath.Dir(configPath), "private-playwright-global")
	if err := os.MkdirAll(filepath.Join(globalConfigDir, ".playwright"), 0o700); err != nil {
		t.Fatal("could not create the private Playwright global config directory")
	}
	if err := os.WriteFile(filepath.Join(globalConfigDir, ".playwright", "cli.config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal("could not isolate the Playwright CLI global config")
	}
	priorGlobalConfig, hadPriorGlobalConfig := os.LookupEnv("PWTEST_CLI_GLOBAL_CONFIG")
	if err := os.Setenv("PWTEST_CLI_GLOBAL_CONFIG", globalConfigDir); err != nil {
		t.Fatal("could not isolate the Playwright CLI global config path")
	}
	t.Cleanup(func() {
		if hadPriorGlobalConfig {
			_ = os.Setenv("PWTEST_CLI_GLOBAL_CONFIG", priorGlobalConfig)
		} else {
			_ = os.Unsetenv("PWTEST_CLI_GLOBAL_CONFIG")
		}
	})

	suffix, err := cleanupBrowserQARandomHex(6)
	if err != nil {
		t.Fatal("could not create a unique no-JavaScript browser attempt")
	}
	session := "readiness-nojs-r1-" + suffix
	profile := filepath.Join(filepath.Dir(configPath), "private-nojs-playwright-profile")
	browserOpen := false
	t.Cleanup(func() {
		if browserOpen {
			cleanupBrowserQARunPlaywright(t, session, "close")
		}
	})

	readinessNoJSOpen(t, session, baseURL+"/__readiness_nojs_probe", cliConfigPath, profile, filepath.Dir(configPath))
	browserOpen = true
	probeSnapshot := readinessNoJSSnapshot(t, session)
	probeText := string(probeSnapshot)
	probeBefore := strings.Contains(probeText, "before")
	probeAfter := strings.Contains(probeText, "after")
	probeNoscript := strings.Contains(probeText, "JavaScript가 비활성화된 상태입니다")
	clearBytes([]byte(probeText))
	clearBytes(probeSnapshot)
	if !probeBefore || probeAfter || !probeNoscript {
		t.Fatalf("JavaScript-disabled static probe failed: unchanged_marker=%t mutated_marker=%t noscript_visible=%t", probeBefore, probeAfter, probeNoscript)
	}

	cleanupBrowserQARunPlaywright(t, session, "goto", baseURL+"/")
	homeSnapshot := readinessNoJSSnapshot(t, session)
	homeText := string(homeSnapshot)
	homeHasReadinessLink := strings.Contains(homeText, "Readiness checklist") || strings.Contains(homeText, "연결 준비 상태")
	homeHasSettingsContent := strings.Contains(homeText, "연결") || strings.Contains(homeText, "설정")
	homeHasNoJSNotice := strings.Contains(homeText, "일부 화면 기능과 현재 상태 조회에는 JavaScript가 필요합니다")
	homeSecretText := strings.Contains(homeText, "cred:")
	t.Logf("nojs_home_readiness_link=%t readable_korean=%t korean_nojs_notice=%t secret_reference_present=%t", homeHasReadinessLink, homeHasSettingsContent, homeHasNoJSNotice, homeSecretText)
	clearBytes([]byte(homeText))
	clearBytes(homeSnapshot)
	if !homeHasReadinessLink || !homeHasSettingsContent || !homeHasNoJSNotice || homeSecretText {
		t.Fatal("no-JavaScript home page did not retain its safe native navigation/readable settings content")
	}
	readinessNoJSCapture(t, session, "home-1440", 1440, 1000)
	cleanupBrowserQARunPlaywright(t, session, "resize", "375", "900")
	readinessNoJSCapture(t, session, "home-375", 375, 900)
	cleanupBrowserQARunPlaywright(t, session, "resize", "1440", "1000")

	cleanupBrowserQARunPlaywright(t, session, "goto", baseURL+"/readiness")
	readinessSnapshot := readinessNoJSSnapshot(t, session)
	readinessText := string(readinessSnapshot)
	readinessHeading := strings.Contains(readinessText, "연결 준비 상태")
	readinessNotice := strings.Contains(readinessText, "현재 읽기 전용 상태를 불러오려면 JavaScript가 필요합니다")
	readinessLinks := strings.Contains(readinessText, "설정 안내") || strings.Contains(readinessText, "설정")
	readinessLoading := strings.Contains(readinessText, "로컬 준비 상태를 불러오는 중")
	readinessRefreshButton := strings.Contains(readinessText, "설정 상태 새로 고침")
	readinessInspectButton := strings.Contains(readinessText, "클라이언트 등록 상태 확인")
	readinessClearButton := strings.Contains(readinessText, "내 확인 표시 지우기")
	readinessInspectedStatus := strings.Contains(readinessText, "클라이언트 설치 및 등록 상태를 확인하지 않았습니다")
	readinessCredentialRef := strings.Contains(readinessText, "cred:")
	t.Logf("nojs_readiness_heading_ko=%t noscript_notice_korean=%t static_guidance=%t status_still_loading=%t refresh_button_visible=%t inspect_button_visible=%t clear_button_visible=%t clients_not_inspected=%t secret_reference_present=%t", readinessHeading, readinessNotice, readinessLinks, readinessLoading, readinessRefreshButton, readinessInspectButton, readinessClearButton, readinessInspectedStatus, readinessCredentialRef)
	clearBytes([]byte(readinessText))
	clearBytes(readinessSnapshot)
	if !readinessHeading || !readinessNotice || !readinessLinks || readinessLoading || readinessRefreshButton || readinessInspectButton || readinessClearButton || !readinessInspectedStatus || readinessCredentialRef {
		t.Fatal("no-JavaScript readiness fallback lost Korean guidance, native links, or honest client state")
	}
	readinessNoJSCapture(t, session, "readiness-1440", 1440, 1000)
	cleanupBrowserQARunPlaywright(t, session, "resize", "375", "900")
	readinessNoJSCapture(t, session, "readiness-375", 375, 900)
	fixture.assertReadOnly(t, configPath)
	if fixture.state.statusGets.Load() != 0 || fixture.state.runtimeStatusGets.Load() != 0 || fixture.state.inspectionGets.Load() != 0 || fixture.state.observerCalls.Load() != 0 {
		t.Fatalf("no-JavaScript page unexpectedly performed local status reads: readiness=%d runtime=%d inspections=%d observer=%d", fixture.state.statusGets.Load(), fixture.state.runtimeStatusGets.Load(), fixture.state.inspectionGets.Load(), fixture.state.observerCalls.Load())
	}
	cleanupBrowserQARunPlaywright(t, session, "close")
	browserOpen = false
}

func readinessNoJSSnapshot(t *testing.T, session string) []byte {
	t.Helper()
	return cleanupBrowserQARunPlaywright(t, session, "snapshot")
}

func readinessNoJSCapture(t *testing.T, session, name string, width, height int) {
	t.Helper()
	path := readinessNoJSArtifactPath(t, name)
	cleanupBrowserQARunPlaywright(t, session, "screenshot", "--filename", path)
	file, err := os.Open(path)
	if err != nil {
		t.Fatal("could not reopen the no-JavaScript screenshot for dimension verification")
	}
	imageConfig, decodeErr := png.DecodeConfig(file)
	_ = file.Close()
	if decodeErr != nil || imageConfig.Width != width || imageConfig.Height != height {
		t.Fatalf("no-JavaScript screenshot dimensions mismatch: name=%s expected=%dx%d decoded=%t actual=%dx%d", name, width, height, decodeErr == nil, imageConfig.Width, imageConfig.Height)
	}
}

func readinessNoJSOpen(t *testing.T, session, targetURL, configPath, profile, privateDir string) {
	t.Helper()
	nodePath, cliPath, err := cleanupBrowserQAResolvePlaywrightCLI()
	if err != nil {
		t.Fatal("cached Playwright CLI is unavailable for the no-JavaScript browser harness")
	}
	logFile, err := os.CreateTemp(privateDir, "nojs-playwright-open-*.log")
	if err != nil {
		t.Fatal("could not create a private bounded Playwright diagnostic file")
	}
	logPath := logFile.Name()
	defer os.Remove(logPath)
	defer logFile.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, nodePath, cliPath, "--session", session, "open", targetURL, "--headed", "--config", configPath, "--profile", profile)
	command.Dir = filepath.Dir(os.Args[0])
	command.Env = append(os.Environ(), "NO_UPDATE_NOTIFIER=1")
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		if command.Process == nil || command.Process.Pid <= 0 {
			return os.ErrProcessDone
		}
		killer := exec.Command("taskkill.exe", "/PID", strconv.Itoa(command.Process.Pid), "/T", "/F")
		killer.Stdout, killer.Stderr = io.Discard, io.Discard
		if killErr := killer.Run(); killErr != nil {
			return errors.New("could not stop the timed-out no-JavaScript Playwright process tree")
		}
		return nil
	}
	command.Stdout, command.Stderr = logFile, logFile
	runErr := command.Run()
	if runErr != nil {
		_, _ = logFile.Seek(0, io.SeekStart)
		privateOutput, _ := io.ReadAll(io.LimitReader(logFile, 2<<20))
		category := readinessNoJSLaunchCategory(ctx.Err(), runErr, privateOutput)
		clearBytes(privateOutput)
		cleanupBrowserQACloseSessionAfterTimeout(nodePath, cliPath, session)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("headed no-JavaScript browser launch timed out: category=%s", category)
		}
		t.Fatalf("headed no-JavaScript browser launch failed: category=%s", category)
	}
	t.Log("nojs_launch_category=success")
}

func readinessNoJSLaunchCategory(ctxErr, runErr error, output []byte) string {
	if errors.Is(ctxErr, context.DeadlineExceeded) {
		return "command_timeout"
	}
	message := strings.ToLower(string(output))
	switch {
	case strings.Contains(message, "executable doesn't exist"), strings.Contains(message, "executable does not exist"), strings.Contains(message, "please run npx playwright install"):
		return "missing_browser_executable"
	case strings.Contains(message, "unknown config"), strings.Contains(message, "invalid configuration"), strings.Contains(message, "cannot unmarshal"), strings.Contains(message, "unknown property"):
		return "config_rejected"
	case strings.Contains(message, "userdatadir"), strings.Contains(message, "user data directory"), strings.Contains(message, "profile is already in use"):
		return "profile_or_context_error"
	case strings.Contains(message, "unsupported protocol"), strings.Contains(message, "invalid url"), strings.Contains(message, "url must"):
		return "unsupported_url"
	case strings.Contains(message, "browsertype.launch"), strings.Contains(message, "browser launch"):
		return "browser_launch_error"
	case runErr != nil:
		return "cli_nonzero_exit"
	default:
		return "unknown"
	}
}

func readinessNoJSArtifactPath(t *testing.T, name string) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal("could not locate no-JavaScript browser artifact directory")
	}
	artifactDir := filepath.Clean(filepath.Join(workingDir, "..", "..", "..", "..", "output", "playwright", "nojs-hardening-20261006-r1", "attempt-07"))
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal("could not create private no-JavaScript browser artifact directory")
	}
	return filepath.Join(artifactDir, "hardening-baseline-20261006-r1-attempt-07-"+name+".png")
}
