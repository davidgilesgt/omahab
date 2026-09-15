import { useEffect, useId, useRef, useState } from "react";
import { InlineError } from "./ui";

export function DestructiveConfirm({
  title,
  description,
  confirmValue,
  confirmLabel,
  onConfirm,
  onClose,
  pending,
  error,
}: {
  title: string;
  description: string;
  confirmValue: string;
  confirmLabel: string;
  onConfirm: () => void;
  onClose: () => void;
  pending?: boolean;
  error?: unknown;
}) {
  const [input, setInput] = useState("");
  const dialogRef = useRef<HTMLDialogElement>(null);
  const labelId = useId();
  useEffect(() => {
    const dialog = dialogRef.current;
    const returnFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    if (dialog && !dialog.open) dialog.showModal();
    return () => {
      if (dialog?.open) dialog.close();
      returnFocus?.focus();
    };
  }, []);
  const confirmed = input === confirmValue;
  return (
    <dialog ref={dialogRef} className="modal" aria-labelledby={labelId} onCancel={(event) => { event.preventDefault(); onClose(); }}>
      <header><div><h2 id={labelId}>{title}</h2></div><button type="button" className="icon-button" onClick={onClose} aria-label="Close">×</button></header>
      <div className="form-stack">
        <p>{description}</p>
        <div className="danger-zone">
          <strong>This action cannot be undone.</strong>
          <p>Type <span className="mono">{confirmValue}</span> to continue.</p>
          <label>{confirmLabel} <span className="mono">{confirmValue}</span>
            <input value={input} onChange={(event) => setInput(event.currentTarget.value)} autoComplete="off" spellCheck={false} />
          </label>
        </div>
        <InlineError error={error} />
        <div className="modal-actions">
          <button type="button" className="button secondary" onClick={onClose}>Cancel</button>
          <button type="button" className="button danger" disabled={!confirmed || pending} onClick={onConfirm}>{pending ? "Working…" : "Confirm"}</button>
        </div>
      </div>
    </dialog>
  );
}
