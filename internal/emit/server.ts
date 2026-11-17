import { Buffer } from "node:buffer";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import type { AddressInfo } from "node:net";
import { localhostHostValidation, localhostOriginValidation, toNodeHandler } from "@modelcontextprotocol/node";
import {
  createMcpHandler, fromJsonSchema, McpServer,
  type CallToolResult, type JSONObject, type JSONValue, type ToolAnnotations,
} from "@modelcontextprotocol/server";
import { AjvJsonSchemaValidator } from "@modelcontextprotocol/server/validators/ajv";
import { Ajv2020 } from "ajv/dist/2020.js";

type Method = "DELETE" | "GET" | "HEAD" | "OPTIONS" | "PATCH" | "POST" | "PUT";
type Parameter = { name: string; explode: boolean } & (
  | { in: "path" | "header"; style: "simple" }
  | { in: "query"; style: "form" | "deepObject" }
  | { in: "cookie"; style: "form" }
);
type Credential =
  | { kind: "apiKey"; in: "query" | "header" | "cookie"; name: string; env: string }
  | { kind: "bearer"; env: string }
  | { kind: "basic"; usernameEnv: string; passwordEnv: string };
type Operation = {
  name: string;
  title?: string;
  description?: string;
  method: Method;
  path: string;
  inputSchema: JSONObject;
  parameters: Parameter[];
  body: boolean;
  security: Credential[];
};
type GeneratedDocument = {
  title: string;
  version: string;
  server: string;
  variables: { env: string; default: string; allowed?: string[] }[];
  operations: Operation[];
};
type Pair = [name: string, value: string];
type ParameterValue =
  | { kind: "scalar"; value: string }
  | { kind: "array"; values: string[] }
  | { kind: "object"; entries: Pair[] };

const document: GeneratedDocument = JSON.parse(readFileSync(new URL("document.json", import.meta.url), "utf8"));
const annotations = {
  DELETE: { idempotentHint: true },
  GET: { readOnlyHint: true },
  HEAD: { readOnlyHint: true },
  OPTIONS: { readOnlyHint: true },
  PATCH: {},
  POST: {},
  PUT: { idempotentHint: true },
} satisfies Record<Method, ToolAnnotations>;

const unenforcedPattern = { test: () => true };
const validator = new AjvJsonSchemaValidator(new Ajv2020({
  strict: false,
  allErrors: true,
  validateSchema: false,
  validateFormats: false,
  unicodeRegExp: false,
  code: {
    regExp: Object.assign((pattern: string, flags: string) => {
      try {
        return new RegExp(pattern, flags);
      } catch {
        return unenforcedPattern;
      }
    }, { code: "unenforcedPattern" }),
  },
}));
const tools = document.operations.map(operation => ({
  operation,
  inputSchema: fromJsonSchema<JSONObject>(operation.inputSchema, validator),
}));

const handler = createMcpHandler(() => {
  const server = new McpServer({ name: document.title, title: document.title, version: document.version });
  for (const { operation, inputSchema } of tools) {
    server.registerTool(operation.name, {
      title: operation.title,
      description: operation.description,
      annotations: annotations[operation.method],
      inputSchema,
    }, (args, context) => call(operation, args, context.mcpReq.signal));
  }
  return server;
}, { onerror: console.error });

const host = process.env.HOST ?? "127.0.0.1";
const loopback = ["127.0.0.1", "::1", "localhost"].includes(host);
const serve = toNodeHandler(handler, { onerror: console.error });
const validateHost = localhostHostValidation();
const validateOrigin = localhostOriginValidation();
const server = createServer((request, response) => {
  if (loopback && (!validateHost(request, response) || !validateOrigin(request, response))) return;
  if (request.url?.split("?", 1)[0] === "/mcp") {
    void serve(request, response);
  } else {
    response.writeHead(404).end();
  }
});
server.listen(Number(process.env.PORT ?? 3000), host, () => {
  const { port } = server.address() as AddressInfo;
  process.stdout.write(`MCP server listening at http://${host.includes(":") ? `[${host}]` : host}:${port}/mcp\n`);
});

class RequestFailure extends Error {}

async function call(operation: Operation, args: JSONObject, signal: AbortSignal): Promise<CallToolResult> {
  try {
    const response = await fetch(upstreamRequest(operation, args, signal));
    if (response.status >= 300 && response.status < 400) {
      await response.body?.cancel().catch(() => {});
      return failure(`Upstream returned HTTP ${response.status}. Redirects are disabled.`);
    }
    const text = await responseText(response);
    if (!response.ok) return failure(text ? `HTTP ${response.status}: ${text}` : `HTTP ${response.status}`);
    return { content: text ? [{ type: "text", text }] : [] };
  } catch (error) {
    return failure(error instanceof RequestFailure ? error.message : "Could not complete the upstream request.");
  }
}

function upstreamRequest(operation: Operation, args: JSONObject, signal: AbortSignal): Request {
  const url = serverURL();
  const pathValues = new Map<string, string>();
  const query = new Map<string, string[]>();
  const cookies = new Map<string, string[]>();
  const headers = new Headers();
  for (const parameter of operation.parameters) {
    if (!Object.hasOwn(args, parameter.name)) continue;
    const value = parameterValue(args[parameter.name], parameter.in === "header" ? String : encode);
    if (!value) continue;
    switch (parameter.in) {
      case "path":
        pathValues.set(parameter.name, simpleValue(value, parameter.explode));
        break;
      case "header":
        headers.set(parameter.name, simpleValue(value, parameter.explode));
        break;
      case "query":
      case "cookie": {
        const fields = parameter.in === "query" ? query : cookies;
        for (const [name, encoded] of namedValues(parameter, value)) {
          const values = fields.get(name) ?? [];
          values.push(encoded);
          fields.set(name, values);
        }
        break;
      }
    }
  }
  for (const credential of operation.security) {
    switch (credential.kind) {
      case "apiKey": {
        const value = requiredEnv(credential.env);
        if (credential.in === "header") {
          headers.set(credential.name, value);
        } else if (credential.in === "query") {
          query.set(encode(credential.name), [encode(value)]);
        } else {
          if (!/^[\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]+$/.test(value)) {
            throw new RequestFailure(`Environment variable ${credential.env} is not a valid cookie value.`);
          }
          cookies.set(encode(credential.name), [value]);
        }
        break;
      }
      case "bearer":
        headers.set("Authorization", `Bearer ${requiredEnv(credential.env)}`);
        break;
      case "basic": {
        const username = requiredEnv(credential.usernameEnv);
        if (username.includes(":")) {
          throw new RequestFailure(`Environment variable ${credential.usernameEnv} must be a username without a colon.`);
        }
        const password = requiredEnv(credential.passwordEnv);
        headers.set("Authorization", `Basic ${Buffer.from(`${username}:${password}`).toString("base64")}`);
        break;
      }
    }
  }
  const path = operation.path.replace(/\{([^{}]+)\}/g, (_, name: string) => {
    const value = pathValues.get(name);
    if (value === undefined) throw new RequestFailure(`Path parameter ${name} must specify a value.`);
    return value;
  });
  if (path.split("/").some(part => /^(?:\.|%2e){1,2}$/i.test(part))) {
    throw new RequestFailure("The request path contains a dot segment.");
  }
  url.pathname = url.pathname.replace(/\/$/, "") + path;
  url.search = joinPairs(query, "&");
  if (cookies.size) headers.set("Cookie", joinPairs(cookies, "; "));
  let body: string | undefined;
  if (operation.body && args.body !== undefined) {
    body = JSON.stringify(args.body);
    headers.set("Content-Type", "application/json");
  }
  return new Request(url, {
    method: operation.method, headers, body, redirect: "manual",
    signal: AbortSignal.any([signal, AbortSignal.timeout(30_000)]),
  });
}

function serverURL(): URL {
  let server = document.server;
  for (const variable of document.variables) {
    const value = process.env[variable.env] ?? variable.default;
    if ((variable.allowed && !variable.allowed.includes(value)) || value === "." || value === "..") {
      throw new RequestFailure(`Environment variable ${variable.env} is not a permitted server value.`);
    }
    server = server.replaceAll(`{${variable.env}}`, encode(value));
  }
  const url = URL.parse(server);
  if (!url) throw new RequestFailure("The server variables do not form a valid API server URL.");
  return url;
}

function requiredEnv(name: string): string {
  const value = process.env[name];
  if (!value) throw new RequestFailure(`Missing credential environment variable ${name}.`);
  return value;
}

function encode(value: string): string {
  return encodeURIComponent(value).replace(/[!'()*]/g, char => `%${char.charCodeAt(0).toString(16).toUpperCase()}`);
}

function parameterValue(value: JSONValue | undefined, encodePart: (value: string) => string): ParameterValue | undefined {
  function scalar(part: NonNullable<JSONValue>): string {
    if (typeof part === "object") throw new RequestFailure("Parameters must be scalars or flat collections.");
    return encodePart(String(part));
  }
  if (value == null) return undefined;
  if (Array.isArray(value)) {
    const values = value.filter(part => part !== null).map(scalar);
    return values.length ? { kind: "array", values } : undefined;
  }
  if (typeof value === "object") {
    const entries: Pair[] = [];
    for (const [key, part] of Object.entries(value)) {
      if (part !== null) entries.push([encodePart(key), scalar(part)]);
    }
    return entries.length ? { kind: "object", entries } : undefined;
  }
  return { kind: "scalar", value: scalar(value) };
}

function simpleValue(value: ParameterValue, explode: boolean): string {
  switch (value.kind) {
    case "scalar": return value.value;
    case "array": return value.values.join(",");
    case "object": return explode ? value.entries.map(([key, part]) => `${key}=${part}`).join(",") : value.entries.flat().join(",");
  }
}

function namedValues(parameter: Parameter, value: ParameterValue): Pair[] {
  const name = encode(parameter.name);
  if (parameter.style === "deepObject") {
    if (value.kind !== "object") throw new RequestFailure("A deepObject parameter must be an object.");
    return value.entries.map(([key, part]) => [`${name}%5B${key}%5D`, part]);
  }
  if (parameter.explode) {
    if (value.kind === "object") return value.entries;
    if (value.kind === "array") return value.values.map(part => [name, part]);
  }
  return [[name, simpleValue(value, false)]];
}

function joinPairs(fields: Map<string, string[]>, delimiter: string): string {
  return Array.from(fields, ([name, values]) => values.map(value => `${name}=${value}`)).flat().join(delimiter);
}

async function responseText(response: Response): Promise<string> {
  if (!response.body) return "";
  const reader = response.body.getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > 1024 * 1024) throw new RequestFailure("The upstream response exceeds 1 MiB.");
      chunks.push(value);
    }
    try {
      return new TextDecoder("utf-8", { fatal: true }).decode(Buffer.concat(chunks, size));
    } catch {
      const media = response.headers.get("Content-Type");
      throw new RequestFailure(media ? `The upstream response (${media}) is not UTF-8.` : "The upstream response is not UTF-8.");
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
}

function failure(text: string): CallToolResult {
  return { content: [{ type: "text", text }], isError: true };
}
