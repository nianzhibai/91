import * as api from "../api";
import { Modal } from "../Modal";

export function DeleteDriveModal({
  drive,
  deleting,
  onCancel,
  onConfirm,
}: {
  drive: api.AdminDrive | null;
  deleting: boolean;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const name = drive?.name || drive?.id || "";
  const title = "删除存储";
  const primaryText = deleting ? "删除中..." : "确认";

  return (
    <Modal
      open={!!drive}
      title={title}
      onClose={onCancel}
      className="admin-modal--delete-confirm"
      footer={
        <>
          <button className="admin-btn" onClick={onCancel} disabled={deleting}>
            取消
          </button>
          <button className="admin-btn" onClick={onConfirm} disabled={deleting}>
            {primaryText}
          </button>
        </>
      }
    >
      <div className="admin-confirm is-message-centered">
        <div className="admin-confirm__content">
          <p className="admin-confirm__message">{`确定要删除「${name}」吗？正在运行的任务将先自动停止，完全退出后再删除。`}</p>
        </div>
      </div>
    </Modal>
  );
}
