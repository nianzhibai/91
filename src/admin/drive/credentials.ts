import type { Kind } from "./constants";
import {
  ONEDRIVE_AUTH_MODE_CUSTOM_APP,
  ONEDRIVE_AUTH_MODE_OPENLIST_API,
  oneDriveAuthMode,
} from "./onedriveAuth";

type Credentials = Record<string, string>;

function hasCredential(credentials: Credentials, key: string): boolean {
  return Boolean((credentials[key] ?? "").trim());
}

// Older records predate the explicit selector. Infer their mode once when the
// edit form opens so custom-app mounts do not appear as OpenList API mounts.
export function driveCredentialsForForm(
  kind: Kind,
  credentials: Credentials
): Credentials {
  const normalized = { ...credentials };
  if (kind === "onedrive" && !(normalized.auth_mode ?? "").trim()) {
    normalized.auth_mode = oneDriveAuthMode(normalized);
  }
  return normalized;
}

// Authentication-mode validation applies to both create and edit flows.
export function driveCredentialError(
  kind: Kind,
  credentials: Credentials,
  isNew: boolean
): string {
  if (kind === "onedrive") {
    const authMode = oneDriveAuthMode(credentials);
    if (
      authMode !== ONEDRIVE_AUTH_MODE_OPENLIST_API &&
      authMode !== ONEDRIVE_AUTH_MODE_CUSTOM_APP
    ) {
      return "请选择有效的 OneDrive 认证方式";
    }
    const hasClientID = hasCredential(credentials, "client_id");
    const hasClientSecret = hasCredential(credentials, "client_secret");
    if (
      authMode === ONEDRIVE_AUTH_MODE_CUSTOM_APP &&
      (!hasClientID || !hasClientSecret)
    ) {
      return "OneDrive 自建应用的客户端 ID 和客户端密钥必须同时填写";
    }
  }

  if (!isNew) return "";
  return newDriveCredentialError(kind, credentials);
}

// Some drives support mutually exclusive authentication methods, which cannot
// be represented by per-field `required` flags. Keep that rule outside the
// React component so create flows and tests share one source of truth.
export function newDriveCredentialError(
  kind: Kind,
  credentials: Credentials
): string {
  if (kind === "p123") {
    const hasScannedToken = hasCredential(credentials, "access_token");
    const hasUsername = hasCredential(credentials, "username");
    const hasPassword = hasCredential(credentials, "password");
    if (!hasScannedToken && (!hasUsername || !hasPassword)) {
      return "请使用方式一扫码登录，或填写方式二的手机号/邮箱和密码";
    }
  }

  if (kind === "pikpak") {
    const hasRefreshToken = hasCredential(credentials, "refresh_token");
    const hasUsername = hasCredential(credentials, "username");
    const hasPassword = hasCredential(credentials, "password");
    if (!hasRefreshToken && (!hasUsername || !hasPassword)) {
      return "请填写 PikPak 邮箱和密码，或使用方式二的 refresh_token";
    }
  }

  return "";
}

// Build an edit patch from fields the form actually exposes. Runtime-managed
// values may exist in both records, but they must never be echoed back from a
// stale form snapshot unless the user can see and intentionally edit them.
export function changedCredentialValues(
  current: Credentials,
  initial: Credentials,
  editableKeys: readonly string[]
): Credentials {
  const changed: Credentials = {};
  for (const key of new Set(editableKeys)) {
    const currentValue = current[key] ?? "";
    if (currentValue !== (initial[key] ?? "")) {
      changed[key] = currentValue;
    }
  }
  return changed;
}
