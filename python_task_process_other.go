//go:build !windows

package main

import (
	"context"
	"os/exec"
)

type commandPythonTaskProcess struct {
	command *exec.Cmd
	stdout  *pythonTaskOutputCollector
	stderr  *pythonTaskOutputCollector
}

func startPythonTaskPlatformProcess(_ context.Context, command *exec.Cmd) (pythonTaskProcess, error) {
	stdout, stderr, err := pythonTaskOutputCollectors(command.Stdout, command.Stderr)
	if err != nil {
		return nil, err
	}
	if err := command.Start(); err != nil {
		return nil, err
	}
	return &commandPythonTaskProcess{command: command, stdout: stdout, stderr: stderr}, nil
}

func (p *commandPythonTaskProcess) Wait() (pythonTaskProcessOutput, error) {
	err := p.command.Wait()
	return pythonTaskProcessOutputFromCollectors(p.stdout, p.stderr), err
}
