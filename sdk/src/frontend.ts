import type { Transport } from "./http.js";

export type FrontendManifest = {
  formatVersion: 1;
  name: string;
  version: string;
  apiMin: string;
  apiMaxExclusive: string;
};

export type FrontendRelease = FrontendManifest & {
  repository?: string;
  sha256: string;
  contentHash: string;
  installedAt: string;
};

export type FrontendStatus = {
  active: string;
  previous?: string;
  builtin?: FrontendManifest;
  installed: FrontendRelease[];
};

export function createFrontendClient(http: Transport) {
  const base = "/admin/api/frontend";
  return {
    status: () => http.request<FrontendStatus>(base),
    install: (repository: string, version: string) => http.request<FrontendRelease>(`${base}/install`, {
      method: "POST", body: JSON.stringify({ repository, version }),
    }),
    activate: (id: string) => http.request<FrontendStatus>(`${base}/active`, {
      method: "PUT", body: JSON.stringify({ id }),
    }),
    rollback: () => http.request<FrontendStatus>(`${base}/rollback`, { method: "POST" }),
    reset: () => http.request<FrontendStatus>(`${base}/reset`, { method: "POST" }),
  };
}
