import { APIResponseError, type RequestOptions, type Transport } from "../http.js";

export const ADMIN_BASE = "/admin/api";

export type AdminContext = {
  http: Transport;
  request: <T>(path: string, init?: RequestOptions) => Promise<T>;
};

export function createAdminContext(http: Transport): AdminContext {
  return {
    http,
    request: <T>(path: string, init?: RequestOptions) => http.request<T>(ADMIN_BASE + path, init),
  };
}

/** `config.yaml` changed since it was read; reload it before saving again. */
export class ConfigConflictError extends APIResponseError {
  constructor(source: APIResponseError) {
    super(409, source.message || "config.yaml 已被其他操作修改", source.body);
    this.name = "ConfigConflictError";
  }
}
