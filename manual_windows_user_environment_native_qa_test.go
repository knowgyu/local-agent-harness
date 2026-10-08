//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sys/windows"
)

const manualWindowsUserEnvironmentNativeQAGate = "LAH_ENABLE_MANUAL_WINDOWS_USER_ENV_NATIVE_QA"

// TestManualWindowsUserEnvironmentNativeQARoundTrip is deliberately opt-in.
// It touches only one randomly named HKCU value and always attempts rollback.
func TestManualWindowsUserEnvironmentNativeQARoundTrip(t *testing.T) {
	if os.Getenv(manualWindowsUserEnvironmentNativeQAGate) != "1" {
		t.Skip("set the opt-in gate to 1 to create and remove one synthetic HKCU environment value")
	}

	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		t.Fatal("could not create a synthetic test identity")
	}
	name := "LAH_QA_20261005_" + hex.EncodeToString(identity[:])
	firstValue := "lah-qa-value-a-" + hex.EncodeToString(identity[:])
	secondValue := "lah-qa-value-b-" + hex.EncodeToString(identity[:])
	firstBytes, secondBytes := []byte(firstValue), []byte(secondValue)

	if _, present := os.LookupEnv(name); present {
		t.Skip("generated process environment name already exists")
	}
	store := windowsUserEnvironmentStore{}
	names, err := store.ListCurrentUserNames()
	if err != nil {
		t.Fatal("could not inspect current-user environment name metadata")
	}
	for _, existing := range names {
		if strings.EqualFold(existing, name) {
			t.Skip("generated registry value name already exists")
		}
	}
	ownershipRecord := filepath.Join(mustCurrentWorkingDirectory(t), "manual_windows_user_environment_native_active_name.txt")
	if _, err := os.Stat(ownershipRecord); err == nil {
		t.Fatal("an earlier synthetic registry ownership record remains; preserving it for recovery")
	} else if !os.IsNotExist(err) {
		t.Fatal("could not check the synthetic registry ownership record")
	}
	if err := os.WriteFile(ownershipRecord, []byte(name), 0o600); err != nil {
		t.Fatal("could not record the generated synthetic name before mutation")
	}

	controller := newUserEnvironmentController(store, windowsUserEnvironmentChangeNotifier{})
	ownedValue := []byte(nil)
	ownedExpected := false
	// Cleanup deletes only the generated name and only while its value still
	// matches the last synthetic value this test attempted to write.
	t.Cleanup(func() {
		defer clear(firstBytes)
		defer clear(secondBytes)
		current, exists, readErr := store.ReadForUpdate(name)
		if readErr != nil {
			t.Error("could not confirm synthetic registry rollback")
			return
		}
		if exists {
			owned := ownedExpected && bytes.Equal(current, ownedValue)
			clear(current)
			if !owned {
				t.Error("synthetic registry value changed unexpectedly; cleanup preserved it")
				return
			}
			if _, deleteErr := controller.DeleteUserEnvironment(context.Background(), name); deleteErr != nil {
				t.Error("could not remove the synthetic registry value")
				return
			}
		}
		_, remains, verifyErr := store.ReadForUpdate(name)
		if verifyErr != nil || remains {
			t.Error("synthetic registry value was not absent after cleanup")
			return
		}
		if _, present := os.LookupEnv(name); present {
			t.Error("the test process environment unexpectedly contains the generated name")
			return
		}
		if err := os.Remove(ownershipRecord); err != nil && !os.IsNotExist(err) {
			t.Error("could not remove the synthetic registry ownership record")
		}
	})

	configPath := filepath.Join(t.TempDir(), "settings.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal("could not create isolated test settings")
	}
	host, csrf := "127.0.0.1:54137", "manual-user-environment-qa-csrf"
	a := &app{
		configPath:      configPath,
		namedSecrets:    newNamedSecretController(configPath, nil),
		userEnvironment: controller,
		host:            host,
		csrf:            csrf,
	}
	handler := newLocalUIHandler(a)

	ownedValue, ownedExpected = firstBytes, true
	if result := manualWindowsUserEnvironmentSubmit(t, handler, host, csrf, uiRouteSaveUserEnvironment, name, firstBytes); !result.OK || !result.Result.Applied || result.Result.Operation != "set" || !result.Result.RequiresNewSession {
		t.Fatal("production UI route did not report the synthetic registration safely")
	}
	manualWindowsUserEnvironmentAssertRegistryValue(t, store, name, firstBytes, true)
	manualWindowsUserEnvironmentAssertParentAbsent(t, name)
	manualWindowsUserEnvironmentAssertPageHidesValue(t, handler, host, name, firstBytes)
	manualWindowsUserEnvironmentAssertMCPHasNoEnvironmentValueTool(t, a.mcpServer(), firstBytes, secondBytes)
	manualWindowsUserEnvironmentAssertDefaultChildInheritance(t, name)
	manualWindowsUserEnvironmentAssertRegistryDerivedChild(t, name, firstBytes)

	ownedValue, ownedExpected = secondBytes, true
	if result := manualWindowsUserEnvironmentSubmit(t, handler, host, csrf, uiRouteSaveUserEnvironment, name, secondBytes); !result.OK || !result.Result.Applied || result.Result.Operation != "set" {
		t.Fatal("production UI route did not report the synthetic replacement safely")
	}
	manualWindowsUserEnvironmentAssertRegistryValue(t, store, name, secondBytes, true)
	manualWindowsUserEnvironmentAssertParentAbsent(t, name)
	manualWindowsUserEnvironmentAssertPageHidesValue(t, handler, host, name, secondBytes)

	ownedValue, ownedExpected = nil, true
	if result := manualWindowsUserEnvironmentSubmit(t, handler, host, csrf, uiRouteSaveUserEnvironment, name, nil); !result.OK || !result.Result.Applied || result.Result.Operation != "set" {
		t.Fatal("production UI route did not accept an empty synthetic value")
	}
	manualWindowsUserEnvironmentAssertRegistryValue(t, store, name, nil, true)
	manualWindowsUserEnvironmentAssertParentAbsent(t, name)
	manualWindowsUserEnvironmentAssertPageHidesValue(t, handler, host, name, nil)

	if result := manualWindowsUserEnvironmentSubmit(t, handler, host, csrf, uiRouteDeleteUserEnvironment, name, nil); !result.OK || !result.Result.Applied || result.Result.Operation != "delete" {
		t.Fatal("production UI route did not report the synthetic deletion safely")
	}
	manualWindowsUserEnvironmentAssertRegistryValue(t, store, name, nil, false)
	manualWindowsUserEnvironmentAssertParentAbsent(t, name)
	ownedExpected = false
}

// TestManualWindowsUserEnvironmentDefaultChildProbe is invoked only by the
// parent test's subprocess and reports no environment values.
func TestManualWindowsUserEnvironmentDefaultChildProbe(t *testing.T) {
	name, ok := manualWindowsUserEnvironmentChildName()
	if !ok {
		t.Skip("subprocess-only probe")
	}
	if _, present := os.LookupEnv(name); present {
		t.Fatal("default child inherited the generated name from the parent process block")
	}
	os.Exit(0)
}

// TestManualWindowsUserEnvironmentCreateEnvironmentBlockProbe is invoked only
// with a fresh Windows user environment block and reports no values.
func TestManualWindowsUserEnvironmentCreateEnvironmentBlockProbe(t *testing.T) {
	args, ok := manualWindowsUserEnvironmentChildArgs()
	if !ok {
		t.Skip("subprocess-only probe")
	}
	if len(args) != 2 {
		os.Exit(41)
	}
	value, present := os.LookupEnv(args[0])
	if !present {
		os.Exit(42)
	}
	digest := sha256.Sum256([]byte(value))
	if hex.EncodeToString(digest[:]) != args[1] {
		os.Exit(43)
	}
	os.Exit(0)
}

func manualWindowsUserEnvironmentChildName() (string, bool) {
	args, ok := manualWindowsUserEnvironmentChildArgs()
	if !ok || len(args) == 0 {
		return "", false
	}
	return args[0], true
}

func manualWindowsUserEnvironmentChildArgs() ([]string, bool) {
	for index, arg := range os.Args {
		if arg == "--" && index+1 < len(os.Args) {
			return os.Args[index+1:], true
		}
	}
	return nil, false
}

func mustCurrentWorkingDirectory(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal("could not locate the isolated QA source directory")
	}
	return directory
}

func manualWindowsUserEnvironmentSubmit(
	t *testing.T,
	handler http.Handler,
	host string,
	csrf string,
	path string,
	name string,
	value []byte,
) userEnvironmentUISuccess {
	t.Helper()
	form := url.Values{"csrf": {csrf}, "name": {name}}
	if path == uiRouteSaveUserEnvironment {
		form.Set("value", string(value))
	}
	body := []byte(form.Encode())
	defer clear(body)
	request := httptest.NewRequest(http.MethodPost, "http://"+host+path, bytes.NewReader(body))
	request.Host = host
	request.Header.Set("Origin", "http://"+host)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=UTF-8")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	responseBody := response.Body.Bytes()
	defer clear(responseBody)
	if response.Code != http.StatusOK || (len(value) > 0 && bytes.Contains(responseBody, value)) {
		t.Fatal("production UI mutation response failed or contained the synthetic value")
	}
	var result userEnvironmentUISuccess
	if err := json.Unmarshal(responseBody, &result); err != nil {
		t.Fatal("production UI mutation response was not safe result JSON")
	}
	if result.Result.Name != name || result.Result.Operation != map[string]string{
		uiRouteSaveUserEnvironment: "set", uiRouteDeleteUserEnvironment: "delete",
	}[path] {
		t.Fatal("production UI mutation response did not match the submitted synthetic operation")
	}
	return result
}

func manualWindowsUserEnvironmentAssertRegistryValue(t *testing.T, store windowsUserEnvironmentStore, name string, want []byte, wantPresent bool) {
	t.Helper()
	got, present, err := store.ReadForUpdate(name)
	if err != nil {
		t.Fatal("could not verify generated registry value")
	}
	defer clear(got)
	if present != wantPresent || (present && !bytes.Equal(got, want)) {
		t.Fatal("generated registry value did not match the expected state")
	}
}

func manualWindowsUserEnvironmentAssertParentAbsent(t *testing.T, name string) {
	t.Helper()
	if _, present := os.LookupEnv(name); present {
		t.Fatal("registry update changed the existing test process environment block")
	}
}

func manualWindowsUserEnvironmentAssertPageHidesValue(t *testing.T, handler http.Handler, host, name string, value []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	request.Host = host
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	page := response.Body.Bytes()
	defer clear(page)
	if response.Code != http.StatusOK || (len(value) > 0 && bytes.Contains(page, value)) || !bytes.Contains(page, []byte(name)) {
		t.Fatal("production settings page failed or did not keep the synthetic value private")
	}
}

func manualWindowsUserEnvironmentAssertMCPHasNoEnvironmentValueTool(t *testing.T, server *mcp.Server, values ...[]byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal("could not connect the production MCP server to an in-memory transport")
	}
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "user-environment-native-qa", Version: "1"}, nil).
		Connect(ctx, clientTransport, nil)
	if err != nil {
		_ = serverSession.Close()
		t.Fatal("could not connect the in-memory MCP inspector")
	}
	defer func() {
		_ = clientSession.Close()
		_ = serverSession.Wait()
	}()
	listed, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal("could not list production MCP tools")
	}
	encoded, err := json.Marshal(listed.Tools)
	if err != nil {
		t.Fatal("could not inspect production MCP tool metadata")
	}
	defer clear(encoded)
	for _, value := range values {
		if len(value) > 0 && bytes.Contains(encoded, value) {
			t.Fatal("production MCP tool metadata exposed a synthetic environment value")
		}
	}
	for _, tool := range listed.Tools {
		name := strings.ToLower(tool.Name)
		if strings.Contains(name, "user_environment") || strings.Contains(name, "set_environment") || strings.Contains(name, "delete_environment") {
			t.Fatal("production MCP exposed a user-environment value mutation or read tool")
		}
	}
}

func manualWindowsUserEnvironmentAssertDefaultChildInheritance(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	stdout, err := os.Create(filepath.Join(dir, "child.stdout"))
	if err != nil {
		t.Fatal("could not isolate child-process output")
	}
	stderr, err := os.Create(filepath.Join(dir, "child.stderr"))
	if err != nil {
		_ = stdout.Close()
		t.Fatal("could not isolate child-process output")
	}
	defer func() {
		_ = stdout.Close()
		_ = stderr.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestManualWindowsUserEnvironmentDefaultChildProbe$", "--", name)
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		t.Fatal("default child process did not confirm inherited-block behavior")
	}
	if err := stdout.Close(); err != nil {
		t.Fatal("could not close isolated child output")
	}
	if err := stderr.Close(); err != nil {
		t.Fatal("could not close isolated child output")
	}
	stdoutInfo, stdoutErr := os.Stat(stdout.Name())
	stderrInfo, stderrErr := os.Stat(stderr.Name())
	if stdoutErr != nil || stderrErr != nil || stdoutInfo.Size() != 0 || stderrInfo.Size() != 0 {
		t.Fatal("child probe emitted unexpected output; output contents were not read")
	}
	if err := os.Remove(stdout.Name()); err != nil {
		t.Fatal("could not remove isolated child output")
	}
	if err := os.Remove(stderr.Name()); err != nil {
		t.Fatal("could not remove isolated child output")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal("could not remove isolated child directory")
	}
}

func manualWindowsUserEnvironmentAssertRegistryDerivedChild(t *testing.T, name string, value []byte) {
	t.Helper()
	if len(value) == 0 {
		t.Fatal("positive registry-derived child probe requires a non-empty synthetic value")
	}

	var environment *uint16
	if err := windows.CreateEnvironmentBlock(&environment, windows.GetCurrentProcessToken(), false); err != nil {
		t.Fatal("Windows could not create a fresh environment block for the current user")
	}
	defer func() {
		_ = windows.DestroyEnvironmentBlock(environment)
	}()

	executable, err := windows.UTF16PtrFromString(os.Args[0])
	if err != nil {
		t.Fatal("could not locate the isolated child probe")
	}
	digest := sha256.Sum256(value)
	arguments := []string{
		os.Args[0],
		"-test.run=^TestManualWindowsUserEnvironmentCreateEnvironmentBlockProbe$",
		"--",
		name,
		hex.EncodeToString(digest[:]),
	}
	commandLine := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		commandLine = append(commandLine, windows.EscapeArg(argument))
	}
	commandLineUTF16, err := windows.UTF16FromString(strings.Join(commandLine, " "))
	if err != nil {
		t.Fatal("could not construct the isolated child command line")
	}
	var startup windows.StartupInfo
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESHOWWINDOW
	startup.ShowWindow = windows.SW_HIDE
	var process windows.ProcessInformation
	if err := windows.CreateProcess(
		executable,
		&commandLineUTF16[0],
		nil,
		nil,
		false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		environment,
		nil,
		&startup,
		&process,
	); err != nil {
		t.Fatal("could not start a child with the Windows-created environment block")
	}
	defer windows.CloseHandle(process.Thread)
	defer windows.CloseHandle(process.Process)

	waitResult, err := windows.WaitForSingleObject(process.Process, 15000)
	if err != nil {
		_ = windows.TerminateProcess(process.Process, 44)
		_, _ = windows.WaitForSingleObject(process.Process, 5000)
		t.Fatal("could not wait for the registry-derived child probe")
	}
	if waitResult != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(process.Process, 44)
		_, _ = windows.WaitForSingleObject(process.Process, 5000)
		t.Fatal("registry-derived child probe did not finish within the time limit")
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil || exitCode != 0 {
		t.Fatal("new Windows environment block did not carry the expected synthetic value")
	}
}
