// Shared enabled policy. Individual cards own their playback independently.

type Listener = (enabled: boolean) => void;

// Do not activate previews until the server policy has been loaded.
let enabled = false;
const listeners = new Set<Listener>();

export const previewController = {
  isEnabled(): boolean {
    return enabled;
  },

  setEnabled(next: boolean) {
    if (enabled === next) return;
    enabled = next;
    listeners.forEach((fn) => fn(enabled));
  },

  subscribe(fn: Listener): () => void {
    listeners.add(fn);
    return () => {
      listeners.delete(fn);
    };
  },
};
