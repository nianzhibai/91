import { createContext, useContext, useEffect, type ReactNode } from "react";
import * as api from "../api";
import { useAdminResource } from "../useAdminResource";
import { reportTelegramAvailabilityError, subscribeTelegramConfigChanges, syncTelegramAvailability } from "./availability";
import { useTelegramAvailability } from "./useTelegramAvailability";

type StatusResource = ReturnType<typeof useAdminResource<api.TelegramStatus | null>>;
const StatusContext = createContext<StatusResource | null>(null);

function loadAvailability(signal: AbortSignal) {
  return syncTelegramAvailability(async () => (await api.getTelegramAvailability(signal)).enabled, signal);
}

// Navigation follows saved configuration; only the workspace reads runtime status.
export function TelegramStatusProvider({ children, workspaceActive, onUnauthorized }: {
  children: ReactNode;
  workspaceActive: boolean;
  onUnauthorized?: () => void;
}) {
  const { enabled } = useTelegramAvailability();
  const configuration = useAdminResource<boolean | null>(loadAvailability, {
    queryKey: "telegram-availability", active: true, initialData: null, intervalMs: null, onUnauthorized,
    retryOnError: enabled === null, maxRetries: 3,
  });
  const status = useAdminResource<api.TelegramStatus | null>(api.getTelegramStatus, {
    queryKey: "telegram-status", active: workspaceActive && enabled === true, initialData: null, onUnauthorized,
    intervalMs: (data) => data?.enabled === false ? null : data?.connection.state === "connecting" ? 5000 : 15_000,
  });
  useEffect(() => {
    if (configuration.error && enabled === null) reportTelegramAvailabilityError();
  }, [configuration.error, enabled]);
  useEffect(() => subscribeTelegramConfigChanges((enabled) => {
    configuration.setData(enabled);
    status.setData((current) => current ? {
      ...current, enabled, connection: { ...current.connection, enabled, config: { ...current.connection.config, enabled } },
    } : current);
    if (enabled) void status.invalidate();
  }), [configuration.setData, status.setData, status.invalidate]);

  return <StatusContext.Provider value={status}>{children}</StatusContext.Provider>;
}

export function useTelegramStatus() {
  const status = useContext(StatusContext);
  if (!status) throw new Error("Telegram status requires the admin layout");
  return status;
}
