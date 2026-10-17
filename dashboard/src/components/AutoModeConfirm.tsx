import { AUTO_MODE_WARNING } from "../approvePlan";
import { ConfirmModal } from "./ConfirmModal";

// The one confirm in front of every switch to `auto`. It was written out five
// times, and the copy on the list's plan approval had drifted into the
// opposite of what the worker does — it told a human git push, gh, kubectl,
// curl, wget and env would still ask, and then approved a mode where none of
// them do. One component, one AUTO_MODE_WARNING, nothing left to drift.
//
// `approving` is the plan-approval variant: the same grant, worded as the
// answer to a plan rather than a bare mode switch.
export function AutoModeConfirm({
  open,
  approving = false,
  onConfirm,
  onCancel,
}: {
  open: boolean;
  approving?: boolean;
  onConfirm: () => void;
  onCancel: () => void;
}) {
  return (
    <ConfirmModal
      open={open}
      title={approving ? "Approve and switch to auto mode?" : "Switch to auto mode?"}
      message={AUTO_MODE_WARNING}
      confirmLabel={approving ? "approve + auto" : "switch to auto"}
      danger={false}
      onConfirm={onConfirm}
      onCancel={onCancel}
    />
  );
}
