# om

Generate a TypeScript MCP server from an OpenAPI document.

## Generate and run

Install with Go 1.27 or newer. Run generated projects with Node.js 22.18 or newer.

```sh
go install github.com/holon-sh/om/cmd/om@latest
om generate https://example.com/openapi.yaml my-api
cd my-api
npm ci
npm start
```

`npm start` serves MCP over Streamable HTTP at `http://127.0.0.1:3000/mcp`. Set `HOST` and `PORT` to listen elsewhere. On a loopback host the server accepts only localhost `Host` and `Origin` headers. The server has no authentication of its own, so put it behind one that authenticates clients before listening beyond loopback. Set the credential environment variables that `om generate` prints before starting the server. Server variables default to the document's values. Override one with `OM_` followed by the variable's name in upper case, with every character other than A–Z and 0–9 replaced by `_`. A value outside the variable's `enum` fails every call.

A document read from a file needs an absolute HTTPS server URL. A document fetched by URL may use a relative one. The output directory must be new.

The server is two files that belong together: `server.ts`, which is the same for every document, and `document.json`, the tools compiled from the document, which `server.ts` reads from its own directory. `package.json` and `package-lock.json` complete the project.

## Supported documents

`om` reads OpenAPI 3.0, 3.1, and 3.2 documents in JSON or YAML. It uses the first document-level server and creates one tool per supported operation, named by its `operationId`, or by its method and path when it has none.

Each parameter becomes a tool argument with the parameter's name, schema, and description. A JSON request body is the argument `body`. `om` handles path, query, header, and cookie parameters with their default serialization, flat `deepObject` query parameters, `application/json` request bodies, API keys, HTTP bearer and HTTP Basic credentials, and schema references into `components.schemas`. `Accept`, `Content-Type`, and `Authorization` header parameters are not arguments.

The server validates arguments as JSON Schema 2020-12 before calling the API. It treats `format` as an annotation and leaves a `pattern` that JavaScript cannot compile to the API. A tool returns the response body as text, up to 1 MiB, and reports redirects as errors without following them.

## Skipped operations

`om` skips each operation it cannot serve and records the reason, for example OAuth-only security, a request body that is not `application/json`, a path- or operation-level server, or two parameters with the same name. `om generate` prints every skipped operation.

Generation fails as a whole only when:

- the document cannot be loaded as OpenAPI JSON or YAML, including when a `$ref` points outside the document or does not resolve;
- it is not OpenAPI 3.0, 3.1, or 3.2, or has no `info` object;
- its first server does not resolve to an HTTPS URL with a valid host and port and no credentials, query, or fragment, or its variables are undeclared or map to the same `OM_` name;
- no operation is supported.

Schemas that no supported operation uses are loaded but not otherwise checked.

## Embed

Call `om.Generate` with the document bytes and, when relative server URLs need resolving, the document's location. The result holds the project files, the document's title, version, and description, the credential and server variables the server reads from the environment, the upstream endpoint, and the skipped operations with their reasons.
