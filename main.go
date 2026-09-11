package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "0.1.0"

type shellInput struct {
	Command        string `json:"command" jsonschema:"shell command to run"`
	Workdir        string `json:"workdir,omitempty" jsonschema:"working directory inside the sandbox; relative paths are resolved from the mounted workspace"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"maximum execution time in seconds; zero uses the server default"`
}

type pathInput struct {
	Path string `json:"path" jsonschema:"file path inside the sandbox; relative paths are resolved from the mounted workspace"`
}

type readFileInput struct {
	Path  string `json:"path" jsonschema:"file path inside the sandbox; relative paths are resolved from the mounted workspace"`
	Line  int    `json:"line,omitempty" jsonschema:"1-based first line to return; defaults to 1"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of lines to return; zero reads through end of file"`
}

type writeFileInput struct {
	Path    string `json:"path" jsonschema:"file path inside the sandbox; relative paths are resolved from the mounted workspace"`
	Content string `json:"content" jsonschema:"complete file content"`
}

type edit struct {
	OldText string `json:"oldText" jsonschema:"exact text to replace; must occur exactly once"`
	NewText string `json:"newText" jsonschema:"replacement text"`
}

type editFileInput struct {
	Path  string `json:"path" jsonschema:"file path inside the sandbox; relative paths are resolved from the mounted workspace"`
	Edits []edit `json:"edits" jsonschema:"ordered exact replacements to apply atomically"`
}

type fileOutput struct {
	Content string `json:"content"`
}

type changeOutput struct {
	Path string `json:"path"`
}

func main() {
	workspaceFlag := flag.String("workspace", "", "host directory to mount (default: current working directory)")
	sbxFlag := flag.String("sbx", "sbx", "path to the sbx executable")
	defaultTimeout := flag.Duration("timeout", 5*time.Minute, "default timeout for each tool invocation")
	flag.Parse()

	workspace, err := resolveWorkspace(*workspaceFlag)
	if err != nil {
		log.Fatal(err)
	}

	runner := &sandboxRunner{command: osCommandRunner{}, sbx: *sbxFlag, workspace: workspace}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, *defaultTimeout)
	err = runner.start(startCtx)
	cancelStart()
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelCleanup()
		if err := runner.close(cleanupCtx); err != nil {
			log.Printf("cleanup: %v", err)
		}
	}()

	server := newServer(runner, *defaultTimeout)
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func newServer(runner *sandboxRunner, defaultTimeout time.Duration) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:        "sbx-shell",
		Title:       "Sandboxed shell and file tools",
		Description: "Runs shell and file tools in one sbx sandbox created when the MCP server starts.",
		Version:     version,
	}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{}})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "shell",
		Description: "Run a shell command in the MCP server's sbx sandbox.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input shellInput) (*mcp.CallToolResult, any, error) {
		ctx, cancel := invocationContext(ctx, defaultTimeout, input.TimeoutSeconds)
		defer cancel()
		result, err := runner.exec(ctx, nil, input.Workdir, "/bin/sh", "-lc", "exec 2>&1; "+input.Command)
		if err != nil {
			return nil, nil, err
		}
		output := result.Stdout
		if result.ExitCode != 0 && output == "" {
			output = fmt.Sprintf("command exited with code %d", result.ExitCode)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: output}},
			IsError: result.ExitCode != 0,
		}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a UTF-8 text file in the MCP server's sbx sandbox, optionally selecting a line range.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input readFileInput) (*mcp.CallToolResult, fileOutput, error) {
		line := input.Line
		if line == 0 {
			line = 1
		}
		ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
		result, err := runner.exec(ctx, nil, "", "python3", "-c", readFileScript, input.Path, fmt.Sprint(line), fmt.Sprint(input.Limit))
		if err != nil {
			return nil, fileOutput{}, err
		}
		if result.ExitCode != 0 {
			return nil, fileOutput{}, commandError("read file", result)
		}
		return nil, fileOutput{Content: result.Stdout}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or atomically replace a UTF-8 text file in the MCP server's sbx sandbox. Parent directories are created.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input writeFileInput) (*mcp.CallToolResult, changeOutput, error) {
		payload, err := json.Marshal(input)
		if err != nil {
			return nil, changeOutput{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
		result, err := runner.exec(ctx, bytesReader(payload), "", "python3", "-c", writeFileScript)
		if err != nil {
			return nil, changeOutput{}, err
		}
		if result.ExitCode != 0 {
			return nil, changeOutput{}, commandError("write file", result)
		}
		return nil, changeOutput{Path: input.Path}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "edit_file",
		Description: "Apply ordered exact-text replacements to a UTF-8 file in the MCP server's sbx sandbox. Each oldText must occur exactly once; the write is atomic.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input editFileInput) (*mcp.CallToolResult, changeOutput, error) {
		payload, err := json.Marshal(input)
		if err != nil {
			return nil, changeOutput{}, err
		}
		ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
		result, err := runner.exec(ctx, bytesReader(payload), "", "python3", "-c", editFileScript)
		if err != nil {
			return nil, changeOutput{}, err
		}
		if result.ExitCode != 0 {
			return nil, changeOutput{}, commandError("edit file", result)
		}
		return nil, changeOutput{Path: input.Path}, nil
	})

	return server
}

func invocationContext(parent context.Context, fallback time.Duration, seconds int) (context.Context, context.CancelFunc) {
	if seconds > 0 {
		return context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	}
	return context.WithTimeout(parent, fallback)
}

func commandError(action string, result execResult) error {
	message := result.Stderr
	if message == "" {
		message = result.Stdout
	}
	return fmt.Errorf("%s: exit code %d: %s", action, result.ExitCode, message)
}

func bytesReader(data []byte) *bytes.Reader { return bytes.NewReader(data) }

func resolveWorkspace(value string) (string, error) {
	if value == "" {
		var err error
		value, err = os.Getwd()
		if err != nil {
			return "", fmt.Errorf("get current directory: %w", err)
		}
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", absolute)
	}
	return absolute, nil
}

const readFileScript = `import pathlib, sys
p = pathlib.Path(sys.argv[1])
start, limit = int(sys.argv[2]), int(sys.argv[3])
if start < 1 or limit < 0:
    raise ValueError("line must be positive and limit must not be negative")
with p.open("r", encoding="utf-8", newline="") as f:
    lines = f.readlines()
s = start - 1
end = None if limit == 0 else s + limit
sys.stdout.write("".join(lines[s:end]))
`

const writeFileScript = `import json, os, pathlib, sys, tempfile
data = json.load(sys.stdin)
p = pathlib.Path(data["path"])
p.parent.mkdir(parents=True, exist_ok=True)
fd, tmp = tempfile.mkstemp(dir=p.parent, prefix="." + p.name + ".")
try:
    with os.fdopen(fd, "w", encoding="utf-8", newline="") as f:
        f.write(data["content"])
    os.replace(tmp, p)
except BaseException:
    try: os.unlink(tmp)
    except FileNotFoundError: pass
    raise
`

const editFileScript = `import json, os, pathlib, sys, tempfile
data = json.load(sys.stdin)
if not data["edits"]:
    raise ValueError("at least one edit is required")
p = pathlib.Path(data["path"])
text = p.read_text(encoding="utf-8")
for i, edit in enumerate(data["edits"], 1):
    old = edit["oldText"]
    count = text.count(old)
    if count != 1:
        raise ValueError(f"edit {i}: oldText occurs {count} times; expected exactly once")
    text = text.replace(old, edit["newText"], 1)
fd, tmp = tempfile.mkstemp(dir=p.parent, prefix="." + p.name + ".")
try:
    with os.fdopen(fd, "w", encoding="utf-8", newline="") as f:
        f.write(text)
    os.replace(tmp, p)
except BaseException:
    try: os.unlink(tmp)
    except FileNotFoundError: pass
    raise
`
