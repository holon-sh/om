# om

`om` generates a standalone TypeScript MCP server from an OpenAPI document. `om.go`, `server.go`, `operation.go` and `schema.go` compile the document into `document.json`; `internal/emit/server.ts` is the fixed interpreter that serves it, the same for every document. `cmd/om` is the command.

## Design

- Delete more than you add.
- Follow one documented path, with no fallbacks or defensive checks. An operation `om` cannot serve is skipped with a reason, and generation fails only in the cases the README lists.
- No caches or memoization.
- Write no comments. The only exception is godoc that links the specification a rule implements.
- Never run `go mod tidy`.

## Tests

- Test behavior through `om.Generate` and the generated server, never internals.
- Tests are deterministic.
- `go vet ./...` and `go test -race ./...` check the module. `OM_TEST_NODE=1` also installs and runs a generated server under Node, as CI does.
