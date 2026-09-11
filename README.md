# sbx-shell

An MCP stdio server that exposes four tools in a reusable [Docker Sandbox](https://github.com/docker/sandboxes):

- `shell` — run a `/bin/sh -lc` command
- `write_file` — atomically create or replace a UTF-8 file
- `read_file` — read a UTF-8 file, optionally by line range
- `edit_file` — atomically apply ordered exact-text replacements

The server follows this lifecycle:

1. On startup, run `sbx create shell` using sbx's built-in shell agent.
2. Mount the server's launch directory at the same absolute path in the sandbox.
3. Reuse that sandbox for every tool call with `sbx exec`.
4. On process exit or termination, run `sbx rm --force`.

File changes persist because the workspace is a bind mount. State elsewhere in the sandbox persists between tool calls and is discarded when the MCP server exits.

## Requirements

- Go 1.24 or later
- `sbx` on `PATH`
- Docker Sandboxes set up and able to use `docker/sandbox-templates:shell-docker`

## Build and run

```sh
go build -o bin/sbx-shell .
./bin/sbx-shell
```

By default, the process's current directory is mounted. Options:

```text
-workspace PATH   host directory to mount
-sbx PATH         sbx executable (default: sbx)
-timeout DURATION default per-invocation timeout (default: 5m)
```

## MCP client configuration

Build first, then configure the client to launch the binary from this repository:

```json
{
  "mcpServers": {
    "sbx-shell": {
      "command": "/absolute/path/to/sbx-shell/bin/sbx-shell",
      "args": [
        "-workspace",
        "/absolute/path/to/workspace"
      ]
    }
  }
}
```

Omit `-workspace` to mount the MCP server process's current working directory.

## Development

```sh
gofmt -w *.go
go test ./...
go vet ./...
```
