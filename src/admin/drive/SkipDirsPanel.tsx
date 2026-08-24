import { useCallback, useEffect, useRef, useState } from "react";
import { ChevronRight, FolderX } from "lucide-react";
import * as api from "../api";
import { useToast } from "../ToastContext";
import { SkipDirsLoadingIndicator } from "./SkipDirsLoadingIndicator";

const AUTO_SAVE_DELAY_MS = 300;
const AUTO_SAVE_RETRY_BASE_MS = 1000;
const AUTO_SAVE_RETRY_MAX_MS = 8000;

type SaveStatus = "idle" | "pending" | "saving" | "saved" | "deferred" | "error";

function normalizeDirIds(ids: Iterable<string>): string[] {
  return Array.from(
    new Set(Array.from(ids, (id) => id.trim()).filter(Boolean))
  ).sort();
}

function dirIdsKey(ids: Iterable<string>): string {
  return JSON.stringify(normalizeDirIds(ids));
}

type SkipDirsPanelProps = {
  drive: api.AdminDrive;
  onSaved: (saved: { id: string; skipDirIds: string[] }) => void;
};

export function SkipDirsPanel({ drive, onSaved }: SkipDirsPanelProps) {
  const { show } = useToast();
  const [selected, setSelected] = useState<Set<string>>(
    () => new Set(normalizeDirIds(drive.skipDirIds ?? []))
  );
  const [saveStatus, setSaveStatus] = useState<SaveStatus>("idle");
  const selectedRef = useRef(selected);
  const draftRevisionRef = useRef(0);
  const savedRevisionRef = useRef(0);
  const serverKeyRef = useRef(dirIdsKey(drive.skipDirIds ?? []));
  const saveChainRef = useRef<Promise<void>>(Promise.resolve());
  const saveTimerRef = useRef<number | null>(null);
  const retryAttemptRef = useRef(0);
  const failureToastShownRef = useRef(false);
  const mountedRef = useRef(true);
  const onSavedRef = useRef(onSaved);
  const showRef = useRef(show);
  const scheduleSaveRef = useRef<(delay: number, retrying?: boolean) => void>(
    () => undefined
  );

  onSavedRef.current = onSaved;
  showRef.current = show;

  const enqueueSave = useCallback(() => {
    const driveId = drive.id;
    saveChainRef.current = saveChainRef.current.then(async () => {
      const requestRevision = draftRevisionRef.current;
      if (requestRevision === savedRevisionRef.current) return;

      const requestIds = normalizeDirIds(selectedRef.current);
      if (mountedRef.current) setSaveStatus("saving");

      let response: api.DriveConfigSaveResult & { skipDirIds: string[] };
      try {
        response = await api.setDriveSkipDirIds(driveId, requestIds);
      } catch (error) {
        // A newer edit already has its own debounce timer/queued save. Only
        // retry when the failed request still represents the latest draft.
        if (requestRevision !== draftRevisionRef.current) {
          if (mountedRef.current) setSaveStatus("pending");
          return;
        }
        if (!mountedRef.current) return;

        setSaveStatus("error");
        if (!failureToastShownRef.current) {
          failureToastShownRef.current = true;
          showRef.current(
            error instanceof Error ? error.message : "扫描跳过目录保存失败",
            "error"
          );
        }
        retryAttemptRef.current += 1;
        const retryDelay = Math.min(
          AUTO_SAVE_RETRY_BASE_MS * 2 ** (retryAttemptRef.current - 1),
          AUTO_SAVE_RETRY_MAX_MS
        );
        scheduleSaveRef.current(retryDelay, true);
        return;
      }

      const savedIds = normalizeDirIds(response.skipDirIds ?? []);
      const savedKey = dirIdsKey(savedIds);
      serverKeyRef.current = savedKey;
      retryAttemptRef.current = 0;
      failureToastShownRef.current = false;
      if (mountedRef.current) {
        onSavedRef.current({ id: driveId, skipDirIds: savedIds });
      }

      const currentKey = dirIdsKey(selectedRef.current);
      if (requestRevision === draftRevisionRef.current) {
        const next = new Set(savedIds);
        selectedRef.current = next;
        savedRevisionRef.current = requestRevision;
        if (mountedRef.current) {
          setSelected(next);
          setSaveStatus(response.deferred ? "deferred" : "saved");
        }
        return;
      }

      // The user changed the selection while this request was running. When
      // the latest draft happens to equal the normalized server response, it
      // is already durable; otherwise its debounce timer/queued save wins.
      if (currentKey === savedKey) {
        savedRevisionRef.current = draftRevisionRef.current;
        if (mountedRef.current) {
          setSaveStatus(response.deferred ? "deferred" : "saved");
        }
      } else if (mountedRef.current) {
        setSaveStatus("pending");
      }
    });
  }, [drive.id]);

  const scheduleSave = useCallback(
    (delay: number, retrying = false) => {
      if (saveTimerRef.current !== null) {
        window.clearTimeout(saveTimerRef.current);
      }
      if (mountedRef.current && !retrying) setSaveStatus("pending");
      saveTimerRef.current = window.setTimeout(() => {
        saveTimerRef.current = null;
        enqueueSave();
      }, delay);
    },
    [enqueueSave]
  );
  scheduleSaveRef.current = scheduleSave;

  const serverSkipDirKey = dirIdsKey(drive.skipDirIds ?? []);

  useEffect(() => {
    // Polling may replace drive.skipDirIds with a fresh array every five
    // seconds. Treat it as a server snapshot, not as authority over a dirty
    // local draft. Content equality also avoids resets caused by array identity.
    if (draftRevisionRef.current !== savedRevisionRef.current) return;
    if (serverSkipDirKey === serverKeyRef.current) return;

    const incoming = normalizeDirIds(drive.skipDirIds ?? []);
    const next = new Set(incoming);
    serverKeyRef.current = serverSkipDirKey;
    selectedRef.current = next;
    setSelected(next);
    setSaveStatus("idle");
  }, [drive.skipDirIds, serverSkipDirKey]);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (saveTimerRef.current !== null) {
        window.clearTimeout(saveTimerRef.current);
        saveTimerRef.current = null;
        // Auto-save should not silently lose a click when the user leaves the
        // detail page during the short debounce window.
        enqueueSave();
      }
    };
  }, [enqueueSave]);

  const toggle = useCallback((id: string) => {
    const next = new Set(selectedRef.current);
    if (next.has(id)) {
      next.delete(id);
    } else {
      next.add(id);
    }
    selectedRef.current = next;
    draftRevisionRef.current += 1;
    retryAttemptRef.current = 0;
    setSelected(next);
    scheduleSave(AUTO_SAVE_DELAY_MS);
  }, [scheduleSave]);

  const saveStatusText =
    saveStatus === "idle"
      ? null
      : {
          pending: "保存中…",
          saving: "保存中…",
          saved: "已自动保存并生效",
          deferred: "已保存，任务结束后生效",
          error: "保存失败，正在重试…",
        }[saveStatus];
  const saveStatusClass =
    saveStatus === "error"
      ? "is-error"
      : saveStatus === "saved" || saveStatus === "deferred"
        ? "is-saved"
        : saveStatus === "pending" || saveStatus === "saving"
          ? "is-saving"
          : "";

  return (
    <div className="admin-detail-card">
      <header className="admin-detail-card__title">
        <div className="admin-detail-card__title-left">
          <FolderX size={16} />
          <span>扫描跳过目录</span>
        </div>
        {saveStatusText && (
          <span
            className={`admin-skipdirs-autosave ${saveStatusClass}`.trim()}
            role="status"
            aria-live="polite"
          >
            {saveStatusText}
          </span>
        )}
      </header>

      <div className="admin-detail-tree-container">
        <DirTreeNode
          driveId={drive.id}
          id=""
          name={drive.name || "存储"}
          depth={0}
          initiallyOpen
          ancestorSkipped={false}
          selected={selected}
          onToggle={toggle}
          disabled={false}
        />
      </div>
    </div>
  );
}

type DirTreeNodeProps = {
  driveId: string;
  id: string;
  name: string;
  depth: number;
  initiallyOpen?: boolean;
  ancestorSkipped: boolean;
  selected: Set<string>;
  onToggle: (id: string) => void;
  disabled: boolean;
};

function DirTreeNode({
  driveId,
  id,
  name,
  depth,
  initiallyOpen,
  ancestorSkipped,
  selected,
  onToggle,
  disabled,
}: DirTreeNodeProps) {
  const [open, setOpen] = useState(!!initiallyOpen);
  const [loading, setLoading] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [children, setChildren] = useState<api.DriveDirEntry[]>([]);
  const [error, setError] = useState("");

  const isRoot = depth === 0;
  const isSelected = id !== "" && selected.has(id);
  const dimmed = ancestorSkipped;
  const showLoading = open && !loaded && !error;

  const loadChildren = useCallback(async () => {
    if (loaded || loading) return;
    setLoading(true);
    setError("");
    try {
      const data = await api.listDriveDirChildren(driveId, id || undefined);
      setChildren(data ?? []);
      setLoaded(true);
    } catch (e) {
      setError(e instanceof Error ? e.message : "加载失败");
    } finally {
      setLoading(false);
    }
  }, [driveId, id, loaded, loading]);

  useEffect(() => {
    if (open && !loaded) {
      void loadChildren();
    }
  }, [open, loaded, loadChildren]);

  function handleToggleOpen() {
    setOpen((v) => !v);
  }

  return (
    <div>
      {!isRoot && (
        <div
          className={`admin-skipdirs-row${dimmed && !isSelected ? " is-dimmed" : ""}`}
        >
          <button
            type="button"
            onClick={handleToggleOpen}
            className={`admin-skipdirs-toggle${open ? " is-open" : ""}`}
            aria-label={open ? "折叠" : "展开"}
            aria-expanded={open}
          >
            <ChevronRight size={14} />
          </button>

          <input
            type="checkbox"
            className="admin-skipdirs-checkbox"
            checked={isSelected}
            onChange={() => onToggle(id)}
            disabled={disabled}
            aria-label={`跳过目录 ${name}`}
          />

          <span className="admin-skipdirs-name" title={name} onClick={handleToggleOpen}>
            {name}
          </span>
        </div>
      )}

      {open && (
        <div className={isRoot ? undefined : "admin-skipdirs-children"}>
          {showLoading && <SkipDirsLoadingIndicator />}
          {error && <div className="admin-skipdirs-status is-error">{error}</div>}
          {loaded && !error && children.length === 0 && (
            <div className="admin-skipdirs-status">无子目录</div>
          )}
          {children.map((child) => (
            <DirTreeNode
              key={child.id}
              driveId={driveId}
              id={child.id}
              name={child.name}
              depth={depth + 1}
              ancestorSkipped={ancestorSkipped || isSelected}
              selected={selected}
              onToggle={onToggle}
              disabled={disabled}
            />
          ))}
        </div>
      )}
    </div>
  );
}
