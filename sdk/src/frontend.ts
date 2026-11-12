import type { Transport } from "./http.js";
import type { Schema } from "./contract.js";

/** One selectable frontend; each installed frontend name appears once. */
export type Frontend = Schema<"Frontend">;
export type FrontendList = Schema<"FrontendList">;
/** A published release newer than every installed version of a frontend. */
export type FrontendUpdate = Schema<"FrontendUpdate">;

export function createFrontendClient(http: Transport) {
  const base = "/admin/api/frontend";
  const post = (path: string, body: unknown) => http.request<FrontendList>(`${base}/${path}`, {
    method: "POST",
    body: JSON.stringify(body),
  });
  return {
    list: () => http.request<FrontendList>(base),
    /** Reads release metadata only; nothing is downloaded or switched. */
    updates: () => http.request<Schema<"FrontendUpdates">>(`${base}/updates`),
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
