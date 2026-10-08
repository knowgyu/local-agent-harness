//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	// PROC_THREAD_ATTRIBUTE_JOB_LIST is missing from x/sys v0.41.0.
	procThreadAttributeJobList   uintptr = 0x0002000d
	pythonTaskTreePollInterval           = 20 * time.Millisecond
	pythonTaskTreeCleanupTimeout         = 3 * time.Second
	pythonTaskOutputDrainTimeout         = 1 * time.Second
	pythonTaskProcessExitCode            = 1
)

type windowsPythonTaskProcess struct {
	process windows.Handle
	job     windows.Handle
	ctx     context.Context
	stdout  *pythonTaskOutputPipe
	stderr  *pythonTaskOutputPipe
}

type pythonTaskOutputPipe struct {
	file      *os.File
	collector *pythonTaskOutputCollector
	done      chan error
	started   bool
	finished  bool
	closeOnce sync.Once
	closeErr  error
}

func newPythonTaskOutputPipe(readHandle windows.Handle, collector *pythonTaskOutputCollector) *pythonTaskOutputPipe {
	return &pythonTaskOutputPipe{
		file:      os.NewFile(uintptr(readHandle), "python-task-output"),
		collector: collector,
		done:      make(chan error, 1),
	}
}

func (p *pythonTaskOutputPipe) start() {
	p.started = true
	go func() {
		_, readErr := io.Copy(p.collector, p.file)
		closeErr := p.close()
		p.done <- errors.Join(readErr, closeErr)
	}()
}

func (p *pythonTaskOutputPipe) close() error {
	if p == nil || p.file == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.closeErr = p.file.Close()
	})
	return p.closeErr
}

func createPythonTaskOutputHandles() (windows.Handle, windows.Handle, error) {
	security := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	var readHandle, writeHandle windows.Handle
	if err := windows.CreatePipe(&readHandle, &writeHandle, &security, 0); err != nil {
		return 0, 0, err
	}
	if err := windows.SetHandleInformation(readHandle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		_ = windows.CloseHandle(readHandle)
		_ = windows.CloseHandle(writeHandle)
		return 0, 0, err
	}
	return readHandle, writeHandle, nil
}

type pythonTaskJobAccounting struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaults           uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

func startPythonTaskPlatformProcess(ctx context.Context, command *exec.Cmd) (pythonTaskProcess, error) {
	if ctx == nil || command == nil || command.Path == "" || len(command.Args) == 0 {
		return nil, errors.New("registered Python task command is unavailable")
	}
	stdoutCollector, stderrCollector, err := pythonTaskOutputCollectors(command.Stdout, command.Stderr)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil || job == 0 {
		if err == nil {
			err = errors.New("Windows returned an invalid job handle")
		}
		return nil, err
	}
	jobOwned := true
	defer func() {
		if jobOwned {
			_ = windows.CloseHandle(job)
		}
	}()

	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, err
	}

	nulSecurity := windows.SecurityAttributes{
		Length:        uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		InheritHandle: 1,
	}
	nulName, err := windows.UTF16PtrFromString("NUL")
	if err != nil {
		return nil, err
	}
	nul, err := windows.CreateFile(nulName, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &nulSecurity, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil || nul == windows.InvalidHandle {
		if nul != windows.InvalidHandle && nul != 0 {
			_ = windows.CloseHandle(nul)
		}
		if err == nil {
			err = errors.New("Windows returned an invalid NUL handle")
		}
		return nil, err
	}
	nulOwned := true
	defer func() {
		if nulOwned {
			_ = windows.CloseHandle(nul)
		}
	}()

	stdoutRead, stdoutWrite, err := createPythonTaskOutputHandles()
	if err != nil {
		return nil, err
	}
	stdoutReadOwned, stdoutWriteOwned := true, true
	defer func() {
		if stdoutReadOwned {
			_ = windows.CloseHandle(stdoutRead)
		}
		if stdoutWriteOwned {
			_ = windows.CloseHandle(stdoutWrite)
		}
	}()
	stderrRead, stderrWrite, err := createPythonTaskOutputHandles()
	if err != nil {
		return nil, err
	}
	stderrReadOwned, stderrWriteOwned := true, true
	defer func() {
		if stderrReadOwned {
			_ = windows.CloseHandle(stderrRead)
		}
		if stderrWriteOwned {
			_ = windows.CloseHandle(stderrWrite)
		}
	}()
	stdoutReadFile := os.NewFile(uintptr(stdoutRead), "python-task-stdout")
	if stdoutReadFile == nil {
		return nil, errPythonTaskOutputUnavailable
	}
	stdoutReadOwned = false
	stdoutReadFileOwned := true
	defer func() {
		if stdoutReadFileOwned && stdoutReadFile != nil {
			_ = stdoutReadFile.Close()
		}
	}()
	stderrReadFile := os.NewFile(uintptr(stderrRead), "python-task-stderr")
	if stderrReadFile == nil {
		return nil, errPythonTaskOutputUnavailable
	}
	stderrReadOwned = false
	stderrReadFileOwned := true
	defer func() {
		if stderrReadFileOwned && stderrReadFile != nil {
			_ = stderrReadFile.Close()
		}
	}()

	application, err := windows.UTF16PtrFromString(command.Path)
	if err != nil {
		return nil, err
	}
	commandLine, err := windows.UTF16FromString(windows.ComposeCommandLine(command.Args))
	if err != nil {
		return nil, err
	}
	workingDirectory, err := windows.UTF16PtrFromString(command.Dir)
	if err != nil {
		return nil, err
	}
	environment, err := pythonTaskWindowsEnvironmentBlock(command.Env)
	if err != nil {
		return nil, err
	}
	defer clear(environment)

	attributes, err := windows.NewProcThreadAttributeList(2)
	if err != nil {
		return nil, err
	}
	defer attributes.Delete()
	jobHandles := []windows.Handle{job}
	if err := attributes.Update(procThreadAttributeJobList, unsafe.Pointer(&jobHandles[0]),
		unsafe.Sizeof(jobHandles[0])); err != nil {
		return nil, err
	}
	standardHandles := []windows.Handle{nul, stdoutWrite, stderrWrite}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
		unsafe.Pointer(&standardHandles[0]), uintptr(len(standardHandles))*unsafe.Sizeof(standardHandles[0])); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	startup := windows.StartupInfoEx{
		StartupInfo: windows.StartupInfo{
			Cb:        uint32(unsafe.Sizeof(windows.StartupInfoEx{})),
			Flags:     windows.STARTF_USESTDHANDLES,
			StdInput:  nul,
			StdOutput: stdoutWrite,
			StdErr:    stderrWrite,
		},
		ProcThreadAttributeList: attributes.List(),
	}
	processInfo := windows.ProcessInformation{}
	creationFlags := uint32(windows.CREATE_DEFAULT_ERROR_MODE | windows.CREATE_UNICODE_ENVIRONMENT |
		windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(application, &commandLine[0], nil, nil, true, creationFlags,
		&environment[0], workingDirectory, &startup.StartupInfo, &processInfo); err != nil {
		return nil, err
	}

	process := &windowsPythonTaskProcess{
		process: processInfo.Process,
		job:     job,
		ctx:     ctx,
		stdout:  &pythonTaskOutputPipe{file: stdoutReadFile, collector: stdoutCollector, done: make(chan error, 1)},
		stderr:  &pythonTaskOutputPipe{file: stderrReadFile, collector: stderrCollector, done: make(chan error, 1)},
	}
	jobOwned = false
	stdoutReadFileOwned = false
	stderrReadFileOwned = false
	stdoutReadFile = nil
	stderrReadFile = nil
	if err := windows.CloseHandle(stdoutWrite); err != nil {
		_ = windows.TerminateJobObject(job, pythonTaskProcessExitCode)
		cleanupErr := process.abortAndClose()
		return nil, errors.Join(errPythonTaskTreeCleanup, err, cleanupErr)
	}
	stdoutWriteOwned = false
	if err := windows.CloseHandle(stderrWrite); err != nil {
		_ = windows.TerminateJobObject(job, pythonTaskProcessExitCode)
		cleanupErr := process.abortAndClose()
		return nil, errors.Join(errPythonTaskTreeCleanup, err, cleanupErr)
	}
	stderrWriteOwned = false
	process.stdout.start()
	process.stderr.start()
	if err := windows.CloseHandle(processInfo.Thread); err != nil {
		_ = windows.TerminateJobObject(job, pythonTaskProcessExitCode)
		cleanupErr := process.abortAndClose()
		return nil, errors.Join(errPythonTaskTreeCleanup, err, cleanupErr)
	}
	if err := windows.CloseHandle(nul); err != nil {
		_ = windows.TerminateJobObject(job, pythonTaskProcessExitCode)
		cleanupErr := process.abortAndClose()
		return nil, errors.Join(errPythonTaskTreeCleanup, err, cleanupErr)
	}
	nulOwned = false
	return process, nil
}

func pythonTaskWindowsEnvironmentBlock(environment []string) ([]uint16, error) {
	entries := append([]string(nil), environment...)
	keys := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("invalid registered Python task environment")
		}
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.HasPrefix(key, "=") {
			return nil, errors.New("invalid registered Python task environment")
		}
		foldedKey := strings.ToUpper(key)
		if _, exists := keys[foldedKey]; exists {
			return nil, errors.New("duplicate registered Python task environment key")
		}
		keys[foldedKey] = struct{}{}
	}
	sort.Slice(entries, func(i, j int) bool {
		left, _, _ := strings.Cut(entries[i], "=")
		right, _, _ := strings.Cut(entries[j], "=")
		return strings.ToUpper(left) < strings.ToUpper(right)
	})
	if len(entries) == 0 {
		return []uint16{0, 0}, nil
	}
	block := make([]uint16, 0, len(entries)*32+1)
	for _, entry := range entries {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			clear(block)
			return nil, errors.New("invalid registered Python task environment")
		}
		block = append(block, encoded...)
	}
	block = append(block, 0)
	return block, nil
}

func (p *windowsPythonTaskProcess) Wait() (pythonTaskProcessOutput, error) {
	if p == nil || p.process == 0 || p.job == 0 {
		return pythonTaskProcessOutput{}, errPythonTaskTreeCleanup
	}
	if err := p.waitForRootExit(); err != nil {
		return pythonTaskProcessOutput{}, p.cleanupAfterFailure(err)
	}
	exitCode, exitErr := p.exitCode()
	treeErr := p.terminateAndWaitForTree()
	if treeErr != nil {
		outputCloseErr := p.closeOutputReaders()
		outputDrainErr := p.waitForOutputReaders()
		closeErr := p.closeHandles()
		p.clearOutputCollectors()
		return pythonTaskProcessOutput{}, errors.Join(errPythonTaskTreeCleanup, exitErr, treeErr, outputCloseErr, outputDrainErr, closeErr)
	}
	outputErr := p.waitForOutputReaders()
	if outputErr != nil {
		outputCloseErr := p.closeOutputReaders()
		closeErr := p.closeHandles()
		p.clearOutputCollectors()
		if errors.Is(outputErr, errPythonTaskOutputDrainTimeout) {
			return pythonTaskProcessOutput{}, errors.Join(errPythonTaskTreeCleanup, outputErr, outputCloseErr, closeErr)
		}
		if closeErr != nil {
			return pythonTaskProcessOutput{}, errors.Join(errPythonTaskTreeCleanup, outputErr, outputCloseErr, closeErr)
		}
		return pythonTaskProcessOutput{}, errors.Join(errPythonTaskOutputUnavailable, outputErr, outputCloseErr)
	}
	closeErr := p.closeHandles()
	if exitErr != nil || closeErr != nil {
		p.clearOutputCollectors()
		return pythonTaskProcessOutput{}, errors.Join(errPythonTaskTreeCleanup, exitErr, closeErr)
	}
	output := pythonTaskProcessOutputFromCollectors(p.stdout.collector, p.stderr.collector)
	if p.ctx.Err() != nil {
		return output, p.ctx.Err()
	}
	if exitCode != 0 {
		return output, fmt.Errorf("registered Python task exited with code %d", exitCode)
	}
	return output, nil
}

func (p *windowsPythonTaskProcess) waitForRootExit() error {
	deadline := time.Time{}
	for {
		if p.ctx.Err() != nil {
			if deadline.IsZero() {
				deadline = time.Now().Add(pythonTaskTreeCleanupTimeout)
				if err := windows.TerminateJobObject(p.job, pythonTaskProcessExitCode); err != nil {
					if !p.processIsSignaled() {
						return err
					}
				}
			}
			if time.Now().After(deadline) {
				return errors.New("root process did not exit after job termination")
			}
		}
		waitResult, err := windows.WaitForSingleObject(p.process, uint32(pythonTaskTreePollInterval/time.Millisecond))
		if err != nil {
			return err
		}
		switch waitResult {
		case windows.WAIT_OBJECT_0:
			return nil
		case uint32(windows.WAIT_TIMEOUT):
		default:
			return fmt.Errorf("unexpected process wait result %d", waitResult)
		}
	}
}

func (p *windowsPythonTaskProcess) processIsSignaled() bool {
	result, err := windows.WaitForSingleObject(p.process, 0)
	return err == nil && result == windows.WAIT_OBJECT_0
}

func (p *windowsPythonTaskProcess) exitCode() (uint32, error) {
	var exitCode uint32
	if err := windows.GetExitCodeProcess(p.process, &exitCode); err != nil {
		return 0, err
	}
	return exitCode, nil
}

func (p *windowsPythonTaskProcess) terminateAndWaitForTree() error {
	active, err := p.activeJobProcesses()
	if err != nil {
		_ = windows.TerminateJobObject(p.job, pythonTaskProcessExitCode)
		return err
	}
	if active == 0 {
		return nil
	}
	if err := windows.TerminateJobObject(p.job, pythonTaskProcessExitCode); err != nil {
		active, queryErr := p.activeJobProcesses()
		if queryErr != nil || active != 0 {
			return errors.Join(err, queryErr)
		}
		return nil
	}

	deadline := time.Now().Add(pythonTaskTreeCleanupTimeout)
	for time.Now().Before(deadline) {
		active, err = p.activeJobProcesses()
		if err != nil {
			return err
		}
		if active == 0 {
			return nil
		}
		time.Sleep(pythonTaskTreePollInterval)
	}
	return errors.New("job still had active processes after termination")
}

func (p *windowsPythonTaskProcess) activeJobProcesses() (uint32, error) {
	var accounting pythonTaskJobAccounting
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicAccountingInformation,
		uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), nil); err != nil {
		return 0, err
	}
	return accounting.ActiveProcesses, nil
}

func (p *windowsPythonTaskProcess) waitForOutputReaders() error {
	var stdoutDone, stderrDone <-chan error
	if p.stdout != nil && p.stdout.started && !p.stdout.finished {
		stdoutDone = p.stdout.done
	}
	if p.stderr != nil && p.stderr.started && !p.stderr.finished {
		stderrDone = p.stderr.done
	}
	if stdoutDone == nil && stderrDone == nil {
		return nil
	}
	timer := time.NewTimer(pythonTaskOutputDrainTimeout)
	defer timer.Stop()
	var result error
	for stdoutDone != nil || stderrDone != nil {
		select {
		case err := <-stdoutDone:
			p.stdout.finished = true
			stdoutDone = nil
			result = errors.Join(result, err)
		case err := <-stderrDone:
			p.stderr.finished = true
			stderrDone = nil
			result = errors.Join(result, err)
		case <-timer.C:
			return errors.Join(errPythonTaskOutputDrainTimeout, result)
		}
	}
	return result
}

func (p *windowsPythonTaskProcess) closeOutputReaders() error {
	var result error
	if p.stdout != nil {
		result = errors.Join(result, p.stdout.close())
	}
	if p.stderr != nil {
		result = errors.Join(result, p.stderr.close())
	}
	return result
}

func (p *windowsPythonTaskProcess) clearOutputCollectors() {
	if p.stdout != nil {
		p.stdout.collector.clear()
	}
	if p.stderr != nil {
		p.stderr.collector.clear()
	}
}

func (p *windowsPythonTaskProcess) cleanupAfterFailure(cause error) error {
	_ = windows.TerminateJobObject(p.job, pythonTaskProcessExitCode)
	deadline := time.Now().Add(pythonTaskTreeCleanupTimeout)
	for time.Now().Before(deadline) {
		if p.processIsSignaled() {
			break
		}
		time.Sleep(pythonTaskTreePollInterval)
	}
	treeErr := p.terminateAndWaitForTree()
	outputCloseErr := p.closeOutputReaders()
	outputDrainErr := p.waitForOutputReaders()
	closeErr := p.closeHandles()
	p.clearOutputCollectors()
	return errors.Join(errPythonTaskTreeCleanup, cause, treeErr, outputCloseErr, outputDrainErr, closeErr)
}

func (p *windowsPythonTaskProcess) abortAndClose() error {
	_ = windows.TerminateJobObject(p.job, pythonTaskProcessExitCode)
	deadline := time.Now().Add(pythonTaskTreeCleanupTimeout)
	for time.Now().Before(deadline) {
		if p.processIsSignaled() {
			break
		}
		time.Sleep(pythonTaskTreePollInterval)
	}
	treeErr := p.terminateAndWaitForTree()
	outputCloseErr := p.closeOutputReaders()
	outputDrainErr := p.waitForOutputReaders()
	closeErr := p.closeHandles()
	p.clearOutputCollectors()
	return errors.Join(treeErr, outputCloseErr, outputDrainErr, closeErr)
}

func (p *windowsPythonTaskProcess) closeHandles() error {
	var result error
	if p.process != 0 {
		if err := windows.CloseHandle(p.process); err != nil {
			result = errors.Join(result, err)
		}
		p.process = 0
	}
	if p.job != 0 {
		if err := windows.CloseHandle(p.job); err != nil {
			result = errors.Join(result, err)
		}
		p.job = 0
	}
	return result
}
