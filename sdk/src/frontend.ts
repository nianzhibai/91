import type { Transport } from "./http.js";

/** One selectable frontend; each installed frontend name appears once. */
export type Frontend = {
  /** Passed to `activate`. Empty selects the application's default site. */
  id: string;
  /** Manifest name. Empty for a default site without `frontend.json`. */
  name: string;
  version?: string;
  repository?: string;
  /** Shipped with the application. */
  builtin: boolean;
  active: boolean;
};

export type FrontendList = { frontends: Frontend[] };

/** A published release newer than every installed version of a frontend. */
export type FrontendUpdate = { name: string; version: string };

export function createFrontendClient(http: Transport) {
  const base = "/admin/api/frontend";
  const post = (path: string, body: unknown) => http.request<FrontendList>(`${base}/${path}`, {
    method: "POST",
    body: JSON.stringify(body),
  });
  return {
    list: () => http.request<FrontendList>(base),
    /** Reads release metadata only; nothing is downloaded or switched. */
    updates: () => http.request<{ updates: FrontendUpdate[] }>(`${base}/updates`),
    /** Switch to an installed frontend without network access. */
    activate: (id: string) => post("activate", { id }),
    /** Add or update a frontend from its latest GitHub release and switch to it. */
    install: (repository: string) => post("install", { repository }),
    /** Install a locally built `frontend.tar.gz` and switch to it. */
    upload: (archive: Blob) => http.request<FrontendList>(`${base}/upload`, {
      method: "POST",
      headers: { "Content-Type": "application/gzip" },
      body: archive,
    }),
  };
}
