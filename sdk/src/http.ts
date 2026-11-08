export type ClientOptions = {
  /** Backend origin, for example https://video.example.com. Empty means same origin. */
  baseURL?: string;
  /** Supply a cookie-aware fetch in non-browser environments. */
  fetch?: typeof fetch;
};

export class APIResponseError extends Error {
  readonly code?: string;
  constructor(readonly status: number, message: string, readonly body?: unknown) {
    super(message);
    this.name = "APIResponseError";
    if (body && typeof body === "object" && "code" in body && typeof body.code === "string") {
      this.code = body.code;
    }
  }
}

export class UnauthorizedError extends APIResponseError {
  constructor(body?: unknown) {
    super(401, "unauthorized", body);
    this.name = "UnauthorizedError";
  }
}

export async function responseError(response: Response): Promise<APIResponseError> {
  const text = await response.text().catch(() => "");
  let message = text;
  let body: unknown;
  try {
    body = JSON.parse(text);
    if (body && typeof body === "object") {
      const parsed = body as { error?: unknown; message?: unknown };
      if (typeof parsed.error === "string") message = parsed.error;
      else if (typeof parsed.message === "string") message = parsed.message;
    }
  } catch { /* A proxy may return plain text instead of JSON. */ }
  if (response.status === 401) return new UnauthorizedError(body);
  if (response.headers.get("Content-Type")?.includes("text/html") || /<!doctype\s+html\b|<html\b|<head\b|<body\b/i.test(message)) {
    message = response.status >= 500
      ? `网站服务暂时不可用（HTTP ${response.status}），请稍后重试`
      : `请求失败（HTTP ${response.status}），请稍后重试`;
  }
  return new APIResponseError(response.status, message || `HTTP ${response.status}`, body);
}

/** Only API requests carry credentials. External media URLs are never fetched here. */
export function createTransport(options: ClientOptions = {}) {
  let baseURL = "";
  if (options.baseURL) {
    const url = new URL(options.baseURL);
    if (!/^https?:$/.test(url.protocol) || url.username || url.password || url.pathname !== "/" || url.search || url.hash) {
      throw new Error("baseURL must be an HTTP(S) origin without credentials, path, query or fragment");
    }
    baseURL = url.origin;
  }

  function apiURL(path: string): string {
    if (!path.startsWith("/") || path.startsWith("//") || path.includes("\\") || /[\u0000-\u0020\u007f]/.test(path)) {
      throw new Error("Expected a backend-relative API path");
    }
    return baseURL + path;
  }

  function resourceURL(path: string): string {
    if (!path) return path;
    if (path.startsWith("/") && !path.startsWith("//")) return apiURL(path);
    const url = new URL(path);
    if (!/^https?:$/.test(url.protocol) || url.username || url.password) {
      throw new Error("Expected an HTTP(S) resource URL");
    }
    return url.href;
  }

  function backendPath(value: string): string {
    const origin = baseURL || globalThis.location?.origin;
    const url = new URL(value, origin || "http://localhost");
    if (url.origin !== (origin || "http://localhost") || url.username || url.password || !url.pathname.startsWith("/api/")) {
      throw new Error("Expected an API URL belonging to this backend");
    }
    return url.pathname + url.search;
  }

  function fetchResponse(path: string, init: RequestInit = {}) {
    return (options.fetch ?? globalThis.fetch)(apiURL(path), { credentials: "include", ...init });
  }

  async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
    const headers = new Headers(init.headers);
    if (!headers.has("Accept")) headers.set("Accept", "application/json");
    if (typeof init.body === "string" && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    const response = await fetchResponse(path, { cache: "no-store", ...init, headers });
    if (!response.ok) throw await responseError(response);
    if (response.status === 204) return undefined as T;
    if (response.headers.get("Content-Type")?.includes("application/json")) return response.json() as Promise<T>;
    return await response.text() as T;
  }

  return { apiURL, resourceURL, backendPath, fetch: fetchResponse, request };
}

export type Transport = ReturnType<typeof createTransport>;
