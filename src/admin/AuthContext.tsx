import {
  ReactNode,
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
} from "react";
import * as api from "./api";
import { applyTelegramEnabled, resetTelegramAvailability } from "./telegram/availability";

type AuthStatus = "loading" | "authed" | "guest" | "unavailable";

type AuthCtx = {
  status: AuthStatus;
  role: string;
  isAdmin: boolean;
  login: (username: string, password: string) => Promise<string | undefined>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
  invalidateSession: () => void;
};

const Ctx = createContext<AuthCtx | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<AuthStatus>("loading");
  const [role, setRole] = useState<string>("");

  const invalidateSession = useCallback(() => {
    resetTelegramAvailability();
    setStatus("guest");
    setRole("");
  }, []);

  const refresh = useCallback(async () => {
    try {
      const r = await api.me();
      if (!r.authenticated) {
        invalidateSession();
        return;
      }
      applyTelegramEnabled(r.role === "admin" && r.telegramEnabled === true);
      setStatus("authed");
      setRole(r.role ?? "");
    } catch {
      // A transport or server failure says nothing about whether the session is
      // valid. Keep an already authenticated screen usable, and use a distinct
      // state during initial loading so route guards do not redirect to login.
      setStatus((current) =>
        current === "authed" ? current : "unavailable"
      );
    }
  }, [invalidateSession]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const login = useCallback(async (u: string, p: string) => {
    const result = await api.login(u, p);
    applyTelegramEnabled(result.role === "admin" && result.telegramEnabled === true);
    setStatus("authed");
    setRole(result.role ?? "");
    return result.role;
  }, []);

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      invalidateSession();
    }
  }, [invalidateSession]);

  const isAdmin = role === "admin";

  const value = useMemo(
    () => ({ status, role, isAdmin, login, logout, refresh, invalidateSession }),
    [status, role, isAdmin, login, logout, refresh, invalidateSession]
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAuth(): AuthCtx {
  const ctx = useContext(Ctx);
  if (!ctx) throw new Error("useAuth must be used inside <AuthProvider>");
  return ctx;
}
