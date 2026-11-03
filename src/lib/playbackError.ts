type StreamErrorPayload = {
  code?: unknown;
  message?: unknown;
};

export type PlaybackDiagnosticOptions = {
  baseHref?: string;
  fetch?: typeof fetch;
  signal?: AbortSignal;
};

export type PlaybackFailure = {
  message: string;
  kind: "access" | "throttled" | "network";
};

const MAX_SERVER_MESSAGE_LENGTH = 500;

/**
 * Ask the same-origin playback endpoint why a media element failed.
 *
 * HTMLMediaElement deliberately hides the HTTP response body, so a 502 is
 * otherwise surfaced as MEDIA_ERR_SRC_NOT_SUPPORTED. The diagnostic request
 * fetches only byte 0 and never follows a cloud-drive redirect.
 */
export async function diagnosePlaybackSource(
  src: string,
  options: PlaybackDiagnosticOptions = {}
): Promise<string | null> {
  return (await diagnosePlaybackFailure(src, options))?.message ?? null;
}

export async function diagnosePlaybackFailure(
  src: string,
  options: PlaybackDiagnosticOptions = {}
): Promise<PlaybackFailure | null> {
  const baseHref = options.baseHref ?? window.location.href;
  const pageURL = new URL(baseHref);
  const sourceURL = new URL(src, pageURL);
  if (!isSameOriginPlaybackURL(sourceURL, pageURL)) return null;

  const fetcher = options.fetch ?? window.fetch.bind(window);
  try {
    const response = await fetcher(sourceURL.href, {
      method: "GET",
      headers: {
        Accept: "application/json",
        Range: "bytes=0-0",
      },
      credentials: "same-origin",
      cache: "no-store",
      redirect: "manual",
      signal: options.signal,
    });

    // A 200/206 means the backend source is reachable; status 0 is the opaque
    // response produced for a manual cross-origin CDN redirect. In both cases
    // the original browser codec/network message remains the best diagnosis.
    if (response.status === 0 || response.status < 400) return null;

    const serverError = await readServerError(response);
    // Provider failures use a structured code inside HTTP 502 responses.
    const kind = serverError.code === "playback_source_expired" ? "network"
      : response.status === 429 || serverError.code === "drive_rate_limited"
      ? "throttled"
      : [401, 403, 404, 410].includes(response.status) ||
          ["drive_auth_failed", "drive_source_not_found"].includes(serverError.code)
        ? "access"
        : "network";
    if (serverError.message) return { message: serverError.message, kind };

    let message: string;
    switch (response.status) {
      case 401:
        message = "当前登录状态已失效，请重新登录后播放";
        break;
      case 403:
        message = "当前账号无权访问该视频源";
        break;
      case 404:
      case 410:
        if (sourceURL.pathname.startsWith("/p/share/")) {
          message = "分享播放凭证已失效，请重新打开有效的一次性分享链接";
          break;
        }
        message = "视频文件不存在或已失效，请联系管理员重新扫描";
        break;
      case 429:
        message = "视频源当前正在限流，请稍后重试";
        break;
      default:
        if (response.status >= 500) {
          message = `视频源服务暂时不可用（HTTP ${response.status}），请稍后重试或联系管理员`;
        } else {
          message = `视频源请求失败（HTTP ${response.status}），请稍后重试`;
        }
    }
    return { message, kind };
  } catch {
    if (options.signal?.aborted) return null;
    return null;
  }
}

export function isSameOriginPlaybackURL(sourceURL: URL, pageURL: URL) {
  if (sourceURL.origin !== pageURL.origin) return false;
  return (
    sourceURL.pathname.startsWith("/p/stream/") ||
    sourceURL.pathname.startsWith("/p/playback/") ||
    sourceURL.pathname.startsWith("/p/hls/") ||
    sourceURL.pathname.startsWith("/p/upload/") ||
    sourceURL.pathname.startsWith("/p/share/")
  );
}

async function readServerError(response: Response) {
  const empty = { code: "", message: "" };
  const contentType = response.headers.get("Content-Type")?.toLowerCase() ?? "";
  if (!contentType.includes("application/json")) return empty;
  try {
    const payload = (await response.json()) as StreamErrorPayload;
    const message = typeof payload?.message === "string" ? payload.message.trim() : "";
    return {
      code: typeof payload?.code === "string" ? payload.code : "",
      message: message.length <= MAX_SERVER_MESSAGE_LENGTH ? message : "",
    };
  } catch {
    return empty;
  }
}
