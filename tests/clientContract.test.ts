import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { createClient } from "../sdk/src/index";

type Parameter = {
  name: string;
  in: string;
  required?: boolean;
  schema: { type: string; enum?: unknown[]; minimum?: number; maximum?: number; maxLength?: number };
};
const contract = JSON.parse(readFileSync(new URL("../backend/internal/api/openapi.json", import.meta.url), "utf8")) as {
  paths: Record<string, Record<string, { parameters?: Parameter[] }>>;
};

test("OpenAPI parameters are unique and every route placeholder is required", () => {
  for (const [path, operations] of Object.entries(contract.paths)) {
    for (const [method, operation] of Object.entries(operations)) {
      const parameters = operation.parameters ?? [];
      const keys = parameters.map(parameter => `${parameter.in}:${parameter.name}`);
      assert.equal(new Set(keys).size, keys.length, `${method} ${path} has duplicate parameters`);
      for (const match of path.matchAll(/\{([^}]+)\}/g)) {
        assert.ok(parameters.some(parameter => parameter.in === "path" && parameter.name === match[1] && parameter.required), `${method} ${path} is missing required ${match[1]}`);
      }
    }
  }
});

test("OpenAPI describes the query parameters sent by SDK library and log requests", async () => {
  const calls: URL[] = [];
  const client = createClient({ baseURL: "https://api.example", fetch: async (input) => {
    const url = new URL(String(input));
    calls.push(url);
    if (url.pathname === "/api/feed") {
      return Response.json({ items: [], total: 3, feedToken: url.searchParams.get("feedToken") || "new-feed", nextCursor: 2, exhausted: false });
    }
    return Response.json({ items: [], total: 0 });
  } });
  await client.videos.list(2, 12, { q: "sample", tag: "demo", sort: "hot", includeTotal: false });
  await client.videos.getFeed({ kind: "listing", cursor: { feedToken: "", position: 0 }, count: 2, q: "sample", tag: "demo", sort: "latest" });
  await client.videos.getFeed({ kind: "listing", cursor: { feedToken: "existing-feed", position: 1 }, count: 2 });
  await client.admin.listLogs({ from: "2026-01-01T00:00:00Z", to: "2026-01-02T00:00:00Z", source: "http", level: "error", method: "GET", query: "sample", limit: 100, before: 10 });

  for (const url of calls) {
    const parameters = contract.paths[url.pathname]?.get?.parameters;
    assert.ok(parameters, `${url.pathname} has no documented parameters`);
    for (const [name, value] of url.searchParams) {
      const parameter = parameters.find(parameter => parameter.in === "query" && parameter.name === name);
      assert.ok(parameter, `${url.pathname} does not document ${name}`);
      const schema = parameter.schema;
      let typed: string | number | boolean = value;
      if (schema.type === "integer") {
        typed = Number(value);
        assert.ok(Number.isInteger(typed), `${name} must be an integer`);
        if (schema.minimum !== undefined) assert.ok(typed >= schema.minimum);
        if (schema.maximum !== undefined) assert.ok(typed <= schema.maximum);
      } else if (schema.type === "boolean") {
        assert.ok(value === "true" || value === "false");
        typed = value === "true";
      } else {
        assert.equal(schema.type, "string");
      }
      if (schema.enum) assert.ok(schema.enum.includes(typed), `${name} rejects ${value}`);
      if (schema.maxLength !== undefined) assert.ok(value.length <= schema.maxLength);
    }
    for (const parameter of parameters) {
      if (parameter.in === "query" && parameter.required) assert.ok(url.searchParams.has(parameter.name));
    }
  }
});
