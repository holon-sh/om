# om

Serve an OpenAPI document as an MCP server.

## Serve

Install with Go 1.27 or newer.

```sh
go install github.com/holon-sh/om/cmd/om@latest
om serve https://example.com/openapi.yaml
```

`om serve` takes a path or an HTTPS URL, prints the operations it skips, and serves MCP over Streamable HTTP at `http://127.0.0.1:3000/mcp`. Set `HOST` and `PORT` to listen elsewhere. When `HOST` is `127.0.0.1`, `::1`, or `localhost`, the server accepts only localhost `Host` and `Origin` headers. The server has no authentication of its own, so put it behind one that authenticates clients before listening beyond loopback.

Credentials and server variables are read from the environment. `om serve` names each credential variable that is not set. Server variables default to the document's values. Override one with `OM_` followed by the variable's name in upper case, with every character other than A–Z and 0–9 replaced by `_`. A value outside the variable's `enum` fails every call.

A document read from a file needs an absolute HTTPS server URL. A document fetched by URL may use a relative one.

## Supported documents

`om` reads OpenAPI 3.0, 3.1, and 3.2 documents in JSON or YAML. It uses the first document-level server and creates one tool per supported operation, named by its `operationId`, or by its method and path when it has none.

Each parameter becomes a tool argument with the parameter's name, schema, and description. A JSON request body is the argument `body`. `om` handles path, query, header, and cookie parameters with their default serialization, flat `deepObject` query parameters, `application/json` request bodies, API keys, HTTP bearer and HTTP Basic credentials, and schema references into `components.schemas`. `Accept`, `Content-Type`, and `Authorization` header parameters are not arguments.

A tool validates its arguments as JSON Schema 2020-12 before calling the API. It treats `format` as an annotation and leaves a `pattern` that Go's regular expressions cannot compile, such as one with lookaround or a backreference, to the API. A tool returns the response body as text, up to 1 MiB, and reports any other status than 2xx as an error with the status and body. It does not follow redirects and gives up after 30 seconds.

## Skipped operations

`om` skips each operation it cannot serve and records the reason, for example OAuth-only security, a request body that is not `application/json`, a path- or operation-level server, or two parameters with the same name.

Compiling fails as a whole only when:

- the document cannot be loaded as OpenAPI JSON or YAML, including when a `$ref` points outside the document or does not resolve;
- it is not OpenAPI 3.0, 3.1, or 3.2, or has no `info` object;
- its first server does not resolve to an HTTPS URL with a valid host and port and no credentials, query, or fragment, or its variables are undeclared or map to the same `OM_` name;
- no operation is supported.

Schemas that no supported operation uses are loaded but not otherwise checked.

## Embed

Compile a document once and serve the result wherever it is needed:

```go
document, omitted, err := compile.Compile(ctx, data, location) // location resolves a relative server URL
model, err := json.Marshal(document)                            // store the model

var served om.Document
err = json.Unmarshal(model, &served)
handler, err := served.Handler(os.LookupEnv, http.DefaultTransport)
http.Handle("/mcp", handler)
```

`compile` is the only package that parses OpenAPI. Parsing a large document takes several hundred MB, so a host can compile where memory is plentiful and serve elsewhere with `om` alone.

A `Document` holds the title, version, and description, the server URL with each server variable as a `{NAME}` placeholder, the inputs, and one operation per tool. An input is a server variable, which has a default and may have choices, or a secret credential. `Endpoint` returns the host and port the server URL reaches.

`Handler` serves the operations as MCP tools over Streamable HTTP, statelessly, at whatever path it is mounted. `lookup` supplies an input's value by name, and a server variable it has no value for takes its default. `transport` carries every upstream request. The handler checks no `Host` or `Origin` headers and authenticates no one. Building it does no work per tool, and each request registers only the tools it reaches, so a host may build a handler for every request.

The model's `om` field is its format. `Handler` refuses a format this version does not serve; compile the document again.
