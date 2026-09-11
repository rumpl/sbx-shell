package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type commandRunner interface {
	Run(ctx context.Context, stdin io.Reader, name string, args ...string) (stdout, stderr string, err error)
}

type osCommandRunner struct{}

func (osCommandRunner) Run(ctx context.Context, stdin io.Reader, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND=")
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

type sandboxRunner struct {
	command   commandRunner
	sbx       string
	workspace string

	mu   sync.RWMutex
	name string
}

type execResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func (r *sandboxRunner) start(ctx context.Context) error {
	name, err := sandboxName()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.name != "" {
		return errors.New("sandbox is already started")
	}

	createArgs := []string{"create", "--quiet", "--skills", "off", "--name", name, "shell", r.workspace}
	_, stderr, err := r.command.Run(ctx, nil, r.sbx, createArgs...)
	if err != nil {
		return fmt.Errorf("create sandbox: %w: %s", err, strings.TrimSpace(stderr))
	}
	r.name = name
	return nil
}

func (r *sandboxRunner) close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.name == "" {
		return nil
	}

	_, stderr, err := r.command.Run(ctx, nil, r.sbx, "rm", "--force", r.name)
	if err != nil {
		return fmt.Errorf("remove sandbox %q: %w: %s", r.name, err, strings.TrimSpace(stderr))
	}
	r.name = ""
	return nil
}

func (r *sandboxRunner) exec(ctx context.Context, stdin io.Reader, workdir string, command ...string) (result execResult, err error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	name := r.name
	if name == "" {
		return result, errors.New("sandbox is not started")
	}

	if workdir == "" {
		workdir = r.workspace
	} else if !filepath.IsAbs(workdir) {
		workdir = filepath.Join(r.workspace, workdir)
	}

	args := []string{"exec", "--workdir", filepath.Clean(workdir), name}
	args = append(args, command...)
	result.Stdout, result.Stderr, err = r.command.Run(ctx, stdin, r.sbx, args...)
	if err == nil {
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}
	if status, ok := err.(interface{ Sys() any }); ok {
		if waitStatus, ok := status.Sys().(syscall.WaitStatus); ok {
			result.ExitCode = waitStatus.ExitStatus()
			return result, nil
		}
	}
	return result, fmt.Errorf("exec in sandbox: %w", err)
}

func sandboxName() (string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate sandbox name: %w", err)
	}
	return "mcp-shell-" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "-" + hex.EncodeToString(suffix[:]), nil
}
