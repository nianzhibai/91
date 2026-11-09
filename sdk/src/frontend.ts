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

export type FrontendCurrent = {
  name: string;
  version: string;
  repository: string;
};

export type FrontendUpdateCheck = {
  repository: string;
  current: FrontendCurrent;
  latestVersion: string;
  updateAvailable: boolean;
};

export type FrontendStatus = {
  active: string;
  current?: FrontendCurrent;
  previous?: string;
  builtin?: FrontendManifest;
  installed: FrontendRelease[];
};

export function createFrontendClient(http: Transport) {
  const base = "/admin/api/frontend";
  return {
    status: () => http.request<FrontendStatus>(base),
    checkUpdate: () => http.request<FrontendUpdateCheck>(`${base}/update`),
    install: () => http.request<FrontendRelease>(`${base}/install`, {
      method: "POST",
    }),
  };
}
