import type { Transport } from "./http.js";
import type { Schema } from "./contract.js";

export type Session = Schema<"Session">;
export type LoginResult = Schema<"LoginResult">;

export function createAuthClient(http: Transport) {
  return {
    setupStatus: () => http.request<{ required: boolean }>("/api/auth/setup"),
    setupAdmin: (username: string, password: string) => http.request<{ ok: boolean }>("/api/auth/setup", {
      method: "POST", body: JSON.stringify({ username, password }),
    }),
    login: (username: string, password: string) => http.request<LoginResult>("/api/auth/login", {
      method: "POST", body: JSON.stringify({ username, password }),
    }),
    logout: () => http.request<{ ok: boolean }>("/api/auth/logout", { method: "POST" }),
    me: () => http.request<Session>("/api/auth/me"),
  };
}
