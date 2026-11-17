# om

Generate a TypeScript MCP server from an OpenAPI document.

## Generate and run

Install with Go 1.27 or newer. Run generated projects with Node.js 22.18 or newer.

```sh
go install github.com/holon-sh/om/cmd/om@v0.0.1
om generate https://example.com/openapi.yaml my-api
cd my-api
npm ci
npm start
```

Connect an MCP client to `http://127.0.0.1:3000/mcp` using Streamable HTTP. Set the credential environment variables printed by `om generate` before starting the server. Server variables use the defaults from the document and can be overridden with their corresponding `OM_` environment variables. Use `PORT` to select a different port and `HOST` to select an interface. The default interface is loopback. When hosting the exported handler remotely, configure authentication at the host.

You can also pass a local JSON or YAML document with an absolute HTTPS server URL. The output directory must be new.

## Supported documents

`om` supports OpenAPI 3.0, 3.1, and 3.2. It uses the first document-level HTTPS server and creates one tool per supported operation. Tool schemas group parameters under `path`, `query`, `header`, and `cookie`, with a separate `body` for JSON requests. It handles default parameter serialization, flat `deepObject` query parameters, API keys, bearer tokens, and HTTP Basic. It uses local references to `components.schemas`.

The generator prints a reason for each skipped operation and fails when no supported operations remain. OAuth, external references, per-operation servers, and non-JSON request bodies are outside this subset.

## Embed

Call `om.Generate` with the document bytes, their location if relative server URLs need resolving, and the project name. The result includes the generated files, required inputs, upstream endpoint, and skipped operations.
