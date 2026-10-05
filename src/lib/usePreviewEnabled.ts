import { useSyncExternalStore } from "react";
import { previewController } from "@/lib/previewController";

export function usePreviewEnabled(): boolean {
  return useSyncExternalStore(
    previewController.subscribe,
    previewController.isEnabled,
    () => false
  );
}
