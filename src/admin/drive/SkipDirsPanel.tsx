import { useCallback, useEffect, useRef, useState, type CSSProperties } from "react";
import { ChevronRight, Eye, EyeOff, Folder, FolderOpen } from "lucide-react";
import * as api from "../api";
import { SkipDirsIcon } from "../icons/SkipDirsIcon";
import { useToast } from "@/components/ToastContext";
import { SkipDirsLoadingIndicator } from "./SkipDirsLoadingIndicator";
import { useDirectoryChildren } from "./useDirectoryChildren";

const AUTO_SAVE_DELAY_MS = 300;
const AUTO_SAVE_RETRY_BASE_MS = 1000;
const AUTO_SAVE_RETRY_MAX_MS = 8000;
const SAVE_SUCCESS_DISPLAY_MS = 2000;

type SaveStatus = "idle" | "pending" | "saving" | "saved" | "error";
type RegisterDirectoryFailure = (id: string, retry: () => void) => () => void;

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
  const [failedDirectories, setFailedDirectories] = useState<Map<string, () => void>>(
    () => new Map()
  );
  const hasFailedDirectories = failedDirectories.size > 0;
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

  const registerDirectoryFailure = useCallback<RegisterDirectoryFailure>((id, retry) => {
    setFailedDirectories((current) => new Map(current).set(id, retry));
    return () => {
      setFailedDirectories((current) => {
        if (current.get(id) !== retry) return current;
        const next = new Map(current);
        next.delete(id);
        return next;
      });
    };
  }, []);

  function retryFailedDirectories() {
    for (const retry of failedDirectories.values()) retry();
  }

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
          setSaveStatus("saved");
        }
        return;
      }

      // The user changed the selection while this request was running. When
      // the latest draft happens to equal the normalized server response, it
      // is already durable; otherwise its debounce timer/queued save wins.
      if (currentKey === savedKey) {
        savedRevisionRef.current = draftRevisionRef.current;
        if (mountedRef.current) {
          setSaveStatus("saved");
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

  useEffect(() => {
    if (saveStatus !== "saved") return;
    const timer = window.setTimeout(() => {
      setSaveStatus((current) => current === "saved" ? "idle" : current);
    }, SAVE_SUCCESS_DISPLAY_MS);
    return () => window.clearTimeout(timer);
  }, [drive.id, saveStatus]);

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
          pending: "保存中",
          saving: "保存中",
          saved: "已保存",
          error: "保存失败，正在重试",
        }[saveStatus];
  const saveStatusClass =
    saveStatus === "error"
      ? "is-error"
      : saveStatus === "saved"
        ? "is-saved"
        : saveStatus === "pending" || saveStatus === "saving"
          ? "is-saving"
          : "";

  return (
    <div className="admin-detail-card admin-skipdirs-panel">
      <header className="admin-detail-card__title">
        <div className="admin-detail-card__title-left">
          <SkipDirsIcon />
          <span>扫描跳过目录</span>
        </div>
        <div className={`admin-skipdirs-header-actions${hasFailedDirectories ? " has-retry" : ""}`}>
          {saveStatusText && (
            <span
              className={`admin-skipdirs-autosave ${saveStatusClass}`.trim()}
              role="status"
              aria-live="polite"
              title={saveStatusText}
            >
              {saveStatusText}
            </span>
          )}
          {hasFailedDirectories && (
            <div className="admin-skipdirs-retry-slot">
              <button
                type="button"
                className="admin-btn admin-skipdirs-retry"
                onClick={retryFailedDirectories}
                title="重试加载失败的目录"
              >
                重试
              </button>
            </div>
          )}
        </div>
      </header>

      <div className="admin-detail-tree-container">
        <DirTreeNode
          key={drive.id}
          driveId={drive.id}
          id=""
          name={drive.name || "存储"}
          depth={0}
          initiallyOpen
          ancestorSkipped={false}
          selected={selected}
          onToggle={toggle}
          disabled={false}
          registerFailure={registerDirectoryFailure}
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
  registerFailure: RegisterDirectoryFailure;
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
  registerFailure,
}: DirTreeNodeProps) {
  const [open, setOpen] = useState(!!initiallyOpen);
  const { status, children, error, retry } = useDirectoryChildren(driveId, id, open);
  const loaded = status === "success";

  const isRoot = depth === 0;
  const isSelected = id !== "" && selected.has(id);
  const dimmed = ancestorSkipped || isSelected;
  const visibilityLabel = `${isSelected ? "取消隐藏目录" : "隐藏目录"} ${name}`;
  const showLoading = open && (status === "idle" || status === "loading");

  useEffect(() => {
    if (open && status === "error") return registerFailure(id, retry);
  }, [id, open, status, retry, registerFailure]);

  function handleToggleOpen() {
    setOpen((v) => !v);
  }

  return (
    <div>
      {!isRoot && (
        <div
          className={`admin-skipdirs-row${dimmed ? " is-dimmed" : ""}`}
          style={{ "--depth": depth - 1 } as CSSProperties}
        >
          <button
            type="button"
            onClick={handleToggleOpen}
            className={`admin-skipdirs-toggle${open ? " is-open" : ""}`}
            aria-label={`${open ? "折叠" : "展开"}目录 ${name}`}
            aria-expanded={open}
            title={name}
          >
            <ChevronRight className="admin-skipdirs-chevron" size={14} aria-hidden="true" />
            {open ? (
              <FolderOpen className="admin-skipdirs-folder" size={16} aria-hidden="true" />
            ) : (
              <Folder className="admin-skipdirs-folder" size={16} aria-hidden="true" />
            )}
            <span className="admin-skipdirs-name">{name}</span>
          </button>

          <button
            type="button"
            className={`admin-skipdirs-visibility${isSelected ? " is-hidden" : ""}`}
            aria-pressed={isSelected}
            onClick={() => onToggle(id)}
            disabled={disabled}
            aria-label={visibilityLabel}
            title={ancestorSkipped ? `${visibilityLabel}（父目录已隐藏）` : visibilityLabel}
          >
            {isSelected ? <EyeOff size={16} aria-hidden="true" /> : <Eye size={16} aria-hidden="true" />}
          </button>
        </div>
      )}

      {open && (
        <div
          className={`admin-skipdirs-children${isRoot ? " is-root" : ""}`}
          style={{ "--depth": depth } as CSSProperties}
        >
          {showLoading && <SkipDirsLoadingIndicator />}
          {status === "error" && (
            <div className="admin-skipdirs-status is-error" role="alert">
              {error}
            </div>
          )}
          {loaded && children.length === 0 && (
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
              registerFailure={registerFailure}
            />
          ))}
        </div>
      )}
    </div>
  );
}
