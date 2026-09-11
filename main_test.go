package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordedCall struct {
	name  string
	args  []string
	stdin string
}

type recordingRunner struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (r *recordingRunner) Run(_ context.Context, stdin io.Reader, name string, args ...string) (string, string, error) {
	var input string
	if stdin != nil {
		data, _ := io.ReadAll(stdin)
		input = string(data)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedCall{name: name, args: append([]string(nil), args...), stdin: input})
	if len(args) > 0 && args[0] == "exec" {
		return "hello\n", "", nil
	}
	return "", "", nil
}

func TestSandboxRunnerLifecycle(t *testing.T) {
	commands := &recordingRunner{}
	runner := &sandboxRunner{
		command:   commands,
		sbx:       "/usr/local/bin/sbx",
		workspace: "/work/project",
	}

	if err := runner.start(context.Background()); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		result, err := runner.exec(context.Background(), nil, "subdir", "/bin/sh", "-lc", "pwd")
		if err != nil {
			t.Fatal(err)
		}
		if result.Stdout != "hello\n" || result.ExitCode != 0 {
			t.Fatalf("unexpected result: %#v", result)
		}
	}
	if err := runner.close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(commands.calls) != 4 {
		t.Fatalf("got %d sbx calls, want 4", len(commands.calls))
	}
	create := commands.calls[0]
	if create.name != "/usr/local/bin/sbx" || create.args[0] != "create" {
		t.Fatalf("unexpected create call: %#v", create)
	}
	if !containsSequence(create.args, "shell", "/work/project") {
		t.Fatalf("create does not use the built-in shell and workspace: %q", create.args)
	}

	name := valueAfter(create.args, "--name")
	if !strings.HasPrefix(name, "mcp-shell-") {
		t.Fatalf("unexpected sandbox name %q", name)
	}
	execCall := commands.calls[1]
	if !containsSequence(execCall.args, "exec", "--workdir", "/work/project/subdir", name, "/bin/sh", "-lc", "pwd") {
		t.Fatalf("unexpected exec call: %q", execCall.args)
	}
	secondExecCall := commands.calls[2]
	if !containsSequence(secondExecCall.args, "exec", "--workdir", "/work/project/subdir", name, "/bin/sh", "-lc", "pwd") {
		t.Fatalf("unexpected second exec call: %q", secondExecCall.args)
	}
	remove := commands.calls[3]
	if !containsSequence(remove.args, "rm", "--force", name) {
		t.Fatalf("unexpected remove call: %q", remove.args)
	}
}

func TestServerExposesFourToolsAndInvokesShell(t *testing.T) {
	commands := &recordingRunner{}
	runner := &sandboxRunner{command: commands, sbx: "sbx", workspace: "/workspace"}
	if err := runner.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer runner.close(context.Background())
	server := newServer(runner, time.Minute)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	tools, err := clientSession.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools.Tools))
	}
	for _, name := range []string{"shell", "write_file", "read_file", "edit_file"} {
		found := false
		for _, tool := range tools.Tools {
			found = found || tool.Name == name
		}
		if !found {
			t.Errorf("tool %q not found", name)
		}
	}

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "shell",
		Arguments: map[string]any{"command": "printf hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("shell returned tool error: %#v", result.Content)
	}
	if result.StructuredContent != nil {
		t.Fatalf("shell returned structured content: %#v", result.StructuredContent)
	}
	if len(result.Content) != 1 {
		t.Fatalf("shell returned %d content blocks, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || text.Text != "hello\n" {
		t.Fatalf("unexpected shell content: %#v", result.Content)
	}
	if len(commands.calls) != 2 {
		t.Fatalf("startup and shell made %d sbx calls, want 2", len(commands.calls))
	}
}

func containsSequence(values []string, sequence ...string) bool {
	if len(sequence) > len(values) {
		return false
	}
	for i := 0; i <= len(values)-len(sequence); i++ {
		match := true
		for j := range sequence {
			match = match && values[i+j] == sequence[j]
		}
		if match {
			return true
		}
	}
	return false
}

func valueAfter(values []string, key string) string {
	for i := 0; i+1 < len(values); i++ {
		if values[i] == key {
			return values[i+1]
		}
	}
	return ""
}
