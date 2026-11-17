# om

[![CI](https://github.com/holon-sh/om/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/holon-sh/om/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.27%2B-00ADD8?logo=go)](https://go.dev/doc/install)
[![Go Reference](https://pkg.go.dev/badge/github.com/holon-sh/om.svg)](https://pkg.go.dev/github.com/holon-sh/om)
[![MIT License](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Discord](https://img.shields.io/badge/Discord-join-5865F2?logo=discord&logoColor=white)](https://discord.gg/jXExyBdy)

<img src=".github/om.svg" width="100" alt="A Go gopher biting into an OpenAPI document">

----

`om` serves an OpenAPI document as an [MCP server](https://modelcontextprotocol.io/docs/learn/server-concepts), with one tool per API operation.

## Install

```sh
go install github.com/holon-sh/om/cmd/om@latest
```

## Usage

```sh
om serve https://example.com/openapi.yaml
```

The server listens at `http://127.0.0.1:3000/mcp`; set `HOST` and `PORT` to change that. `om serve` lists any operations it skipped and the environment variables that hold your API credentials. Server variables default to the document's values; override one with `OM_` and its name in upper case.

The server has no authentication of its own. Put it behind one before exposing it beyond localhost.

## Embed

Compile a document once, store the model, and serve it wherever you need it:

```go
document, skipped, err := compile.Compile(ctx, data, location)
model, err := json.Marshal(document)

var served om.Document
err = json.Unmarshal(model, &served)
handler, err := served.Handler(os.LookupEnv, http.DefaultTransport)
http.Handle("/mcp", handler)
```

Only `compile` parses OpenAPI, which can take a few hundred MB for a large document. `Handler` needs only the model: `lookup` supplies credentials and server variables by name, and `transport` carries every request to the API. A request that reaches the handler over a loopback connection must carry a localhost `Host` header, which protects it from DNS rebinding. A handler is cheap to build, so a host can build one per request: a call reads only the called operation, and `tools/list` returns the tools in pages of about 256 KiB. It refuses a model whose `om` format it does not know.

## License

[MIT](LICENSE). The om gopher is based on the [Go gopher](https://go.dev/blog/gopher) by Renee French, licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
