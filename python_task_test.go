package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPythonTaskValidation(t *testing.T) {
	root := t.TempDir()
	valid := pythonTask{
		ID: "python:" + strings.Repeat("a", 32), Name: "safe task",
		InterpreterPath: filepath.Join(root, "Python 3", "python.exe"),
		ScriptPath:      filepath.Join(root, "scripts & checks", "task.py"),
	}
	if err := validatePythonTask(valid); err != nil {
		t.Fatalf("valid registered task rejected: %v", err)
	}

	for name, mutate := range map[string]func(*pythonTask){
		"bad ID":                func(task *pythonTask) { task.ID = "python:not-an-id" },
		"blank name":            func(task *pythonTask) { task.Name = " " },
		"control in name":       func(task *pythonTask) { task.Name = "task\nother" },
		"relative executable":   func(task *pythonTask) { task.InterpreterPath = "python.exe" },
		"wrong executable type": func(task *pythonTask) { task.InterpreterPath = filepath.Join(root, "python.cmd") },
		"relative script":       func(task *pythonTask) { task.ScriptPath = "task.py" },
		"wrong script type":     func(task *pythonTask) { task.ScriptPath = filepath.Join(root, "task.txt") },
		"oversize path": func(task *pythonTask) {
			task.ScriptPath = filepath.Join(root, strings.Repeat("x", pythonTaskPathLimit)+".py")
		},
		"secret environment name without reference": func(task *pythonTask) { task.SecretEnvName = "BUILD_TOKEN" },
		"malformed credential reference": func(task *pythonTask) {
			task.SecretEnvName, task.SecretRef = "BUILD_TOKEN", "cred:not-a-reference"
		},
		"reserved environment name": func(task *pythonTask) {
			task.SecretEnvName, task.SecretRef = "Path", "cred:"+strings.Repeat("a", 32)
		},
		"partial file fingerprint": func(task *pythonTask) {
			task.InterpreterSHA256, task.InterpreterSize = strings.Repeat("a", 64), 1
		},
		"malformed file fingerprint": func(task *pythonTask) {
			task.InterpreterSHA256, task.InterpreterSize = "not-a-digest", 1
			task.ScriptSHA256, task.ScriptSize = strings.Repeat("b", 64), 1
		},
		"oversized executable fingerprint": func(task *pythonTask) {
			task.InterpreterSHA256, task.InterpreterSize = strings.Repeat("a", 64), maxPythonInterpreterBytes+1
			task.ScriptSHA256, task.ScriptSize = strings.Repeat("b", 64), 1
		},
		"invalid environment name": func(task *pythonTask) {
			task.SecretEnvName, task.SecretRef = "=C:", "cred:"+strings.Repeat("a", 32)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := validatePythonTask(candidate); err == nil {
				t.Fatal("invalid registered task was accepted")
			}
		})
	}
	for _, name := range []string{"", "=C:", "PATH", "SystemRoot", "PYTHONPATH", "env name", "env-name", "토큰"} {
		if validPythonTaskSecretEnvName(name) {
			t.Errorf("invalid or reserved secret environment name %q was accepted", name)
		}
	}
	for _, name := range []string{"BUILD_TOKEN", "_SECRET", "A1", "LAH_TASK_SECRET"} {
		if !validPythonTaskSecretEnvName(name) {
			t.Errorf("valid secret environment name %q was rejected", name)
		}
	}

	duplicate := valid
	duplicate.ID = "python:" + strings.Repeat("b", 32)
	if err := validateConfig(config{Version: configVersion, PythonTasks: []pythonTask{valid, duplicate}}); err == nil {
		t.Fatal("case-insensitive duplicate Python task name was accepted")
	}

	tooMany := make([]pythonTask, maxPythonTasks+1)
	for index := range tooMany {
		tooMany[index] = pythonTask{
			ID:   fmt.Sprintf("python:%032x", index),
			Name: fmt.Sprintf("task %d", index), InterpreterPath: valid.InterpreterPath, ScriptPath: valid.ScriptPath,
		}
	}
	if err := validateConfig(config{Version: configVersion, PythonTasks: tooMany}); err == nil {
		t.Fatal("task catalog larger than its fixed limit was accepted")
	}
}

func TestPythonTaskFileFingerprintDetectsSameSizeChangeAndEnforcesLimits(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	if err := os.WriteFile(interpreter, []byte("synthetic executable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("alpha-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := pythonTask{ID: "python:" + strings.Repeat("1", 32), Name: "fingerprint test", InterpreterPath: interpreter, ScriptPath: script}
	task = pinPythonTaskForTest(t, task)
	if err := verifyPythonTaskFingerprint(task, nil); err != nil {
		t.Fatalf("unchanged files failed fingerprint check: %v", err)
	}
	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("bravo-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(script, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := verifyPythonTaskFingerprint(task, nil); !errors.Is(err, errPythonTaskFileUnavailable) {
		t.Fatalf("same-size content change with restored timestamp was not rejected: %v", err)
	}

	boundary := filepath.Join(root, "boundary.bin")
	if err := os.WriteFile(boundary, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, size, err := hashPythonTaskFile(boundary, 5, nil); err != nil || size != 5 {
		t.Fatalf("exact byte limit rejected: size=%d err=%v", size, err)
	}
	if _, _, err := hashPythonTaskFile(boundary, 4, nil); !errors.Is(err, errPythonTaskFileUnavailable) {
		t.Fatalf("file over byte limit was not rejected: %v", err)
	}
}

func TestPythonTaskFileFingerprintReadFailureIsGenericAndClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "python.exe")
	if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, opener := range map[string]pythonTaskFileOpener{
		"permission error": func(string) (pythonTaskFile, error) { return nil, errors.New("permission-denied-canary") },
		"read error": func(string) (pythonTaskFile, error) {
			return pythonTaskReadFailureFile{info: info}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := hashPythonTaskFile(path, 100, opener)
			if !errors.Is(err, errPythonTaskFileUnavailable) || strings.Contains(err.Error(), "permission-denied-canary") || strings.Contains(err.Error(), path) {
				t.Fatalf("file failure was not generic/fail-closed: %v", err)
			}
		})
	}
}

type pythonTaskReadFailureFile struct {
	info os.FileInfo
}

func (f pythonTaskReadFailureFile) Read([]byte) (int, error) {
	return 0, errors.New("read-denied-canary")
}
func (f pythonTaskReadFailureFile) Stat() (os.FileInfo, error) { return f.info, nil }
func (pythonTaskReadFailureFile) Close() error                 { return nil }

func TestMigrateV4ConfigToV5PreservesExistingSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := serviceBundleConfig()
	old.Version = 4
	old.ConnectionTests = map[string]connectionTest{
		old.GitHubTargets[0].ID: {Result: "success", CompletedAt: "2026-09-27T00:00:00Z"},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v4 config: %v", err)
	}
	got, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != configVersion || len(got.PythonTasks) != 0 ||
		!reflect.DeepEqual(got.GitHubTargets, old.GitHubTargets) ||
		!reflect.DeepEqual(got.JenkinsTargets, old.JenkinsTargets) ||
		!reflect.DeepEqual(got.HarborTargets, old.HarborTargets) ||
		!reflect.DeepEqual(got.DashboardTargets, old.DashboardTargets) ||
		!reflect.DeepEqual(got.ServiceBundles, old.ServiceBundles) ||
		!reflect.DeepEqual(got.ConnectionTests, old.ConnectionTests) {
		t.Fatalf("v4 migration changed saved fields: %#v", got)
	}

	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeat migration changed v6 settings")
	}
}

func TestMigrateV5ConfigToV7PreservesPythonTasksUnpinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := configV5{
		Version: 5,
		GitHubTargets: []target{{
			ID: "github:" + strings.Repeat("b", 32), Name: "legacy target",
			Origin: "https://github.example.invalid", Repository: "owner/repository",
			SecretRef: "cred:" + strings.Repeat("c", 32),
		}},
		PythonTasks: []pythonTaskV5{{
			ID: "python:" + strings.Repeat("a", 32), Name: "legacy task",
			InterpreterPath: `C:\Python\python.exe`, ScriptPath: `C:\tasks\task.py`,
		}},
		ConnectionTests: map[string]connectionTest{"github:" + strings.Repeat("b", 32): {Result: "success", CompletedAt: "2026-09-27T00:00:00Z"}},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v5 config: %v", err)
	}
	got, err := readConfig(path)
	if err != nil || got.Version != configVersion || len(got.PythonTasks) != 1 {
		t.Fatalf("migrated v5 settings = %#v err=%v", got, err)
	}
	task := got.PythonTasks[0]
	if task.Name != old.PythonTasks[0].Name || task.SecretRef != "" || task.SecretEnvName != "" || pythonTaskIsPinned(task) ||
		task.InterpreterPath != old.PythonTasks[0].InterpreterPath || task.ScriptPath != old.PythonTasks[0].ScriptPath {
		t.Fatalf("v5 migration changed task data or invented a secret mapping/fingerprint: %#v", task)
	}
	if got.ConnectionTests["github:"+strings.Repeat("b", 32)].Result != "success" {
		t.Fatal("v5 migration dropped connection history")
	}
}

func TestMigrateV6ConfigToV7PreservesSecretMappingButLeavesTasksUnpinned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	task := pythonTaskV6{
		ID: "python:" + strings.Repeat("8", 32), Name: "legacy v6 task",
		InterpreterPath: `C:\Python\python.exe`, ScriptPath: `C:\tasks\task.py`,
		SecretEnvName: "BUILD_TOKEN", SecretRef: "cred:" + strings.Repeat("9", 32),
	}
	old := configV6{
		Version:     6,
		PythonTasks: []pythonTaskV6{task},
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := migrateConfig(path); err != nil {
		t.Fatalf("migrate v6 config: %v", err)
	}
	got, err := readConfig(path)
	if err != nil || got.Version != configVersion || len(got.PythonTasks) != 1 {
		t.Fatalf("migrated v6 settings = %#v err=%v", got, err)
	}
	migrated := got.PythonTasks[0]
	if migrated.Name != task.Name || migrated.SecretEnvName != task.SecretEnvName || migrated.SecretRef != task.SecretRef ||
		migrated.InterpreterPath != task.InterpreterPath || migrated.ScriptPath != task.ScriptPath || pythonTaskIsPinned(migrated) {
		t.Fatalf("v6 migration changed task/secret mapping or inferred a file pin: %#v", migrated)
	}
	a := &app{configPath: path, openApprovalBrowser: func(string) error {
		t.Fatal("an unpinned migrated task opened approval")
		return nil
	}}
	if catalog, err := a.registeredPythonTasks(); err != nil || len(catalog.Tasks) != 0 {
		t.Fatalf("un-pinned migrated task appeared runnable: %#v err=%v", catalog, err)
	}
	if _, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: migrated.Name}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("un-pinned migrated task execution error = %v", err)
	}
}

func TestMigrateV4RejectsUnknownAndTrailingJSONWithoutWriting(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown field":  `{"version":4,"future":true}`,
		"trailing value": `{"version":4} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if err := migrateConfig(path); err == nil {
				t.Fatal("invalid v4 settings were accepted")
			}
			after, _ := os.ReadFile(path)
			if string(after) != string(before) {
				t.Fatal("failed migration modified the original settings")
			}
		})
	}
}

func TestPythonTaskSaveRequiresCSRFAndRunAsUserConfirmation(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "Python & 3", "python.exe")
	script := filepath.Join(root, "script (safe).py")
	if err := os.MkdirAll(filepath.Dir(interpreter), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interpreter, []byte("synthetic interpreter placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("success"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	secrets := &pythonTaskSecretSpy{}
	a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43801", csrf: "python-task-csrf"}

	form := url.Values{
		"csrf":                    {a.csrf},
		"python_task_id":          {"new"},
		"python_task_name":        {"Synthetic local check"},
		"python_interpreter_path": {interpreter},
		"python_script_path":      {script},
		"python_task_enabled":     {"yes"},
		"confirm_run_as_user":     {"yes"},
		"confirm_python_files":    {"yes"},
		"python_secret_env_name":  {""},
		"python_secret_value":     {""},
	}
	response := servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save status = %d, body=%q", response.Code, response.Body)
	}
	location, err := url.Parse(response.Header().Get("Location"))
	if err != nil || location.Query().Get("python_task_id") == "" || location.Query().Get("python_task_id") == "new" {
		t.Fatalf("saved task redirect = %q err=%v", response.Header().Get("Location"), err)
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.PythonTasks) != 1 || cfg.PythonTasks[0].Name != "Synthetic local check" || cfg.PythonTasks[0].Disabled ||
		cfg.PythonTasks[0].InterpreterPath != interpreter || cfg.PythonTasks[0].ScriptPath != script || !pythonTaskIsPinned(cfg.PythonTasks[0]) {
		t.Fatalf("saved task = %#v", cfg.PythonTasks)
	}
	if secrets.loads != 0 || secrets.saves != 0 || secrets.deletes != 0 {
		t.Fatalf("Python registration accessed credentials: %#v", secrets)
	}

	unchanged := append([]pythonTask(nil), cfg.PythonTasks...)
	noConfirmation := url.Values{}
	for key, values := range form {
		noConfirmation[key] = append([]string(nil), values...)
	}
	noConfirmation.Del("confirm_run_as_user")
	blocked := servePythonTaskSave(t, a, noConfirmation, "http://"+a.host)
	if blocked.Code != http.StatusSeeOther {
		t.Fatalf("unconfirmed save status = %d", blocked.Code)
	}
	cfg, err = readConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.PythonTasks, unchanged) {
		t.Fatal("task changed without run-as-user confirmation")
	}

	wrongOrigin := servePythonTaskSave(t, a, form, "http://attacker.example.invalid")
	if wrongOrigin.Code != http.StatusForbidden {
		t.Fatalf("wrong-origin save status = %d, want 403", wrongOrigin.Code)
	}
}

func TestPythonTaskSaveRequiresExplicitFileReviewAndDoesNotSilentlyRepin(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	for _, path := range []string{interpreter, script} {
		if err := os.WriteFile(path, []byte("alpha-123"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: configPath, host: "127.0.0.1:43803", csrf: "python-file-review-csrf"}
	form := url.Values{
		"csrf":                    {a.csrf},
		"python_task_id":          {"new"},
		"python_task_name":        {"Synthetic pinned task"},
		"python_interpreter_path": {interpreter},
		"python_script_path":      {script},
		"python_task_enabled":     {"yes"},
		"confirm_run_as_user":     {"yes"},
		"python_secret_env_name":  {""},
		"python_secret_value":     {""},
	}
	response := servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("unreviewed new-task save status=%d", response.Code)
	}
	if status := pythonTaskStatus(a); status != "Python task was not saved. Review and confirm the current executable and script files before saving." {
		t.Fatalf("unreviewed save status message = %q", status)
	}
	if cfg, err := readConfig(configPath); err != nil || len(cfg.PythonTasks) != 0 {
		t.Fatalf("unreviewed task changed settings: tasks=%#v err=%v", cfg.PythonTasks, err)
	}

	form.Set("confirm_python_files", "yes")
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("reviewed new-task save status=%d body=%q", response.Code, response.Body)
	}
	cfg, err := readConfig(configPath)
	if err != nil || len(cfg.PythonTasks) != 1 || !pythonTaskIsPinned(cfg.PythonTasks[0]) {
		t.Fatalf("reviewed task was not pinned: tasks=%#v err=%v", cfg.PythonTasks, err)
	}
	task := cfg.PythonTasks[0]
	baseline := task.ScriptSHA256
	form.Set("python_task_id", task.ID)
	form.Set("python_task_name", "Renamed without repinning")
	form.Del("confirm_python_files")
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("unchanged-file metadata save status=%d", response.Code)
	}
	cfg, err = readConfig(configPath)
	if err != nil || cfg.PythonTasks[0].Name != "Renamed without repinning" || cfg.PythonTasks[0].ScriptSHA256 != baseline {
		t.Fatalf("metadata save changed the file baseline: tasks=%#v err=%v", cfg.PythonTasks, err)
	}

	info, err := os.Stat(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("bravo-123"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(script, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	form.Set("python_task_name", "Drifted file without review")
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther || pythonTaskStatus(a) != "Python task was not saved. Review and confirm the current executable and script files before saving." {
		t.Fatalf("drifted task save without review status=%d message=%q", response.Code, pythonTaskStatus(a))
	}
	cfg, err = readConfig(configPath)
	if err != nil || cfg.PythonTasks[0].Name != "Renamed without repinning" || cfg.PythonTasks[0].ScriptSHA256 != baseline {
		t.Fatalf("unreviewed drift changed saved task: tasks=%#v err=%v", cfg.PythonTasks, err)
	}
	form.Set("confirm_python_files", "yes")
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("explicit re-pin save status=%d", response.Code)
	}
	cfg, err = readConfig(configPath)
	if err != nil || cfg.PythonTasks[0].Name != "Drifted file without review" || cfg.PythonTasks[0].ScriptSHA256 == baseline {
		t.Fatalf("explicit re-pin did not accept current file: tasks=%#v err=%v", cfg.PythonTasks, err)
	}
}

func pythonTaskStatus(a *app) string {
	a.statusMu.RLock()
	defer a.statusMu.RUnlock()
	return a.status
}

func TestPythonTaskSaveSecretReferenceLifecycle(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	for _, path := range []string{interpreter, script} {
		if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
		t.Fatal(err)
	}
	const secretCanary = "python-task-save-secret-canary"
	secrets := newPythonTaskSecretStoreFake()
	a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43821", csrf: "python-secret-save-csrf"}
	form := url.Values{
		"csrf":                    {a.csrf},
		"python_task_id":          {"new"},
		"python_task_name":        {"Synthetic secret task"},
		"python_interpreter_path": {interpreter},
		"python_script_path":      {script},
		"python_task_enabled":     {"yes"},
		"confirm_run_as_user":     {"yes"},
		"confirm_python_files":    {"yes"},
		"python_secret_env_name":  {"BUILD_TOKEN"},
		"python_secret_value":     {secretCanary},
	}
	response := servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("new task save status=%d body=%q", response.Code, response.Body)
	}
	cfg, err := readConfig(configPath)
	if err != nil || len(cfg.PythonTasks) != 1 {
		t.Fatalf("saved settings=%#v err=%v", cfg.PythonTasks, err)
	}
	task := cfg.PythonTasks[0]
	if task.SecretEnvName != "BUILD_TOKEN" || !secretRefPattern.MatchString(task.SecretRef) || len(secrets.saves) != 1 || string(secrets.values[task.SecretRef]) != secretCanary {
		t.Fatalf("saved task mapping/store state task=%#v saves=%#v", task, secrets.saves)
	}
	settingsBytes, err := os.ReadFile(configPath)
	if err != nil || !strings.Contains(string(settingsBytes), task.SecretRef) || strings.Contains(string(settingsBytes), secretCanary) {
		t.Fatalf("settings did not contain only the secret reference: err=%v bytes=%q", err, settingsBytes)
	}

	pageRequest := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/?python_task_id="+url.QueryEscape(task.ID), nil)
	pageResponse := httptest.NewRecorder()
	a.renderRootPage(pageResponse, pageRequest, nil, http.StatusOK)
	page := pageResponse.Body.String()
	if !strings.Contains(page, "BUILD_TOKEN") || strings.Contains(page, task.SecretRef) || strings.Contains(page, secretCanary) ||
		strings.Contains(page, `id="python-secret-value" type="password" name="python_secret_value" value=`) {
		t.Fatalf("task editor exposed a secret or reference: %s", page)
	}

	form.Set("python_task_id", task.ID)
	form.Set("python_secret_env_name", "RENAMED_BUILD_TOKEN")
	form.Set("python_secret_value", "")
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("blank replacement save status=%d body=%q", response.Code, response.Body)
	}
	cfg, err = readConfig(configPath)
	if err != nil || cfg.PythonTasks[0].SecretRef != task.SecretRef || cfg.PythonTasks[0].SecretEnvName != "RENAMED_BUILD_TOKEN" ||
		len(secrets.saves) != 1 || len(secrets.deletes) != 0 {
		t.Fatalf("blank replacement did not preserve credential: task=%#v store=%#v err=%v", cfg.PythonTasks[0], secrets, err)
	}

	shared := cfg.PythonTasks[0]
	shared.ID = "python:" + strings.Repeat("b", 32)
	shared.Name = "Second task sharing reference"
	cfg.PythonTasks = append(cfg.PythonTasks, shared)
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	form.Set("python_secret_clear", "yes")
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("clear shared mapping status=%d body=%q", response.Code, response.Body)
	}
	cfg, err = readConfig(configPath)
	if err != nil || cfg.PythonTasks[0].SecretRef != "" || cfg.PythonTasks[1].SecretRef != task.SecretRef || len(secrets.deletes) != 0 {
		t.Fatalf("clearing one shared mapping removed a live credential: tasks=%#v deletes=%#v err=%v", cfg.PythonTasks, secrets.deletes, err)
	}

	form.Set("python_task_id", shared.ID)
	form.Set("python_task_name", shared.Name)
	response = servePythonTaskSave(t, a, form, "http://"+a.host)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("clear final mapping status=%d body=%q", response.Code, response.Body)
	}
	cfg, err = readConfig(configPath)
	if err != nil || cfg.PythonTasks[1].SecretRef != "" || len(secrets.deletes) != 1 || secrets.deletes[0] != task.SecretRef {
		t.Fatalf("final mapping did not remove unused credential: tasks=%#v deletes=%#v err=%v", cfg.PythonTasks, secrets.deletes, err)
	}
}

func TestPythonTaskSaveSecretFailureAndConfigRollback(t *testing.T) {
	const secretCanary = "python-task-save-failure-canary"
	for _, test := range []struct {
		name          string
		storeSaveErr  error
		blockConfigIO bool
	}{
		{name: "credential store error", storeSaveErr: errors.New("credential-store-error-canary")},
		{name: "settings write rollback", blockConfigIO: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			interpreter := filepath.Join(root, "python.exe")
			script := filepath.Join(root, "task.py")
			for _, path := range []string{interpreter, script} {
				if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			configPath := filepath.Join(root, "config.json")
			if test.blockConfigIO {
				blockedParent := filepath.Join(root, "blocked-parent")
				if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				configPath = filepath.Join(blockedParent, "config.json")
			} else if err := writeConfig(configPath, config{Version: configVersion}); err != nil {
				t.Fatal(err)
			}
			var before []byte
			if !test.blockConfigIO {
				before, _ = os.ReadFile(configPath)
			}
			secrets := newPythonTaskSecretStoreFake()
			secrets.saveErr = test.storeSaveErr
			a := &app{configPath: configPath, secrets: secrets, host: "127.0.0.1:43822", csrf: "python-secret-failure-csrf"}
			form := url.Values{
				"csrf":                    {a.csrf},
				"python_task_id":          {"new"},
				"python_task_name":        {"Failing secret task"},
				"python_interpreter_path": {interpreter},
				"python_script_path":      {script},
				"python_task_enabled":     {"yes"},
				"confirm_run_as_user":     {"yes"},
				"confirm_python_files":    {"yes"},
				"python_secret_env_name":  {"BUILD_TOKEN"},
				"python_secret_value":     {secretCanary},
			}
			response := servePythonTaskSave(t, a, form, "http://"+a.host)
			if response.Code != http.StatusSeeOther {
				t.Fatalf("failed save status=%d body=%q", response.Code, response.Body)
			}
			a.statusMu.RLock()
			status := a.status
			a.statusMu.RUnlock()
			if strings.Contains(status, secretCanary) || strings.Contains(status, "credential-store-error-canary") {
				t.Fatalf("secret-store details reached status: %q", status)
			}
			if test.blockConfigIO {
				if len(secrets.saves) != 1 || len(secrets.deletes) != 1 || len(secrets.values) != 0 {
					t.Fatalf("failed settings write did not roll back the new credential: %#v", secrets)
				}
			} else {
				after, err := os.ReadFile(configPath)
				if err != nil || !reflect.DeepEqual(before, after) || len(secrets.saves) != 1 || len(secrets.deletes) != 0 {
					t.Fatalf("credential failure changed settings/store: err=%v before=%q after=%q store=%#v", err, before, after, secrets)
				}
			}
		})
	}
}

func TestPythonTaskUIShowsConsentAndEscapedSavedTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	root := t.TempDir()
	task := pythonTask{
		ID: "python:" + strings.Repeat("e", 32), Name: "Synthetic <task>",
		InterpreterPath: filepath.Join(root, "Python & Tools", "python.exe"),
		ScriptPath:      filepath.Join(root, "Scripts & Checks", "task.py"),
		SecretEnvName:   "BUILD_TOKEN",
		SecretRef:       "cred:" + strings.Repeat("e", 32),
		Disabled:        true,
	}
	if err := writeConfig(path, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	a := &app{configPath: path, host: "127.0.0.1:43802", csrf: "python-task-ui-csrf"}

	for _, test := range []struct {
		name     string
		cookie   string
		language string
		text     []string
	}{
		{
			name: "English", cookie: "lah_lang=en", language: "en",
			text: []string{
				`id="python-task-heading"`,
				`action="/save-python-task"`,
				`value="Synthetic &lt;task&gt;"`,
				`value="` + strings.ReplaceAll(task.InterpreterPath, "&", "&amp;") + `"`,
				`value="` + strings.ReplaceAll(task.ScriptPath, "&", "&amp;") + `"`,
				`Absolute path to executable (.exe)`,
				`The selected .exe is launched as saved; the app does not verify that it is Python.`,
				`limits execution to two active task trees and 30 seconds per call.`,
				`On Windows 10 and newer, the root process enters a private Job Object at process creation.`,
				`saved SHA-256 fingerprints of the .exe and .py files`,
				`this is not a sandbox`,
				`<span data-i18n-static>Enable this task for MCP</span>`,
				`Before showing the one-time local browser approval page, the app checks the saved SHA-256 fingerprints of the .exe and .py files; it checks them again immediately before launch.`,
				`UNC and device-namespace paths are rejected; mapped network drives are not identified.`,
				`A configured secret is loaded only after approval and task recheck`,
				`Standard output and error may be included in the current MCP response up to 8 KiB per stream after best-effort filtering of the configured task secret and known credential patterns.`,
				`If decoding, filtering, or process cleanup cannot be confirmed, both streams are omitted.`,
				`Output is not stored in settings, logs, or task history; filtering cannot guarantee removal of every secret.`,
				`The page shows the task name and, when configured, the secret environment variable name, but not paths or secret values.`,
				`BUILD_TOKEN`,
				`Clear the saved secret mapping and remove its credential when unused`,
				`Task names appear in the local MCP catalog and approval page. Do not include secrets or file paths in the name.`,
				`<span data-i18n-static>I reviewed the selected .exe and .py files. I will review and confirm them before saving whenever a file is new, changed, or reported as drifted. Files over 256 MiB (.exe) or 16 MiB (.py) cannot be registered.</span>`,
				`<span data-i18n-static>I understand that each MCP run requires separate local browser approval. The saved SHA-256 fingerprints of the .exe and .py files are checked before approval and immediately before launch; missing, unreadable, oversized, or changed files block start. A file may still change after the final check; this is not race-free.`,
			},
		},
		{
			name: "Korean", cookie: "lah_lang=ko", language: "ko",
			text: []string{
				`<html lang="ko">`,
				`'Registered Python tasks': '등록된 Python 작업'`,
				`'Enable this task for MCP': 'MCP에서 이 작업 사용'`,
				`<span data-i18n-static>Enable this task for MCP</span>`,
				`작업 이름은 로컬 MCP 카탈로그와 승인 화면에 표시됩니다. 이름에 비밀값이나 파일 경로를 넣지 마세요.`,
				`일회용 로컬 브라우저 승인 페이지를 표시하기 전에 앱은 저장된 .exe 및 .py 파일의 SHA-256 지문을 확인하고, 실행 직전에도 다시 확인합니다.`,
				`UNC 및 장치 네임스페이스 경로는 거부하지만 네트워크 드라이브로 매핑된 경로인지는 판별하지 않습니다.`,
				`현재 MCP 응답에는 표준 출력과 오류를 각 8 KiB까지 포함할 수 있으며, 등록된 작업 비밀값과 알려진 자격 증명 패턴을 최선 노력으로 가립니다.`,
				`한 출력이 상한을 넘으면 해당 출력 전체를 생략하고 잘림 상태를 표시합니다. 디코딩·가림 또는 프로세스 정리 확인에 실패하면 양쪽 출력을 모두 생략합니다.`,
				`출력은 설정·로그·실행 이력에 저장하지 않으며 필터가 모든 비밀값을 제거한다고 보장하지 않습니다.`,
				`<span data-i18n-static>I reviewed the selected .exe and .py files. I will review and confirm them before saving whenever a file is new, changed, or reported as drifted. Files over 256 MiB (.exe) or 16 MiB (.py) cannot be registered.</span>`,
				`<span data-i18n-static>I understand that each MCP run requires separate local browser approval. The saved SHA-256 fingerprints of the .exe and .py files are checked before approval and immediately before launch; missing, unreadable, oversized, or changed files block start. A file may still change after the final check; this is not race-free.`,
				`MCP를 실행할 때마다 별도의 로컬 브라우저 승인이 필요함을 이해합니다. .exe 및 .py 파일의 저장된 SHA-256 지문은 승인 전과 실행 직전에 확인하며, 파일이 없거나 읽을 수 없거나 크기 제한을 넘거나 변경되었으면 실행을 차단합니다.`,
				`실행 파일(.exe)의 절대 경로`,
				`앱은 해당 파일이 Python인지 확인하지 않습니다.`,
				`동시 작업 트리 2개와 호출당 30초로 제한합니다.`,
				`Windows 10 이상에서는 루트 프로세스를 만들 때 비공개 Job Object에 넣습니다.`,
				`트리 정리를 확인할 수 없으면 앱을 다시 시작할 때까지 이후 등록 작업 실행을 차단합니다.`,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "http://"+a.host+"/?python_task_id="+url.QueryEscape(task.ID), nil)
			if test.cookie != "" {
				request.Header.Set("Cookie", test.cookie)
			}
			response := httptest.NewRecorder()
			a.renderRootPage(response, request, nil, http.StatusOK)
			page := response.Body.String()
			if !strings.Contains(page, `<html lang="`+test.language+`"`) {
				t.Fatalf("page language was not %q", test.language)
			}
			for _, expected := range test.text {
				if !strings.Contains(page, expected) {
					t.Errorf("Python task UI omitted %q", expected)
				}
			}
			if strings.Contains(page, "Standard output and error are discarded.") || strings.Contains(page, "표준 출력과 오류는 버립니다.") {
				t.Fatal("Python task UI still says process output is discarded")
			}
			if strings.Contains(page, `name="python_task_enabled" value="yes" checked`) {
				t.Fatal("disabled task appeared enabled in the form")
			}
		})
	}
}

func TestPythonTaskStatusMessagesAreLocalized(t *testing.T) {
	for _, test := range []struct{ english, korean string }{
		{
			"Python task was not saved. Check the name and absolute Python/script paths.",
			"Python 작업을 저장하지 못했습니다. 이름과 Python 실행 파일·스크립트의 절대 경로를 확인하세요.",
		},
		{
			"Python task was not saved. Confirm the run-as-user notice.",
			"Python 작업을 저장하지 못했습니다. 현재 사용자 권한으로 실행된다는 안내를 확인하세요.",
		},
		{
			"Python task was not saved. Review and confirm the current executable and script files before saving.",
			"Python 작업을 저장하지 못했습니다. 현재 실행 파일과 스크립트를 검토하고 확인한 뒤 저장하세요.",
		},
		{
			"Python task was not saved. The executable or script could not be read within the allowed size limits.",
			"Python 작업을 저장하지 못했습니다. 실행 파일 또는 스크립트를 허용된 크기 안에서 읽지 못했습니다.",
		},
		{"Python task saved.", "Python 작업을 저장했습니다."},
	} {
		if got := localizeUIMessage(uiLocaleEnglish, test.english); got != test.english {
			t.Errorf("English status = %q, want %q", got, test.english)
		}
		if got := localizeUIMessage(uiLocaleKorean, test.english); got != test.korean {
			t.Errorf("Korean status = %q, want %q", got, test.korean)
		}
	}
}

func TestPythonTaskMCPUsesOnlyRegisteredTaskAndReturnsStatus(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "Python & 3", "python.exe")
	if err := os.MkdirAll(filepath.Dir(interpreter), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(interpreter, []byte("synthetic interpreter placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "script (safe).py")
	if err := os.WriteFile(script, []byte("success"), 0o600); err != nil {
		t.Fatal(err)
	}
	failedScript := filepath.Join(root, "failed.py")
	if err := os.WriteFile(failedScript, []byte("fail"), 0o600); err != nil {
		t.Fatal(err)
	}
	floodScript := filepath.Join(root, "flood.py")
	if err := os.WriteFile(floodScript, []byte("flood"), 0o600); err != nil {
		t.Fatal(err)
	}
	missingScript := filepath.Join(root, "missing.py")
	cfg := config{
		Version: configVersion,
		PythonTasks: []pythonTask{
			{ID: "python:" + strings.Repeat("a", 32), Name: "Synthetic local check", InterpreterPath: interpreter, ScriptPath: script},
			{ID: "python:" + strings.Repeat("b", 32), Name: "Synthetic failing check", InterpreterPath: interpreter, ScriptPath: failedScript},
			{ID: "python:" + strings.Repeat("c", 32), Name: "Synthetic output flood", InterpreterPath: interpreter, ScriptPath: floodScript},
			{ID: "python:" + strings.Repeat("d", 32), Name: "Disabled task", InterpreterPath: interpreter, ScriptPath: missingScript, Disabled: true},
		},
	}
	for index := range cfg.PythonTasks[:3] {
		cfg.PythonTasks[index] = pinPythonTaskForTest(t, cfg.PythonTasks[index])
	}
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	var launched int
	a := &app{
		configPath: configPath,
		openApprovalBrowser: func(address string) error {
			return submitPythonTaskApproval(t, address, "allow", nil, interpreter, script, failedScript, floodScript)
		},
	}
	a.pythonTaskCommand = func(ctx context.Context, executable string, args ...string) *exec.Cmd {
		launched++
		if executable != interpreter ||
			!reflect.DeepEqual(args, []string{"-I", "-B", script}) &&
				!reflect.DeepEqual(args, []string{"-I", "-B", failedScript}) &&
				!reflect.DeepEqual(args, []string{"-I", "-B", floodScript}) {
			t.Errorf("launched command = %q %#v, want saved interpreter and fixed flags/script", executable, args)
		}
		scriptPath := args[len(args)-1]
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", scriptPath)
	}

	ctx, session := connectPythonTaskMCP(t, a)
	catalog, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "registered_python_tasks"})
	if err != nil || catalog.IsError {
		t.Fatalf("task catalog result=%#v err=%v", catalog, err)
	}
	catalogJSON, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Synthetic local check", "python_run_registered_task"} {
		if !strings.Contains(string(catalogJSON), expected) {
			t.Errorf("task catalog omitted %q: %s", expected, catalogJSON)
		}
	}
	for _, privateValue := range []string{
		interpreter, script, failedScript, floodScript,
		cfg.PythonTasks[0].InterpreterSHA256, cfg.PythonTasks[0].ScriptSHA256,
		cfg.PythonTasks[1].InterpreterSHA256, cfg.PythonTasks[1].ScriptSHA256,
		cfg.PythonTasks[2].InterpreterSHA256, cfg.PythonTasks[2].ScriptSHA256,
	} {
		if strings.Contains(string(catalogJSON), privateValue) {
			t.Errorf("task catalog exposed a path or file fingerprint: %s", catalogJSON)
		}
	}
	if strings.Contains(string(catalogJSON), "Disabled task") {
		t.Fatalf("disabled task appeared in catalog: %s", catalogJSON)
	}

	extra, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Synthetic local check", "script_path": script},
	})
	if err == nil && (extra == nil || !extra.IsError) {
		t.Fatalf("extra executable selector was not rejected: result=%+v", extra)
	}
	unknown, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Disabled task"},
	})
	if err == nil && (unknown == nil || !unknown.IsError) {
		t.Fatalf("disabled task ran: result=%+v", unknown)
	}
	if launched != 0 {
		t.Fatalf("rejected MCP inputs launched %d processes", launched)
	}
	secretInput, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Synthetic local check", "secret_value": "python-mcp-input-canary"},
	})
	if err == nil && (secretInput == nil || !secretInput.IsError) {
		t.Fatalf("MCP secret input was not rejected: result=%+v", secretInput)
	}

	t.Setenv("LAH_PYTHON_TASK_PARENT_CANARY", "must-not-reach-child")
	success, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Synthetic local check"},
	})
	if err != nil || success.IsError {
		t.Fatalf("registered task call result=%#v err=%v", success, err)
	}
	encoded, err := json.Marshal(success)
	if err != nil {
		t.Fatal(err)
	}
	var result pythonTaskResult
	structured, err := json.Marshal(success.StructuredContent)
	if err != nil || json.Unmarshal(structured, &result) != nil {
		t.Fatalf("task result decode failed: %s err=%v", structured, err)
	}
	if result.Status != "completed" || result.Message != "The registered Python task completed." ||
		!strings.Contains(result.Stdout, "Authorization: [REDACTED]") || !strings.Contains(result.Stderr, "TOKEN=[REDACTED]") ||
		strings.Contains(string(encoded), "python_task_output_canary") || strings.Contains(string(encoded), "must-not-reach-child") ||
		strings.Contains(string(encoded), script) || strings.Contains(string(encoded), interpreter) ||
		strings.Contains(string(encoded), cfg.PythonTasks[0].InterpreterSHA256) || strings.Contains(string(encoded), cfg.PythonTasks[0].ScriptSHA256) || launched != 1 {
		t.Fatalf("task execution crossed its result or launch boundary: launches=%d response=%s", launched, encoded)
	}

	failure, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Synthetic failing check"},
	})
	if err != nil || failure.IsError {
		t.Fatalf("failed task result=%#v err=%v", failure, err)
	}
	failureJSON, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(failureJSON), `"status":"failed"`) || strings.Contains(string(failureJSON), "python_task_output_canary") ||
		!strings.Contains(string(failureJSON), "TOKEN=[REDACTED]") || strings.Contains(string(failureJSON), "exited with code") || launched != 2 {
		t.Fatalf("failed task response exposed child output or wrong status: launches=%d response=%s", launched, failureJSON)
	}

	flood, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Synthetic output flood"},
	})
	if err != nil || flood.IsError {
		t.Fatalf("flood task result=%#v err=%v", flood, err)
	}
	floodJSON, err := json.Marshal(flood)
	if err != nil {
		t.Fatal(err)
	}
	var floodResult pythonTaskResult
	structured, err = json.Marshal(flood.StructuredContent)
	if err != nil || json.Unmarshal(structured, &floodResult) != nil {
		t.Fatalf("flood result decode failed: %s err=%v", structured, err)
	}
	if floodResult.Status != "completed" || floodResult.Stdout != "" || floodResult.Stderr != "" ||
		!floodResult.StdoutTruncated || !floodResult.StderrTruncated || strings.Contains(string(floodJSON), strings.Repeat("x", 100)) || launched != 3 {
		t.Fatalf("flood output was not drained and omitted at the cap: launches=%d response=%s", launched, floodJSON)
	}

	a.pythonTaskSanitizer = func([]byte, []byte) (string, bool, error) {
		return "", false, errPythonTaskOutputUnavailable
	}
	unsafeOutput, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "python_run_registered_task", Arguments: map[string]any{"task": "Synthetic local check"},
	})
	if err != nil || unsafeOutput.IsError {
		t.Fatalf("sanitizer failure result=%#v err=%v", unsafeOutput, err)
	}
	unsafeJSON, _ := json.Marshal(unsafeOutput)
	structured, _ = json.Marshal(unsafeOutput.StructuredContent)
	var unsafeResult pythonTaskResult
	if json.Unmarshal(structured, &unsafeResult) != nil || unsafeResult.Status != "completed" ||
		unsafeResult.Stdout != "" || unsafeResult.Stderr != "" || !unsafeResult.OutputRedacted ||
		strings.Contains(string(unsafeJSON), "python_task_output_canary") || launched != 4 {
		t.Fatalf("sanitizer failure exposed output: launches=%d response=%s", launched, unsafeJSON)
	}
}

func TestPythonTaskLoadsOnlySelectedSecretAfterApprovalAndKeepsItOutOfMCP(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	scriptOne := filepath.Join(root, "one.py")
	scriptTwo := filepath.Join(root, "two.py")
	for path, data := range map[string]string{interpreter: "synthetic executable", scriptOne: "secret-one", scriptTwo: "secret-two"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	refOne, refTwo := "cred:"+strings.Repeat("1", 32), "cred:"+strings.Repeat("2", 32)
	const canaryOne = "python-task-first-secret-canary"
	const canaryTwo = "python-task-second-secret-canary"
	secrets := newPythonTaskSecretStoreFake()
	secrets.values[refOne] = []byte(canaryOne)
	secrets.values[refTwo] = []byte(canaryTwo)
	tasks := []pythonTask{
		{ID: "python:" + strings.Repeat("a", 32), Name: "First secret task", InterpreterPath: interpreter, ScriptPath: scriptOne, SecretEnvName: "LAH_TASK_SECRET", SecretRef: refOne},
		{ID: "python:" + strings.Repeat("b", 32), Name: "Second secret task", InterpreterPath: interpreter, ScriptPath: scriptTwo, SecretEnvName: "LAH_TASK_SECRET", SecretRef: refTwo},
	}
	for index := range tasks {
		tasks[index] = pinPythonTaskForTest(t, tasks[index])
	}
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: tasks}); err != nil {
		t.Fatal(err)
	}
	a := &app{
		configPath: configPath, secrets: secrets,
		openApprovalBrowser: func(address string) error {
			return submitPythonTaskApproval(t, address, "allow", nil, interpreter, scriptOne, scriptTwo)
		},
		pythonTaskCommand: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", args[len(args)-1])
		},
	}
	t.Setenv("LAH_PYTHON_TASK_PARENT_CANARY", "parent-environment-canary")

	ctx, session := connectPythonTaskMCP(t, a)
	catalog, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "registered_python_tasks"})
	if err != nil || catalog.IsError {
		t.Fatalf("catalog result=%#v err=%v", catalog, err)
	}
	catalogJSON, _ := json.Marshal(catalog)
	for _, forbidden := range []string{refOne, refTwo, canaryOne, canaryTwo, "LAH_TASK_SECRET"} {
		if strings.Contains(string(catalogJSON), forbidden) {
			t.Fatalf("catalog exposed task secret mapping %q: %s", forbidden, catalogJSON)
		}
	}

	for _, expected := range []struct{ name, canary, ref string }{
		{"First secret task", canaryOne, refOne},
		{"Second secret task", canaryTwo, refTwo},
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "python_run_registered_task", Arguments: map[string]any{"task": expected.name}})
		if err != nil || result.IsError {
			t.Fatalf("task %q result=%#v err=%v", expected.name, result, err)
		}
		encoded, _ := json.Marshal(result)
		structured, _ := json.Marshal(result.StructuredContent)
		var taskResult pythonTaskResult
		if json.Unmarshal(structured, &taskResult) != nil || taskResult.Status != "completed" ||
			!strings.Contains(taskResult.Stdout+taskResult.Stderr, "task finished with token [REDACTED]") ||
			strings.Contains(string(encoded), expected.canary) || strings.Contains(string(encoded), expected.ref) || strings.Contains(string(encoded), "LAH_TASK_SECRET") {
			t.Fatalf("MCP result exposed secret data: %s", encoded)
		}
	}
	if !reflect.DeepEqual(secrets.loads, []string{refOne, refTwo}) {
		t.Fatalf("loaded refs=%#v, want only the selected task references", secrets.loads)
	}
	if len(secrets.loadedBuffers) != 2 {
		t.Fatalf("loaded buffers=%d, want one per approved run", len(secrets.loadedBuffers))
	}
	for _, loaded := range secrets.loadedBuffers {
		if !reflect.DeepEqual(loaded, make([]byte, len(loaded))) {
			t.Fatalf("loaded secret copy was not cleared after the process: %q", loaded)
		}
	}
}

func TestPythonTaskSecretLookupFailsClosedBeforeLaunch(t *testing.T) {
	ref := "cred:" + strings.Repeat("9", 32)
	for _, test := range []struct {
		name     string
		value    []byte
		loadErr  error
		populate bool
	}{
		{name: "missing", populate: false},
		{name: "store error with canary", value: []byte("load-error-secret-canary"), loadErr: errors.New("store-error-secret-canary"), populate: true},
		{name: "empty", value: []byte{}, populate: true},
		{name: "NUL", value: []byte{'x', 0, 'y'}, populate: true},
		{name: "invalid UTF-8", value: []byte{0xff, 0xfe}, populate: true},
		{name: "oversize", value: []byte(strings.Repeat("x", maxSecretSize+1)), populate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			interpreter := filepath.Join(root, "python.exe")
			script := filepath.Join(root, "task.py")
			for _, path := range []string{interpreter, script} {
				if err := os.WriteFile(path, []byte("synthetic"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			task := pythonTask{
				ID: "python:" + strings.Repeat("9", 32), Name: "Lookup failure task",
				InterpreterPath: interpreter, ScriptPath: script, SecretEnvName: "LAH_TEST_TOKEN", SecretRef: ref,
			}
			task = pinPythonTaskForTest(t, task)
			configPath := filepath.Join(root, "config.json")
			if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
				t.Fatal(err)
			}
			secrets := newPythonTaskSecretStoreFake()
			if test.populate {
				secrets.values[ref] = append([]byte(nil), test.value...)
			}
			secrets.loadErr = test.loadErr
			launched := 0
			a := &app{
				configPath: configPath, secrets: secrets,
				openApprovalBrowser: func(address string) error {
					return submitPythonTaskApproval(t, address, "allow", nil, interpreter, script)
				},
				pythonTaskCommand: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
					launched++
					return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", args[len(args)-1])
				},
			}
			_, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
			if err == nil || !strings.Contains(err.Error(), "credential is unavailable; no task was run") || launched != 0 ||
				!reflect.DeepEqual(secrets.loads, []string{ref}) {
				t.Fatalf("lookup failure err=%v launches=%d refs=%#v", err, launched, secrets.loads)
			}
			if strings.Contains(err.Error(), "canary") || len(secrets.loadedBuffers) > 0 && strings.Trim(string(secrets.loadedBuffers[0]), "\x00") != "" {
				t.Fatalf("credential error leaked data or left loaded bytes: err=%v buffers=%q", err, secrets.loadedBuffers)
			}
		})
	}
}

func TestPythonTaskSubprocessHelper(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	scriptPath := os.Args[separator+1]
	mode := ""
	if separator+2 < len(os.Args) {
		mode = os.Args[separator+2]
	}
	if mode != "" {
		pythonTaskSubprocessTreeMode(scriptPath, mode)
		return
	}
	script, err := os.ReadFile(scriptPath)
	if err != nil {
		os.Exit(20)
	}
	workingDirectory, err := os.Getwd()
	if err != nil || os.Getenv("LAH_PYTHON_TASK_PARENT_CANARY") != "" || workingDirectory != filepath.Dir(scriptPath) {
		os.Exit(21)
	}
	switch string(script) {
	case "success":
		fmt.Fprintln(os.Stdout, "Authorization: Bearer python_task_output_canary")
		fmt.Fprintln(os.Stderr, "TOKEN=python_task_output_canary")
	case "fail":
		fmt.Fprintln(os.Stderr, "TOKEN=python_task_output_canary")
		os.Exit(7)
	case "secret-one":
		if os.Getenv("LAH_TASK_SECRET") != "python-task-first-secret-canary" ||
			os.Getenv("LAH_OTHER_TASK_SECRET") != "" || os.Getenv("LAH_PYTHON_TASK_PARENT_CANARY") != "" ||
			strings.Contains(strings.Join(os.Args, " "), "python-task-first-secret-canary") {
			os.Exit(30)
		}
		fmt.Fprintf(os.Stdout, "task finished with token %s\n", os.Getenv("LAH_TASK_SECRET"))
	case "secret-two":
		if os.Getenv("LAH_TASK_SECRET") != "python-task-second-secret-canary" ||
			os.Getenv("LAH_OTHER_TASK_SECRET") != "" || os.Getenv("LAH_PYTHON_TASK_PARENT_CANARY") != "" ||
			strings.Contains(strings.Join(os.Args, " "), "python-task-second-secret-canary") {
			os.Exit(31)
		}
		fmt.Fprintf(os.Stderr, "task finished with token %s\n", os.Getenv("LAH_TASK_SECRET"))
	case "flood":
		_, _ = fmt.Fprint(os.Stdout, strings.Repeat("x", 128<<10))
		_, _ = fmt.Fprint(os.Stderr, strings.Repeat("y", 128<<10))
	case "sleep":
		if err := os.WriteFile(scriptPath+".started", []byte("started"), 0o600); err != nil {
			os.Exit(23)
		}
		time.Sleep(5 * time.Second)
	case "spawn-tree", "spawn-tree-fail", "spawn-tree-wait":
		pythonTaskSubprocessTreeMode(scriptPath, "root")
	default:
		os.Exit(22)
	}
}

func pythonTaskSubprocessTreeMode(scriptPath, mode string) {
	writePID := func(suffix string) {
		if err := os.WriteFile(scriptPath+"."+suffix+".pid", []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
			os.Exit(40)
		}
	}
	writeReady := func(suffix string) {
		if err := os.WriteFile(scriptPath+"."+suffix+".ready", []byte("ready"), 0o600); err != nil {
			os.Exit(41)
		}
	}
	waitForFile := func(path string) bool {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path); err == nil {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	startChild := func(childMode string) {
		child := exec.Command(os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", scriptPath, childMode)
		child.Stdin = nil
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(42)
		}
	}
	switch mode {
	case "root":
		writePID("root")
		startChild("child")
		if !waitForFile(scriptPath + ".child.ready") {
			os.Exit(43)
		}
		writeReady("tree")
		if !waitForFile(scriptPath + ".root.release") {
			os.Exit(44)
		}
		contents, err := os.ReadFile(scriptPath)
		if err != nil {
			os.Exit(45)
		}
		if string(contents) == "spawn-tree-fail" {
			os.Exit(7)
		}
	case "child":
		writePID("child")
		startChild("grandchild")
		if !waitForFile(scriptPath + ".grandchild.ready") {
			os.Exit(46)
		}
		writeReady("child")
		for {
			time.Sleep(time.Hour)
		}
	case "grandchild":
		writePID("grandchild")
		writeReady("grandchild")
		for {
			time.Sleep(time.Hour)
		}
	default:
		os.Exit(47)
	}
}

func TestPythonTaskSettingsUpdateCannotInterleaveWithSecretProcessStart(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "sleep.py")
	changedScript := filepath.Join(root, "changed.py")
	for path, content := range map[string]string{interpreter: "synthetic", script: "sleep", changedScript: "synthetic"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pythonTask{
		ID: "python:" + strings.Repeat("c", 32), Name: "Serialized task",
		InterpreterPath: interpreter, ScriptPath: script,
		SecretEnvName: "LAH_SERIALIZED_SECRET", SecretRef: "cred:" + strings.Repeat("c", 32),
	}
	task = pinPythonTaskForTest(t, task)
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	secrets := newPythonTaskSecretStoreFake()
	secrets.values[task.SecretRef] = []byte("synthetic-secret")
	loadStarted, releaseLoad := make(chan struct{}), make(chan struct{})
	secrets.loadHook = func() {
		close(loadStarted)
		<-releaseLoad
	}
	a := &app{
		configPath: configPath, secrets: secrets,
		openApprovalBrowser: func(address string) error {
			return submitPythonTaskApproval(t, address, "allow", nil, interpreter, script)
		},
		pythonTaskCommand: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", args[len(args)-1])
		},
	}
	type runResult struct {
		result pythonTaskResult
		err    error
	}
	runDone := make(chan runResult, 1)
	go func() {
		result, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
		runDone <- runResult{result: result, err: err}
	}()
	select {
	case <-loadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("task did not reach secret loading")
	}

	changed := task
	changed.ScriptPath = changedScript
	changed = pinPythonTaskForTest(t, changed)
	writerStarted, writeDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(writerStarted)
		writeDone <- withConfigLock(func() error {
			return writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{changed}})
		})
	}()
	<-writerStarted
	select {
	case err := <-writeDone:
		t.Fatalf("settings update passed the lock during secret loading: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseLoad)
	startedFile := script + ".started"
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(startedFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("synthetic child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("settings update after child start: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("settings update remained blocked while the child was running")
	}
	select {
	case result := <-runDone:
		if result.err != nil || result.result.Status != "completed" {
			t.Fatalf("task result=%#v err=%v", result.result, result.err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("synthetic child did not finish")
	}
}

func TestPythonTaskProcessHonorsCallerCancellation(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "sleep.py")
	if err := os.WriteFile(interpreter, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("sleep"), 0o600); err != nil {
		t.Fatal(err)
	}
	task := pythonTask{ID: "python:" + strings.Repeat("d", 32), Name: "Sleep", InterpreterPath: interpreter, ScriptPath: script}
	task = pinPythonTaskForTest(t, task)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	command := func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", args[len(args)-1])
	}
	err := runPythonTaskProcess(ctx, task, command, nil)
	if err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("cancelled child err=%v context=%v", err, ctx.Err())
	}
}

func TestPythonTaskBlocksLaunchAfterUnconfirmedTreeCleanup(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	for path, content := range map[string]string{interpreter: "synthetic", script: "success"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pinPythonTaskForTest(t, pythonTask{
		ID: "python:" + strings.Repeat("f", 32), Name: "Fail-closed task",
		InterpreterPath: interpreter, ScriptPath: script,
	})
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	previous := pythonTaskTreeUnavailable.Swap(true)
	defer pythonTaskTreeUnavailable.Store(previous)
	approvalOpened := false
	processStarted := false
	a := &app{
		configPath: configPath,
		openApprovalBrowser: func(string) error {
			approvalOpened = true
			return nil
		},
		pythonTaskCommand: func(context.Context, string, ...string) *exec.Cmd {
			processStarted = true
			return nil
		},
	}
	result, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
	if err != nil || result.Status != "failed" || !strings.Contains(result.Message, "no task was run") || approvalOpened || processStarted {
		t.Fatalf("result=%#v err=%v approval=%v process=%v", result, err, approvalOpened, processStarted)
	}
}

func TestPythonTaskStartCleanupFailureBlocksLaterLaunch(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	for path, content := range map[string]string{interpreter: "synthetic", script: "success"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pinPythonTaskForTest(t, pythonTask{
		ID: "python:" + strings.Repeat("a", 32), Name: "Start cleanup failure",
		InterpreterPath: interpreter, ScriptPath: script,
	})
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	previous := pythonTaskTreeUnavailable.Swap(false)
	defer pythonTaskTreeUnavailable.Store(previous)

	approvalCount := 0
	startCount := 0
	a := &app{
		configPath: configPath,
		openApprovalBrowser: func(address string) error {
			approvalCount++
			return submitPythonTaskApproval(t, address, "allow", nil, interpreter, script)
		},
		pythonTaskStarter: func(context.Context, pythonTask, pythonTaskCommandFactory, []byte) (pythonTaskProcess, error) {
			startCount++
			return nil, errors.Join(errPythonTaskTreeCleanup, errors.New("synthetic native cleanup failure"))
		},
	}
	first, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
	if err != nil || first.Status != "failed" || !strings.Contains(first.Message, "no task was run") ||
		strings.Contains(first.Message, "synthetic") || !pythonTaskTreeIsUnavailable() {
		t.Fatalf("first result=%#v err=%v tree unavailable=%v", first, err, pythonTaskTreeIsUnavailable())
	}
	second, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
	if err != nil || second.Status != "failed" || !strings.Contains(second.Message, "no task was run") || approvalCount != 1 || startCount != 1 {
		t.Fatalf("second result=%#v err=%v approvals=%d starts=%d", second, err, approvalCount, startCount)
	}
}

func TestPythonTaskCleanupFailureDiscardsCapturedOutput(t *testing.T) {
	root := t.TempDir()
	interpreter := filepath.Join(root, "python.exe")
	script := filepath.Join(root, "task.py")
	for path, content := range map[string]string{interpreter: "synthetic", script: "success"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	task := pinPythonTaskForTest(t, pythonTask{
		ID: "python:" + strings.Repeat("e", 32), Name: "Output cleanup failure",
		InterpreterPath: interpreter, ScriptPath: script,
	})
	configPath := filepath.Join(root, "config.json")
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: []pythonTask{task}}); err != nil {
		t.Fatal(err)
	}
	previous := pythonTaskTreeUnavailable.Swap(false)
	defer pythonTaskTreeUnavailable.Store(previous)
	a := &app{
		configPath: configPath,
		openApprovalBrowser: func(address string) error {
			return submitPythonTaskApproval(t, address, "allow", nil, interpreter, script)
		},
		pythonTaskStarter: func(context.Context, pythonTask, pythonTaskCommandFactory, []byte) (pythonTaskProcess, error) {
			return pythonTaskTestProcess{
				output: pythonTaskProcessOutput{
					Stdout: []byte("captured-output-canary"),
					Stderr: []byte("captured-error-canary"),
				},
				err: errPythonTaskTreeCleanup,
			}, nil
		},
	}
	result, err := a.runRegisteredPythonTask(context.Background(), pythonTaskInput{Task: task.Name})
	encoded, marshalErr := json.Marshal(result)
	if err != nil || marshalErr != nil || result.Status != "failed" ||
		strings.Contains(string(encoded), "captured-output-canary") || strings.Contains(string(encoded), "captured-error-canary") ||
		!pythonTaskTreeIsUnavailable() {
		t.Fatalf("cleanup failure returned captured output: result=%s err=%v marshal=%v", encoded, err, marshalErr)
	}
}

func TestSanitizePythonTaskOutput(t *testing.T) {
	githubCanary := "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	privateKey := "-----BEGIN RSA PRIVATE KEY-----\nsynthetic-private-key\n-----END RSA PRIVATE KEY-----"
	cases := []struct {
		name       string
		input      []byte
		secret     []byte
		want       string
		wantError  bool
		wantCutoff bool
	}{
		{name: "plain UTF-8 keeps line breaks", input: []byte("step one\nstep two\tready"), want: "step one\nstep two\tready"},
		{name: "configured task secret", input: []byte("token=task-secret-canary"), secret: []byte("task-secret-canary"), want: "token=[REDACTED]"},
		{name: "short configured secret uses noncolliding marker", input: []byte("value=a"), secret: []byte("a"), want: "v[REDACTED]lue=[REDACTED]"},
		{name: "authorization header", input: []byte("Authorization: Bearer auth-canary"), want: "Authorization: [REDACTED]"},
		{name: "credential field", input: []byte(`{"client_secret":"field-canary"}`), want: `{"client_secret":"[REDACTED]"}`},
		{name: "GitHub token", input: []byte(githubCanary), want: "[REDACTED]"},
		{name: "private key", input: []byte(privateKey), want: "[REDACTED]"},
		{name: "output expansion is byte capped", input: []byte(strings.Repeat("a", 4096)), secret: []byte("a"), wantCutoff: true},
		{name: "invalid UTF-8", input: []byte{0xff}, wantError: true},
		{name: "NUL", input: []byte("before\x00after"), wantError: true},
		{name: "UTF-8 BOM", input: []byte("\xef\xbb\xbftext"), wantError: true},
		{name: "UTF-16LE-looking bytes", input: []byte{'t', 0, 'o', 0, 'k', 0}, wantError: true},
		{name: "embedded BOM", input: []byte("text\ufeffmore"), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, truncated, err := sanitizePythonTaskOutput(tc.input, tc.secret)
			if tc.wantError {
				if err == nil || got != "" {
					t.Fatalf("sanitizePythonTaskOutput() = %q, %v; want fail-closed output", got, err)
				}
				return
			}
			if err != nil || truncated != tc.wantCutoff {
				t.Fatalf("sanitizePythonTaskOutput() truncated=%v err=%v, want truncated=%v", truncated, err, tc.wantCutoff)
			}
			if tc.wantCutoff {
				if len(got) > maxPythonTaskOutputBytes || !utf8.ValidString(got) || strings.Contains(got, "a") {
					t.Fatalf("sanitized expansion was not safely capped: len=%d output=%q", len(got), got)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("sanitizePythonTaskOutput() = %q, want %q", got, tc.want)
			}
			if len(tc.secret) > 0 && strings.Contains(got, string(tc.secret)) {
				t.Fatalf("sanitized output retained configured secret %q: %q", tc.secret, got)
			}
		})
	}
}

func TestPythonTaskOutputCollectorDropsOverflowingStreamAndKeepsDraining(t *testing.T) {
	collector := newPythonTaskOutputCollector()
	exact := bytes.Repeat([]byte("x"), maxPythonTaskOutputBytes)
	if n, err := collector.Write(exact); err != nil || n != len(exact) {
		t.Fatalf("write exact cap = %d, %v", n, err)
	}
	got, truncated := collector.take()
	if truncated || !bytes.Equal(got, exact) {
		t.Fatalf("exact-cap output truncated=%v len=%d", truncated, len(got))
	}
	clear(got)
	if n, err := collector.Write(exact); err != nil || n != len(exact) {
		t.Fatalf("write before overflow = %d, %v", n, err)
	}
	if n, err := collector.Write([]byte("overflow")); err != nil || n != len("overflow") {
		t.Fatalf("overflow write = %d, %v", n, err)
	}
	if n, err := collector.Write(bytes.Repeat([]byte("more"), maxPythonTaskOutputBytes)); err != nil || n != 4*maxPythonTaskOutputBytes {
		t.Fatalf("post-overflow drain write = %d, %v", n, err)
	}
	got, truncated = collector.take()
	if len(got) != 0 || !truncated || len(collector.value) != 0 {
		t.Fatalf("overflowing output was retained: truncated=%v len=%d collector=%d", truncated, len(got), len(collector.value))
	}
}

func TestPythonTaskOutputSanitizationFailureDropsBothStreams(t *testing.T) {
	output := pythonTaskProcessOutput{
		Stdout: []byte("stdout-secret-canary"),
		Stderr: []byte("stderr-secret-canary"),
	}
	result := sanitizePythonTaskProcessOutput(&output, []byte("configured-secret"), func([]byte, []byte) (string, bool, error) {
		return "", false, errPythonTaskOutputUnavailable
	})
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "" || result.Stderr != "" || !result.OutputRedacted ||
		strings.Contains(string(encoded), "stdout-secret-canary") || strings.Contains(string(encoded), "stderr-secret-canary") ||
		len(output.Stdout) != 0 || len(output.Stderr) != 0 {
		t.Fatalf("sanitization failure did not fail closed: output=%+v result=%s", output, encoded)
	}
}

type pythonTaskTestProcess struct {
	output pythonTaskProcessOutput
	err    error
}

func (p pythonTaskTestProcess) Wait() (pythonTaskProcessOutput, error) {
	return p.output, p.err
}

func pinPythonTaskForTest(t *testing.T, task pythonTask) pythonTask {
	t.Helper()
	pinned, err := pinPythonTaskFiles(task, nil)
	if err != nil {
		t.Fatalf("pin synthetic Python task files: %v", err)
	}
	return pinned
}

func connectPythonTaskMCP(t *testing.T, a *app) (context.Context, *mcp.ClientSession) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := a.mcpServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "python-task-test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return ctx, clientSession
}

func servePythonTaskSave(t *testing.T, a *app, form url.Values, origin string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://"+a.host+"/save-python-task", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", origin)
	response := httptest.NewRecorder()
	a.securityHeaders(http.HandlerFunc(a.handlePythonTaskSave)).ServeHTTP(response, request)
	return response
}

type pythonTaskSecretSpy struct{ loads, saves, deletes int }

func (s *pythonTaskSecretSpy) Save(string, []byte) error { s.saves++; return nil }
func (s *pythonTaskSecretSpy) Load(string) ([]byte, error) {
	s.loads++
	return nil, errors.New("no secret access expected")
}
func (s *pythonTaskSecretSpy) Delete(string) error { s.deletes++; return nil }

var _ secretStore = (*pythonTaskSecretSpy)(nil)

type pythonTaskSecretStoreFake struct {
	values        map[string][]byte
	loads         []string
	saves         []string
	deletes       []string
	loadedBuffers [][]byte
	loadErr       error
	saveErr       error
	deleteErr     error
	loadHook      func()
}

func newPythonTaskSecretStoreFake() *pythonTaskSecretStoreFake {
	return &pythonTaskSecretStoreFake{values: make(map[string][]byte)}
}

func (s *pythonTaskSecretStoreFake) Save(ref string, value []byte) error {
	s.saves = append(s.saves, ref)
	if s.saveErr != nil {
		return s.saveErr
	}
	s.values[ref] = append([]byte(nil), value...)
	return nil
}

func (s *pythonTaskSecretStoreFake) Load(ref string) ([]byte, error) {
	s.loads = append(s.loads, ref)
	if s.loadHook != nil {
		s.loadHook()
	}
	value, ok := s.values[ref]
	if !ok && s.loadErr == nil {
		return nil, errors.New("missing fake credential")
	}
	loaded := append([]byte(nil), value...)
	s.loadedBuffers = append(s.loadedBuffers, loaded)
	return loaded, s.loadErr
}

func (s *pythonTaskSecretStoreFake) Delete(ref string) error {
	s.deletes = append(s.deletes, ref)
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.values, ref)
	return nil
}

var _ secretStore = (*pythonTaskSecretStoreFake)(nil)
