import { createContext, useCallback, useContext, useEffect, useId, useRef, useState, type ReactNode } from "react";

export type ConfirmOptions = {
  // message asks the question and says what follows, such as "Delete
  // region? Runs stop getting it."
  message: string;
  // action labels the button that says yes, such as "Delete".
  action: string;
  // danger marks an action that destroys or cuts access. Focus then
  // starts on Cancel.
  danger?: boolean;
};

type Ask = (o: ConfirmOptions) => Promise<boolean>;

const ConfirmContext = createContext<Ask>(() => Promise.resolve(false));

export const useConfirm = () => useContext(ConfirmContext);

// ConfirmProvider shows one confirm dialog at a time in place of the
// browser's window.confirm. Escape and Cancel answer no.
export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState<ConfirmOptions & { resolve: (yes: boolean) => void }>();
  const dialog = useRef<HTMLDialogElement>(null);
  const id = useId();

  const ask = useCallback<Ask>((o) => new Promise((resolve) => setOpen({ ...o, resolve })), []);
  const answer = (yes: boolean) => {
    open?.resolve(yes);
    setOpen(undefined);
  };

  useEffect(() => {
    const d = dialog.current;
    if (open && d && !d.open) d.showModal();
    if (!open && d?.open) d.close();
  }, [open]);

  return (
    <ConfirmContext.Provider value={ask}>
      {children}
      {open && (
        <dialog
          ref={dialog}
          className="confirm"
          role="alertdialog"
          aria-modal="true"
          aria-describedby={id}
          onCancel={(e) => {
            e.preventDefault();
            answer(false);
          }}
        >
          <p id={id}>{open.message}</p>
          <div className="row confirm-actions">
            <button type="button" autoFocus={open.danger} onClick={() => answer(false)}>
              Cancel
            </button>
            <button type="button" autoFocus={!open.danger} className={open.danger ? "danger" : "primary"} onClick={() => answer(true)}>
              {open.action}
            </button>
          </div>
        </dialog>
      )}
    </ConfirmContext.Provider>
  );
}
