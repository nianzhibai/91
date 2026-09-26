import { useCallback, useEffect, useRef, useState } from "react";
import { listDriveDirChildren, type DriveDirEntry } from "../api";

type DirectoryState = {
  driveId: string;
  parentId: string;
  status: "idle" | "loading" | "success" | "error";
  children: DriveDirEntry[];
  error: string;
};

export function useDirectoryChildren(
  driveId: string,
  parentId: string,
  open: boolean
) {
  const empty: DirectoryState = {
    driveId, parentId, status: "idle", children: [], error: "",
  };
  const [state, setState] = useState<DirectoryState>(empty);
  const [version, setVersion] = useState(0);
  const attemptRef = useRef<{
    driveId: string;
    parentId: string;
    version: number;
    settled: boolean;
  } | null>(null);

  useEffect(() => {
    if (!open) return;
    const previous = attemptRef.current;
    if (
      previous?.driveId === driveId &&
      previous.parentId === parentId &&
      previous.version === version
    ) {
      return;
    }

    const attempt = { driveId, parentId, version, settled: false };
    attemptRef.current = attempt;
    const controller = new AbortController();
    setState({ driveId, parentId, status: "loading", children: [], error: "" });
    void listDriveDirChildren(driveId, parentId || undefined, controller.signal).then(
      (children) => {
        if (controller.signal.aborted) return;
        attempt.settled = true;
        setState({
          driveId, parentId, status: "success", children: children ?? [], error: "",
        });
      },
      (error: unknown) => {
        if (controller.signal.aborted) return;
        attempt.settled = true;
        setState({
          driveId, parentId, status: "error", children: [],
          error: error instanceof Error && error.message ? error.message : "加载失败",
        });
      }
    );

    return () => {
      controller.abort();
      // Canceled attempts can restart on reopening (also React StrictMode's
      // effect replay). A settled failure stays put until an explicit retry.
      if (!attempt.settled && attemptRef.current === attempt) {
        attemptRef.current = null;
      }
    };
  }, [driveId, parentId, open, version]);

  const retry = useCallback(() => setVersion((current) => current + 1), []);
  // Do not render the previous drive/directory while its replacement loads.
  const current = state.driveId === driveId && state.parentId === parentId ? state : empty;
  return { ...current, retry };
}
