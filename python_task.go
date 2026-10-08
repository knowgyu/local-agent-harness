package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	pythonTaskTimeout         = 30 * time.Second
	pythonTaskWaitDelay       = 1 * time.Second
	pythonTaskPathLimit       = 4096
	maxPythonInterpreterBytes = 256 << 20
	maxPythonScriptBytes      = 16 << 20
	maxPythonTasks            = 64
	maxPythonTaskFormSize     = 40 << 10
)

var pythonTaskIDPattern = regexp.MustCompile(`^python:[a-f0-9]{32}$`)
var pythonTaskSecretEnvNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var pythonTaskSHA256Pattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

var (
	errPythonTaskCredentialStore       = errors.New("Python task credential store unavailable")
	errPythonTaskCredentialUnavailable = errors.New("Python task credential is unavailable")
	errPythonTaskCredentialOrphan      = errors.New("Python task credential may remain unused")
	errPythonTaskOldCredentialCleanup  = errors.New("Python task saved but old credential cleanup failed")
	errPythonTaskFileReviewRequired    = errors.New("Python task file review confirmation required")
	errPythonTaskFileUnavailable       = errors.New("Python task file unavailable")
)

const pythonTaskRunToolDescription = "Run one enabled task previously registered in local settings. Every call requires a fresh one-time approval in a local browser page; deny, cancel, timeout, or browser failure prevents execution. At most two approval pages may be pending at once. Before approval and again after approval, the app hashes the saved executable and script within 256 MiB and 16 MiB limits; missing files, read errors, or changed contents prevent process start. The approval page shows the task name and, when configured, the saved secret environment variable name, but never executable or script paths or secret values. This is a user confirmation step, not an authentication boundary against other processes running as the same Windows user, and a file may still change after its final check. The only input is the exact display name; after approval and the final task/file recheck, the app loads only that task's Credential Manager reference, then launches the selected .exe directly with fixed -I -B flags and the saved script path. The stored secret value is passed only in the child process environment under its configured variable name; it is never accepted as an MCP argument. The child uses the script directory and minimal process environment. The current result may include UTF-8 stdout and stderr after a best-effort filter for the configured task secret and known credential patterns. Each stream is limited to 8 KiB; a stream that exceeds the raw capture limit is omitted and marked truncated. Invalid UTF-8, NUL, or a filtering failure omits both streams. Output is not written to settings, logs, or execution history. Filtering does not guarantee complete secret removal. Execution is limited to two active task trees and 30 seconds per call. On supported Windows versions, the root process is assigned to a private Job Object as part of process creation; ordinary CreateProcess descendants are terminated when the root exits, fails, times out, or is cancelled. If tree cleanup cannot be confirmed, later registered Python task launches are blocked and no output is returned. No executable, path, additional argument, or environment value is accepted. The selected .exe is not verified as Python. The process runs with the current Windows user's permissions, can have local or network side effects, and is not sandboxed. Descendant processes may inherit the secret. This does not contain processes created through other mechanisms such as Win32_Process.Create."

type pythonTask struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	InterpreterPath   string `json:"interpreter_path"`
	ScriptPath        string `json:"script_path"`
	Disabled          bool   `json:"disabled,omitempty"`
	SecretEnvName     string `json:"secret_env_name,omitempty"`
	SecretRef         string `json:"secret_ref,omitempty"`
	InterpreterSHA256 string `json:"interpreter_sha256,omitempty"`
	InterpreterSize   int64  `json:"interpreter_size"`
	ScriptSHA256      string `json:"script_sha256,omitempty"`
	ScriptSize        int64  `json:"script_size"`
}

type pythonTaskV5 struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	InterpreterPath string `json:"interpreter_path"`
	ScriptPath      string `json:"script_path"`
	Disabled        bool   `json:"disabled,omitempty"`
}

type pythonTaskV6 struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	InterpreterPath string `json:"interpreter_path"`
	ScriptPath      string `json:"script_path"`
	Disabled        bool   `json:"disabled,omitempty"`
	SecretEnvName   string `json:"secret_env_name,omitempty"`
	SecretRef       string `json:"secret_ref,omitempty"`
}

type pythonTaskFile interface {
	io.Reader
	Stat() (os.FileInfo, error)
	Close() error
}

type pythonTaskFileOpener func(string) (pythonTaskFile, error)

type pythonTaskChoice struct {
	ID       string
	Name     string
	Disabled bool
}

type pythonTaskSummary struct {
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

type pythonTasksResult struct {
	Tasks []pythonTaskSummary `json:"tasks"`
}

type pythonTaskInput struct {
	Task string `json:"task"`
}

type pythonTaskResult struct {
	Task            string `json:"task"`
	Status          string `json:"status"`
	Message         string `json:"message"`
	Stdout          string `json:"stdout,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	StdoutTruncated bool   `json:"stdout_truncated,omitempty"`
	StderrTruncated bool   `json:"stderr_truncated,omitempty"`
	OutputRedacted  bool   `json:"output_redacted,omitempty"`
}

type pythonTaskCommandFactory func(context.Context, string, ...string) *exec.Cmd

var pythonTaskSlots = make(chan struct{}, 2)

func validatePythonTask(task pythonTask) error {
	name := strings.TrimSpace(task.Name)
	if !pythonTaskIDPattern.MatchString(task.ID) || name == "" || name != task.Name || len(task.Name) > 80 ||
		strings.IndexFunc(task.Name, unicode.IsControl) >= 0 {
		return errors.New("invalid registered Python task")
	}
	if !validPythonTaskPath(task.InterpreterPath, ".exe") || !validPythonTaskPath(task.ScriptPath, ".py") {
		return errors.New("invalid registered Python task paths")
	}
	if task.SecretRef == "" {
		if task.SecretEnvName != "" {
			return errors.New("Python task secret mapping has no credential reference")
		}
	} else if !secretRefPattern.MatchString(task.SecretRef) || !validPythonTaskSecretEnvName(task.SecretEnvName) {
		return errors.New("invalid Python task secret mapping")
	}
	if !validPythonTaskFingerprintState(task) {
		return errors.New("invalid Python task file fingerprint")
	}
	return nil
}

func validPythonTaskFingerprintState(task pythonTask) bool {
	unpinned := task.InterpreterSHA256 == "" && task.InterpreterSize == 0 && task.ScriptSHA256 == "" && task.ScriptSize == 0
	if unpinned {
		return true
	}
	return pythonTaskSHA256Pattern.MatchString(task.InterpreterSHA256) &&
		task.InterpreterSize > 0 && task.InterpreterSize <= maxPythonInterpreterBytes &&
		pythonTaskSHA256Pattern.MatchString(task.ScriptSHA256) &&
		task.ScriptSize >= 0 && task.ScriptSize <= maxPythonScriptBytes
}

func pythonTaskIsPinned(task pythonTask) bool {
	return pythonTaskSHA256Pattern.MatchString(task.InterpreterSHA256) &&
		task.InterpreterSize > 0 && task.InterpreterSize <= maxPythonInterpreterBytes &&
		pythonTaskSHA256Pattern.MatchString(task.ScriptSHA256) &&
		task.ScriptSize >= 0 && task.ScriptSize <= maxPythonScriptBytes
}

func validPythonTaskSecretEnvName(name string) bool {
	if !pythonTaskSecretEnvNamePattern.MatchString(name) {
		return false
	}
	switch strings.ToUpper(name) {
	case "PATH", "PATHEXT", "COMSPEC", "SYSTEMROOT", "WINDIR", "TEMP", "TMP",
		"PYTHONHOME", "PYTHONPATH", "PYTHONSTARTUP", "PYTHONINSPECT", "PYTHONUSERBASE",
		"PYTHONWARNINGS", "PYTHONUTF8", "PYTHONIOENCODING":
		return false
	default:
		return true
	}
}

func validPythonTaskSecret(value []byte) bool {
	if len(value) == 0 || len(value) > maxSecretSize || !utf8.Valid(value) {
		return false
	}
	for _, b := range value {
		if b == 0 {
			return false
		}
	}
	return true
}

func validPythonTaskPath(value, extension string) bool {
	if value == "" || len(value) > pythonTaskPathLimit ||
		value != strings.TrimSpace(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 ||
		!filepath.IsAbs(value) || !strings.EqualFold(filepath.Ext(value), extension) {
		return false
	}
	if runtime.GOOS == "windows" {
		windowsPath := strings.ReplaceAll(value, "/", `\`)
		volume := filepath.VolumeName(windowsPath)
		if len(volume) != 2 || volume[1] != ':' || len(windowsPath) < 3 || windowsPath[2] != '\\' {
			return false
		}
	}
	return true
}

func pinPythonTaskFiles(task pythonTask, openFile pythonTaskFileOpener) (pythonTask, error) {
	interpreterHash, interpreterSize, err := hashPythonTaskFile(task.InterpreterPath, maxPythonInterpreterBytes, openFile)
	if err != nil {
		return pythonTask{}, errPythonTaskFileUnavailable
	}
	scriptHash, scriptSize, err := hashPythonTaskFile(task.ScriptPath, maxPythonScriptBytes, openFile)
	if err != nil {
		return pythonTask{}, errPythonTaskFileUnavailable
	}
	task.InterpreterSHA256, task.InterpreterSize = interpreterHash, interpreterSize
	task.ScriptSHA256, task.ScriptSize = scriptHash, scriptSize
	return task, nil
}

func verifyPythonTaskFingerprint(task pythonTask, openFile pythonTaskFileOpener) error {
	if !pythonTaskIsPinned(task) {
		return errPythonTaskFileUnavailable
	}
	current, err := pinPythonTaskFiles(task, openFile)
	if err != nil || current.InterpreterSHA256 != task.InterpreterSHA256 || current.InterpreterSize != task.InterpreterSize ||
		current.ScriptSHA256 != task.ScriptSHA256 || current.ScriptSize != task.ScriptSize {
		return errPythonTaskFileUnavailable
	}
	return nil
}

func hashPythonTaskFile(path string, maxBytes int64, openFile pythonTaskFileOpener) (digest string, size int64, resultErr error) {
	if openFile == nil {
		openFile = func(path string) (pythonTaskFile, error) { return os.Open(path) }
	}
	file, err := openFile(path)
	if err != nil || file == nil {
		if file != nil {
			_ = file.Close()
		}
		return "", 0, errPythonTaskFileUnavailable
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && resultErr == nil {
			digest, size, resultErr = "", 0, errPythonTaskFileUnavailable
		}
	}()

	before, err := file.Stat()
	if err != nil || before == nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxBytes {
		return "", 0, errPythonTaskFileUnavailable
	}
	hasher := sha256.New()
	readSize, err := io.Copy(hasher, io.LimitReader(file, maxBytes+1))
	if err != nil || readSize > maxBytes {
		return "", 0, errPythonTaskFileUnavailable
	}
	after, err := file.Stat()
	if err != nil || after == nil || !after.Mode().IsRegular() || !os.SameFile(before, after) ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || readSize != after.Size() {
		return "", 0, errPythonTaskFileUnavailable
	}
	pathInfo, err := os.Stat(path)
	if err != nil || pathInfo == nil || !pathInfo.Mode().IsRegular() || !os.SameFile(after, pathInfo) {
		return "", 0, errPythonTaskFileUnavailable
	}
	return hex.EncodeToString(hasher.Sum(nil)), readSize, nil
}

func (a *app) handlePythonTaskSave(w http.ResponseWriter, r *http.Request) {
	if !a.checkPostLimit(w, r, maxPythonTaskFormSize) {
		return
	}
	if !validPythonTaskSaveForm(r.PostForm) {
		a.redirectPythonTask(w, r, "new", "Python task was not saved. Check the task details and secret mapping.", true)
		return
	}
	if r.PostForm.Get("confirm_run_as_user") != "yes" {
		a.redirectPythonTask(w, r, "new", "Python task was not saved. Confirm the run-as-user notice.", true)
		return
	}

	name := strings.TrimSpace(r.PostForm.Get("python_task_name"))
	interpreterPath := r.PostForm.Get("python_interpreter_path")
	scriptPath := r.PostForm.Get("python_script_path")
	secretValue := r.PostForm.Get("python_secret_value")
	secretValueBytes := []byte(secretValue)
	defer clear(secretValueBytes)
	secretEnvName := r.PostForm.Get("python_secret_env_name")
	clearSecret := r.PostForm.Get("python_secret_clear") == "yes"
	if interpreterPath != strings.TrimSpace(interpreterPath) || scriptPath != strings.TrimSpace(scriptPath) {
		a.redirectPythonTask(w, r, r.PostForm.Get("python_task_id"), "Python task was not saved. Check the task details and secret mapping.", true)
		return
	}

	taskID := r.PostForm.Get("python_task_id")
	var savedID string
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("local settings are invalid")
		}

		index := -1
		if taskID == "" || taskID == "new" {
			taskID, err = newTargetID("python")
			if err != nil {
				return errors.New("could not create Python task ID")
			}
		} else {
			for i := range cfg.PythonTasks {
				if cfg.PythonTasks[i].ID == taskID {
					index = i
					break
				}
			}
			if index < 0 {
				return errors.New("registered Python task is unavailable")
			}
		}

		var old pythonTask
		if index >= 0 {
			old = cfg.PythonTasks[index]
		}
		task := pythonTask{
			ID: taskID, Name: name, InterpreterPath: interpreterPath, ScriptPath: scriptPath,
			Disabled: r.PostForm.Get("python_task_enabled") != "yes",
		}
		if index >= 0 {
			task.SecretEnvName, task.SecretRef = old.SecretEnvName, old.SecretRef
		}
		if clearSecret {
			if secretValue != "" {
				return errors.New("invalid Python task secret mapping")
			}
			task.SecretEnvName, task.SecretRef = "", ""
		} else if secretValue != "" {
			if !validPythonTaskSecretEnvName(secretEnvName) || !validPythonTaskSecret(secretValueBytes) {
				return errors.New("invalid Python task secret mapping")
			}
			task.SecretEnvName = secretEnvName
			task.SecretRef = "cred:" + strings.Repeat("0", 32)
		} else if secretEnvName != "" {
			task.SecretEnvName = secretEnvName
		}
		if validatePythonTask(task) != nil {
			return errors.New("invalid Python task")
		}
		fileReviewConfirmed := r.PostForm.Get("confirm_python_files") == "yes"
		filesNeedPin := index < 0 || old.InterpreterPath != task.InterpreterPath || old.ScriptPath != task.ScriptPath || !pythonTaskIsPinned(old)
		if fileReviewConfirmed {
			task, err = pinPythonTaskFiles(task, nil)
			if err != nil {
				return errPythonTaskFileUnavailable
			}
		} else {
			if filesNeedPin || verifyPythonTaskFingerprint(old, nil) != nil {
				return errPythonTaskFileReviewRequired
			}
			task.InterpreterSHA256, task.InterpreterSize = old.InterpreterSHA256, old.InterpreterSize
			task.ScriptSHA256, task.ScriptSize = old.ScriptSHA256, old.ScriptSize
		}
		if validatePythonTask(task) != nil {
			return errors.New("invalid Python task")
		}
		for i, existing := range cfg.PythonTasks {
			if i != index && (existing.ID == task.ID || strings.EqualFold(existing.Name, task.Name)) {
				return errors.New("duplicate registered Python task")
			}
		}

		newRef := ""
		if secretValue != "" {
			if a.secrets == nil {
				return errPythonTaskCredentialStore
			}
			refBytes := make([]byte, 16)
			if _, err := rand.Read(refBytes); err != nil {
				return errors.New("could not create Python task credential reference")
			}
			newRef = "cred:" + hex.EncodeToString(refBytes)
			task.SecretRef = newRef
			if err := a.secrets.Save(newRef, secretValueBytes); err != nil {
				return errPythonTaskCredentialStore
			}
		}
		if index < 0 {
			cfg.PythonTasks = append(cfg.PythonTasks, task)
		} else {
			cfg.PythonTasks[index] = task
		}
		if err := writeConfig(a.configPath, cfg); err != nil {
			if newRef != "" && a.secrets.Delete(newRef) != nil {
				return errPythonTaskCredentialOrphan
			}
			return errors.New("could not save Python task")
		}
		savedID = task.ID
		if old.SecretRef != "" && old.SecretRef != task.SecretRef && !configReferencesSecret(cfg, old.SecretRef) {
			if a.secrets == nil || a.secrets.Delete(old.SecretRef) != nil {
				return errPythonTaskOldCredentialCleanup
			}
		}
		return nil
	})
	if err != nil {
		message := "Python task was not saved. Check the task details and secret mapping."
		switch {
		case errors.Is(err, errPythonTaskCredentialStore):
			message = secretStoreError()
		case errors.Is(err, errPythonTaskCredentialOrphan):
			message = "Could not save Python task settings. Previous settings are unchanged, but an unused credential remains in Windows Credential Manager. Remove the Local Agent Harness credential entry before retrying."
		case errors.Is(err, errPythonTaskOldCredentialCleanup):
			message = "Python task saved, but the previous unused credential could not be removed from Windows Credential Manager."
		case errors.Is(err, errPythonTaskFileReviewRequired):
			message = "Python task was not saved. Review and confirm the current executable and script files before saving."
		case errors.Is(err, errPythonTaskFileUnavailable):
			message = "Python task was not saved. The executable or script could not be read within the allowed size limits."
		}
		a.redirectPythonTask(w, r, taskID, message, true)
		return
	}
	a.redirectPythonTask(w, r, savedID, "Python task saved.", false)
}

func validPythonTaskSaveForm(form url.Values) bool {
	allowed := map[string]bool{
		"csrf": true, "python_task_id": true, "python_task_name": true,
		"python_interpreter_path": true, "python_script_path": true,
		"python_task_enabled": true, "confirm_run_as_user": true, "confirm_python_files": true,
		"python_secret_env_name": true, "python_secret_value": true, "python_secret_clear": true,
	}
	for key, values := range form {
		if !allowed[key] || len(values) != 1 {
			return false
		}
	}
	for _, required := range []string{"csrf", "python_task_id", "python_task_name", "python_interpreter_path", "python_script_path", "confirm_run_as_user", "python_secret_env_name", "python_secret_value"} {
		if len(form[required]) != 1 {
			return false
		}
	}
	if values := form["python_secret_clear"]; len(values) == 1 && values[0] != "yes" {
		return false
	}
	if values := form["python_task_enabled"]; len(values) == 1 && values[0] != "yes" {
		return false
	}
	if values := form["confirm_python_files"]; len(values) == 1 && values[0] != "yes" {
		return false
	}
	return true
}

func (a *app) redirectPythonTask(w http.ResponseWriter, r *http.Request, taskID, message string, failed bool) {
	a.setStatus(message, failed)
	if taskID == "" {
		taskID = "new"
	}
	http.Redirect(w, r, "/?python_task_id="+url.QueryEscape(taskID), http.StatusSeeOther)
}

func (a *app) registeredPythonTasks() (pythonTasksResult, error) {
	result := pythonTasksResult{Tasks: []pythonTaskSummary{}}
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("local target settings are invalid")
		}
		for _, task := range cfg.PythonTasks {
			if task.Disabled || validatePythonTask(task) != nil || !pythonTaskIsPinned(task) {
				continue
			}
			result.Tasks = append(result.Tasks, pythonTaskSummary{
				Name: task.Name, Actions: []string{"python_run_registered_task"},
			})
		}
		sort.Slice(result.Tasks, func(i, j int) bool { return result.Tasks[i].Name < result.Tasks[j].Name })
		return nil
	})
	return result, err
}

func (a *app) runRegisteredPythonTask(ctx context.Context, input pythonTaskInput) (pythonTaskResult, error) {
	if ctx == nil || input.Task == "" || len(input.Task) > 80 || input.Task != strings.TrimSpace(input.Task) ||
		strings.IndexFunc(input.Task, unicode.IsControl) >= 0 {
		return pythonTaskResult{}, errors.New("an exact registered Python task name is required")
	}

	var selected pythonTask
	err := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("local task settings are invalid")
		}
		for _, task := range cfg.PythonTasks {
			if task.Name == input.Task && !task.Disabled && validatePythonTask(task) == nil {
				if !pythonTaskIsPinned(task) || verifyPythonTaskFingerprint(task, nil) != nil {
					return errors.New("the registered Python task files changed or are unavailable; no approval was requested")
				}
				selected = task
				return nil
			}
		}
		return errors.New("the requested registered Python task is unavailable")
	})
	if err != nil {
		return pythonTaskResult{}, err
	}
	if pythonTaskTreeIsUnavailable() {
		return pythonTaskResult{Task: selected.Name, Status: "failed", Message: "Registered Python task process cleanup is unavailable; no task was run."}, nil
	}
	approvalCtx, approvalCancel := context.WithTimeout(ctx, pythonTaskApprovalTimeout)
	approvalErr := a.confirmPythonTaskRun(approvalCtx, selected.Name, selected.SecretEnvName)
	approvalCancel()
	if approvalErr != nil {
		return pythonTaskResult{}, approvalErr
	}
	if ctx.Err() != nil {
		return pythonTaskResult{}, errors.New("Python task approval expired or was canceled; no task was run.")
	}

	taskCtx, cancel := context.WithTimeout(ctx, pythonTaskTimeout)
	defer cancel()
	select {
	case pythonTaskSlots <- struct{}{}:
		defer func() { <-pythonTaskSlots }()
	case <-taskCtx.Done():
		status := "timed_out"
		message := "The registered Python task exceeded its 30-second limit before it could start."
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = "cancelled"
			message = "The registered Python task was cancelled before it could start."
		}
		return pythonTaskResult{Task: selected.Name, Status: status, Message: message}, nil
	}
	if taskCtx.Err() != nil {
		status := "timed_out"
		message := "The registered Python task exceeded its 30-second limit before it could start."
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = "cancelled"
			message = "The registered Python task was cancelled before it could start."
		}
		return pythonTaskResult{Task: selected.Name, Status: status, Message: message}, nil
	}
	if pythonTaskTreeIsUnavailable() {
		return pythonTaskResult{Task: selected.Name, Status: "failed", Message: "Registered Python task process cleanup is unavailable; no task was run."}, nil
	}
	command := a.pythonTaskCommand
	if command == nil {
		command = exec.CommandContext
	}
	var process pythonTaskProcess
	var secret []byte
	defer func() { clear(secret) }()
	// Serialize the final config check, secret load, and process start with settings writes.
	// Release the lock before Wait so a running task cannot block settings changes.
	startErr := withConfigLock(func() error {
		cfg, err := readConfig(a.configPath)
		if err != nil {
			return errors.New("local task settings are invalid")
		}
		current := false
		for _, task := range cfg.PythonTasks {
			if task.ID == selected.ID && task == selected && !task.Disabled && validatePythonTask(task) == nil {
				current = true
				break
			}
		}
		if !current {
			return errors.New("the registered Python task changed before it could start")
		}
		if verifyPythonTaskFingerprint(selected, nil) != nil {
			return errors.New("the registered Python task files changed or are unavailable; no task was run")
		}
		if pythonTaskTreeIsUnavailable() {
			return errPythonTaskTreeUnavailable
		}
		if taskCtx.Err() != nil {
			return nil
		}

		secret, err = loadPythonTaskSecret(a.secrets, selected)
		if err != nil {
			return errPythonTaskCredentialUnavailable
		}
		if taskCtx.Err() != nil {
			return nil
		}
		process, err = a.startPythonTaskProcess(taskCtx, selected, command, secret)
		if err != nil {
			if pythonTaskTreeIsUnavailable() {
				return errPythonTaskTreeUnavailable
			}
			return nil
		}
		return nil
	})
	if startErr != nil {
		if errors.Is(startErr, errPythonTaskCredentialUnavailable) {
			return pythonTaskResult{}, errors.New("The registered Python task credential is unavailable; no task was run.")
		}
		if errors.Is(startErr, errPythonTaskTreeUnavailable) {
			return pythonTaskResult{Task: selected.Name, Status: "failed", Message: "Registered Python task process cleanup is unavailable; no task was run."}, nil
		}
		return pythonTaskResult{}, startErr
	}
	if taskCtx.Err() != nil && process == nil {
		status := "timed_out"
		message := "The registered Python task exceeded its 30-second limit before it could start."
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = "cancelled"
			message = "The registered Python task was cancelled before it could start."
		}
		return pythonTaskResult{Task: selected.Name, Status: status, Message: message}, nil
	}
	result := pythonTaskResult{Task: selected.Name, Status: "failed", Message: "The registered Python task failed."}
	if process == nil {
		if pythonTaskTreeIsUnavailable() {
			result.Message = "Registered Python task process cleanup is unavailable; no task was run."
		}
		return result, nil
	}
	output, waitErr := process.Wait()
	if errors.Is(waitErr, errPythonTaskTreeCleanup) {
		clearPythonTaskProcessOutput(&output)
		markPythonTaskTreeUnavailable()
		result.Message = "The registered Python task ended, but process cleanup could not be confirmed. New registered Python tasks are blocked."
		return result, nil
	}
	if errors.Is(waitErr, errPythonTaskOutputUnavailable) {
		result.OutputRedacted = true
	}
	if waitErr == nil {
		result.Status = "completed"
		result.Message = "The registered Python task completed."
	} else if errors.Is(ctx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Status = "cancelled"
		result.Message = "The registered Python task was cancelled."
	} else if errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
		result.Status = "timed_out"
		result.Message = "The registered Python task exceeded its 30-second limit."
	}
	outputResult := sanitizePythonTaskProcessOutput(&output, secret, a.pythonTaskSanitizer)
	result.Stdout = outputResult.Stdout
	result.Stderr = outputResult.Stderr
	result.StdoutTruncated = outputResult.StdoutTruncated
	result.StderrTruncated = outputResult.StderrTruncated
	result.OutputRedacted = result.OutputRedacted || outputResult.OutputRedacted
	return result, nil
}

func loadPythonTaskSecret(store secretStore, task pythonTask) ([]byte, error) {
	if task.SecretRef == "" {
		return nil, nil
	}
	if store == nil || !secretRefPattern.MatchString(task.SecretRef) || !validPythonTaskSecretEnvName(task.SecretEnvName) {
		return nil, errPythonTaskCredentialUnavailable
	}
	value, err := store.Load(task.SecretRef)
	if err != nil || !validPythonTaskSecret(value) {
		clear(value)
		return nil, errPythonTaskCredentialUnavailable
	}
	return value, nil
}

func runPythonTaskProcess(ctx context.Context, task pythonTask, command pythonTaskCommandFactory, secret []byte) error {
	process, err := startPythonTaskProcess(ctx, task, command, secret)
	if err != nil {
		markPythonTaskTreeUnavailableOnError(err)
		return err
	}
	output, err := process.Wait()
	clearPythonTaskProcessOutput(&output)
	markPythonTaskTreeUnavailableOnError(err)
	return err
}

func startPythonTaskProcess(ctx context.Context, task pythonTask, command pythonTaskCommandFactory, secret []byte) (pythonTaskProcess, error) {
	if command == nil || validatePythonTask(task) != nil || verifyPythonTaskFingerprint(task, nil) != nil ||
		(task.SecretRef == "" && len(secret) != 0) || (task.SecretRef != "" && !validPythonTaskSecret(secret)) {
		return nil, errors.New("registered Python task is unavailable")
	}
	if pythonTaskTreeIsUnavailable() {
		return nil, errors.New("registered Python task process cleanup is unavailable")
	}
	cmd := command(ctx, task.InterpreterPath, "-I", "-B", task.ScriptPath)
	if cmd == nil {
		return nil, errors.New("registered Python task is unavailable")
	}
	cmd.Dir = filepath.Dir(task.ScriptPath)
	environment := pythonTaskEnvironment()
	if task.SecretRef != "" {
		environment = append(environment, task.SecretEnvName+"="+string(secret))
	}
	cmd.Env = environment
	cmd.Stdin = nil
	cmd.Stdout = newPythonTaskOutputCollector()
	cmd.Stderr = newPythonTaskOutputCollector()
	cmd.WaitDelay = pythonTaskWaitDelay
	defer func() {
		for index := range environment {
			environment[index] = ""
		}
		clear(environment)
		cmd.Env = nil
	}()
	return startPythonTaskPlatformProcess(ctx, cmd)
}

func pythonTaskEnvironment() []string {
	if runtime.GOOS != "windows" {
		return []string{}
	}

	environment := make([]string, 0, 3)
	for _, key := range []string{"SystemRoot", "TEMP", "TMP"} {
		if value := os.Getenv(key); value != "" {
			environment = append(environment, key+"="+value)
		}
	}
	return environment
}
