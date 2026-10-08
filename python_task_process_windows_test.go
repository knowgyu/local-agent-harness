//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestPythonTaskWindowsProcessTreeLifecycle(t *testing.T) {
	tests := []struct {
		name          string
		script        string
		cancel        bool
		deadline      time.Duration
		wantExitError bool
	}{
		{name: "root exit terminates descendants", script: "spawn-tree"},
		{name: "failed root terminates descendants", script: "spawn-tree-fail", wantExitError: true},
		{name: "caller cancellation terminates descendants", script: "spawn-tree-wait", cancel: true},
		{name: "deadline terminates descendants", script: "spawn-tree-wait", deadline: 2 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			interpreter, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(root, "task.py")
			if err := os.WriteFile(script, []byte(test.script), 0o600); err != nil {
				t.Fatal(err)
			}
			task := pythonTask{
				ID: "python:" + strings.Repeat("e", 32), Name: "Synthetic process tree",
				InterpreterPath: interpreter, ScriptPath: script,
			}
			task = pinPythonTaskForTest(t, task)

			var ctx context.Context
			var cancel context.CancelFunc
			if test.deadline > 0 {
				ctx, cancel = context.WithTimeout(context.Background(), test.deadline)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			command := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
				return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", script, "root")
			}
			done := make(chan error, 1)
			go func() {
				done <- runPythonTaskProcess(ctx, task, command, nil)
			}()

			handles := waitForPythonTaskProcessTree(t, script)
			defer closePythonTaskProcessHandles(t, handles)
			switch {
			case test.cancel:
				cancel()
			case test.deadline > 0:
			case test.wantExitError:
				writePythonTaskTreeRelease(t, script)
			case test.name == "root exit terminates descendants":
				writePythonTaskTreeRelease(t, script)
			}

			select {
			case err := <-done:
				if test.wantExitError && err == nil {
					t.Fatal("failed root process returned nil")
				}
				if !test.wantExitError && test.deadline == 0 && !test.cancel && err != nil {
					t.Fatalf("successful root process returned %v", err)
				}
				if test.cancel && !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("caller context error = %v, want canceled", ctx.Err())
				}
				if test.deadline > 0 && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
					t.Fatalf("deadline context error = %v, want deadline exceeded", ctx.Err())
				}
			case <-time.After(8 * time.Second):
				t.Fatal("registered task process tree did not finish")
			}
			waitForPythonTaskProcessHandlesToSignal(t, handles)
		})
	}
}

func TestPythonTaskWindowsEnvironmentBlockIsExplicitAndSorted(t *testing.T) {
	block, err := pythonTaskWindowsEnvironmentBlock([]string{"ZZ_TEST=last", "aa_test=first"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := windows.UTF16ToString(block), "aa_test=first"; got != want {
		t.Fatalf("first environment entry = %q, want %q", got, want)
	}
	if len(block) == 0 || block[len(block)-1] != 0 || block[len(block)-2] != 0 {
		t.Fatalf("environment block is not double-null terminated: %#v", block)
	}
	empty, err := pythonTaskWindowsEnvironmentBlock(nil)
	if err != nil || len(empty) != 2 || empty[0] != 0 || empty[1] != 0 {
		t.Fatalf("empty environment block = %#v, err=%v", empty, err)
	}
	if _, err := pythonTaskWindowsEnvironmentBlock([]string{"Path=one", "PATH=two"}); err == nil {
		t.Fatal("case-insensitive duplicate environment keys were accepted")
	}
}

func TestPythonTaskWindowsSlotsCountWholeProcessTrees(t *testing.T) {
	root := t.TempDir()
	interpreter, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	tasks := make([]pythonTask, 3)
	scripts := make([]string, len(tasks))
	for index := range tasks {
		scripts[index] = filepath.Join(root, fmt.Sprintf("task-%d.py", index))
		if err := os.WriteFile(scripts[index], []byte("spawn-tree-wait"), 0o600); err != nil {
			t.Fatal(err)
		}
		tasks[index] = pythonTask{
			ID:   "python:" + strings.Repeat(string(rune('a'+index)), 32),
			Name: fmt.Sprintf("Synthetic task %d", index), InterpreterPath: interpreter, ScriptPath: scripts[index],
		}
		tasks[index] = pinPythonTaskForTest(t, tasks[index])
	}
	if err := writeConfig(configPath, config{Version: configVersion, PythonTasks: tasks}); err != nil {
		t.Fatal(err)
	}

	type runResult struct {
		result pythonTaskResult
		err    error
	}
	start := func(index int, ctx context.Context) <-chan runResult {
		done := make(chan runResult, 1)
		a := &app{
			configPath: configPath,
			openApprovalBrowser: func(address string) error {
				return submitPythonTaskApproval(t, address, "allow", nil, interpreter, scripts[index])
			},
			pythonTaskCommand: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				return exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPythonTaskSubprocessHelper$", "--", args[len(args)-1], "root")
			},
		}
		go func() {
			result, err := a.runRegisteredPythonTask(ctx, pythonTaskInput{Task: tasks[index].Name})
			done <- runResult{result: result, err: err}
		}()
		return done
	}
	waitForCompletion := func(done <-chan runResult) runResult {
		t.Helper()
		select {
		case result := <-done:
			return result
		case <-time.After(10 * time.Second):
			t.Fatal("registered Python task did not finish")
			return runResult{}
		}
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	done1 := start(0, ctx1)
	done2 := start(1, ctx2)
	handles1 := waitForPythonTaskProcessTree(t, scripts[0])
	defer closePythonTaskProcessHandles(t, handles1)
	handles2 := waitForPythonTaskProcessTree(t, scripts[1])
	defer closePythonTaskProcessHandles(t, handles2)
	done3 := start(2, ctx3)
	time.Sleep(250 * time.Millisecond)
	if _, err := os.Stat(scripts[2] + ".tree.ready"); err == nil {
		t.Fatal("third task tree launched while both task slots were occupied")
	}

	cancel1()
	if result := waitForCompletion(done1); result.err != nil || result.result.Status != "cancelled" {
		t.Fatalf("first task result=%#v err=%v, want cancellation", result.result, result.err)
	}
	handles3 := waitForPythonTaskProcessTree(t, scripts[2])
	defer closePythonTaskProcessHandles(t, handles3)
	select {
	case result := <-done2:
		t.Fatalf("second task ended before cancellation: result=%#v err=%v", result.result, result.err)
	default:
	}

	cancel2()
	cancel3()
	if result := waitForCompletion(done2); result.err != nil || result.result.Status != "cancelled" {
		t.Fatalf("second task result=%#v err=%v, want cancellation", result.result, result.err)
	}
	if result := waitForCompletion(done3); result.err != nil || result.result.Status != "cancelled" {
		t.Fatalf("third task result=%#v err=%v, want cancellation", result.result, result.err)
	}
	waitForPythonTaskProcessHandlesToSignal(t, handles1)
	waitForPythonTaskProcessHandlesToSignal(t, handles2)
	waitForPythonTaskProcessHandlesToSignal(t, handles3)
}

func waitForPythonTaskProcessTree(t *testing.T, script string) []windows.Handle {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(script + ".tree.ready"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(script + ".tree.ready"); err != nil {
		t.Fatalf("synthetic process tree did not become ready: %v", err)
	}
	handles := make([]windows.Handle, 0, 3)
	for _, name := range []string{"root", "child", "grandchild"} {
		pidBytes, err := os.ReadFile(script + "." + name + ".pid")
		if err != nil {
			closePythonTaskProcessHandles(t, handles)
			t.Fatalf("read %s pid: %v", name, err)
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(string(pidBytes)), 10, 32)
		if err != nil || pid == 0 {
			closePythonTaskProcessHandles(t, handles)
			t.Fatalf("parse %s pid %q: %v", name, pidBytes, err)
		}
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			closePythonTaskProcessHandles(t, handles)
			t.Fatalf("open %s process handle: %v", name, err)
		}
		handles = append(handles, handle)
	}
	return handles
}

func writePythonTaskTreeRelease(t *testing.T, script string) {
	t.Helper()
	if err := os.WriteFile(script+".root.release", []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func waitForPythonTaskProcessHandlesToSignal(t *testing.T, handles []windows.Handle) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		allSignaled := true
		for _, handle := range handles {
			result, err := windows.WaitForSingleObject(handle, 0)
			if err != nil {
				t.Fatalf("wait on process handle: %v", err)
			}
			if result != windows.WAIT_OBJECT_0 {
				allSignaled = false
			}
		}
		if allSignaled {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("one or more synthetic process tree members were still running")
}

func closePythonTaskProcessHandles(t *testing.T, handles []windows.Handle) {
	t.Helper()
	for _, handle := range handles {
		if err := windows.CloseHandle(handle); err != nil {
			t.Errorf("close synthetic process handle: %v", err)
		}
	}
}
