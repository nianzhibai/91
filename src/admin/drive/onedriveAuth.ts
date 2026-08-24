export const ONEDRIVE_AUTH_MODE_OPENLIST_API = "openlist_api";
export const ONEDRIVE_AUTH_MODE_CUSTOM_APP = "custom_app";

type Credentials = Record<string, string>;

export function oneDriveAuthMode(credentials: Credentials): string {
  const explicit = (credentials.auth_mode ?? "").trim();
  if (explicit) return explicit;

  const hasCustomClient = Boolean(
    (credentials.client_id ?? "").trim() ||
      (credentials.client_secret ?? "").trim()
  );
  return hasCustomClient
    ? ONEDRIVE_AUTH_MODE_CUSTOM_APP
    : ONEDRIVE_AUTH_MODE_OPENLIST_API;
}
