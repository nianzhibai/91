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

  /** Raw access for callers that handle the response themselves. */
  function fetchResponse(path: string, init: RequestInit = {}) {
    return (options.fetch ?? globalThis.fetch)(apiURL(path), { credentials: "include", ...init });
  }

  /**
   * One request whose body `read` consumes before the timeout and the caller's
   * abort listener are released, so neither stops at the response headers.
   */
  async function attempt<T>(path: string, init: RequestInit, timeoutMs: number | undefined, read: (response: Response) => Promise<T>): Promise<T> {
    if (!timeoutMs) {
      const response = await fetchResponse(path, init);
      if (!response.ok) throw await responseError(response);
      return read(response);
    }
    const caller = init.signal;
    const controller = new AbortController();
    const forward = () => controller.abort(caller?.reason);
    if (caller?.aborted) forward();
    else caller?.addEventListener("abort", forward, { once: true });
    const timer = globalThis.setTimeout(
      () => controller.abort(new DOMException("请求超时，请稍后重试", "TimeoutError")),
      timeoutMs,
    );
    try {
      const response = await fetchResponse(path, { ...init, signal: controller.signal });
      if (!response.ok) throw await responseError(response);
      return await read(response);
    } catch (error) {
      // fetch rejects with a generic AbortError; report why the request stopped.
      if (controller.signal.aborted) throw controller.signal.reason;
      throw error;
    } finally {
      globalThis.clearTimeout(timer);
      caller?.removeEventListener("abort", forward);
    }
  }

  /** Requests are never retried unless `retry` is set, which callers use only for reads. */
  async function exchange<T>(path: string, init: RequestOptions, read: (response: Response) => Promise<T>): Promise<T> {
    const { retry, timeoutMs, ...fetchInit } = init;
    const headers = new Headers(fetchInit.headers);
    if (!headers.has("Accept")) headers.set("Accept", "application/json");
    if (typeof fetchInit.body === "string" && !headers.has("Content-Type")) headers.set("Content-Type", "application/json");
    const requestInit: RequestInit = { cache: "no-store", ...fetchInit, headers };
    for (let attempts = 1; ; attempts++) {
      requestInit.signal?.throwIfAborted();
      try {
        return await attempt(path, requestInit, timeoutMs, read);
      } catch (error) {
        if (!retry || attempts >= RETRY_ATTEMPTS || requestInit.signal?.aborted || !retryable(error)) throw error;
      }
      await delay(RETRY_DELAY_MS, requestInit.signal);
    }
  }

  /**
   * Send a request and return the successful response for the caller to read.
   * Failures become `APIResponseError`. The caller's `signal` also cancels the
   * body read; use `request` when a timeout or retry must cover the body.
   */
  function send(path: string, init: RequestInit = {}): Promise<Response> {
    return exchange(path, init, async (response) => response);
  }

  /** Send a request whose successful response is JSON or empty, reading the whole body. */
  function request<T>(path: string, init: RequestOptions = {}): Promise<T> {
    return exchange(path, init, async (response) => {
      const text = await response.text();
      return (text ? JSON.parse(text) : undefined) as T;
    });
  }

  return { apiURL, resourceURL, backendPath, fetch: fetchResponse, send, request };
}

export type RequestOptions = RequestInit & {
  /** Retry once after a network failure, timeout, 408, 425, 429 or 5xx. Use only for reads. */
  retry?: boolean;
  /** Abort with a `TimeoutError` after this many milliseconds. */
  timeoutMs?: number;
};

const RETRY_ATTEMPTS = 2;
const RETRY_DELAY_MS = 200;

function retryable(error: unknown): boolean {
  if (error instanceof APIResponseError) {
    return error.status === 408 || error.status === 425 || error.status === 429 || error.status >= 500;
  }
  return error instanceof TypeError || (error instanceof DOMException && error.name === "TimeoutError");
}

function delay(ms: number, signal?: AbortSignal | null): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(signal.reason);
    const abort = () => { globalThis.clearTimeout(timer); reject(signal!.reason); };
    const timer = globalThis.setTimeout(() => { signal?.removeEventListener("abort", abort); resolve(); }, ms);
    signal?.addEventListener("abort", abort, { once: true });
  });
}

export type Transport = ReturnType<typeof createTransport>;
