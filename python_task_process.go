package main

import (
	"context"
	"errors"
	"sync/atomic"
)

var errPythonTaskTreeCleanup = errors.New("registered Python task process tree cleanup could not be confirmed")
var errPythonTaskTreeUnavailable = errors.New("registered Python task process cleanup is unavailable")

type pythonTaskProcess interface {
	Wait() (pythonTaskProcessOutput, error)
}

type pythonTaskProcessStarter func(context.Context, pythonTask, pythonTaskCommandFactory, []byte) (pythonTaskProcess, error)

var pythonTaskTreeUnavailable atomic.Bool

func pythonTaskTreeIsUnavailable() bool {
	return pythonTaskTreeUnavailable.Load()
}

func markPythonTaskTreeUnavailable() {
	pythonTaskTreeUnavailable.Store(true)
}

func markPythonTaskTreeUnavailableOnError(err error) {
	if errors.Is(err, errPythonTaskTreeCleanup) {
		markPythonTaskTreeUnavailable()
	}
}

func (a *app) startPythonTaskProcess(ctx context.Context, task pythonTask, command pythonTaskCommandFactory, secret []byte) (pythonTaskProcess, error) {
	starter := a.pythonTaskStarter
	if starter == nil {
		starter = startPythonTaskProcess
	}
	process, err := starter(ctx, task, command, secret)
	markPythonTaskTreeUnavailableOnError(err)
	return process, err
}
