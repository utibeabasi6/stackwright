import { useEffect, useRef, useState, type ButtonHTMLAttributes, type ReactNode } from "react";
import { message } from "./api";

const doneFor = 2000;

export type ActionState = {
  state: "idle" | "busy" | "done";
  error?: string;
  // run calls fn, which reports a failure by returning false or throwing.
  // A thrown error shows beside the button.
  run: (fn: () => unknown) => Promise<void>;
};

export function useAction(): ActionState {
  const [state, setState] = useState<ActionState["state"]>("idle");
  const [error, setError] = useState<string>();
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);
  useEffect(() => () => clearTimeout(timer.current), []);
  const run = async (fn: () => unknown) => {
    clearTimeout(timer.current);
    setError(undefined);
    setState("busy");
    try {
      if ((await fn()) === false) {
        setState("idle");
        return;
      }
      setState("done");
      timer.current = setTimeout(() => setState("idle"), doneFor);
    } catch (e) {
      setState("idle");
      setError(message(e));
    }
  };
  return { state, error, run };
}

type Labels = {
  // busy is shown while the action runs, such as "Saving…"; done for two
  // seconds after it succeeds, such as "Saved".
  busy: string;
  done: string;
  children: ReactNode;
};

// ActionButton shows an action's state on the button: disabled with a
// spinner while it runs, a check when it is done, and an error beside it.
// Screen readers hear each state.
export function ActionButton({ action, busy, done, children, disabled, className, ...rest }: Labels & { action: ActionState } & ButtonHTMLAttributes<HTMLButtonElement>) {
  const { state, error } = action;
  return (
    <>
      <button {...rest} className={["action", className].filter(Boolean).join(" ")} disabled={disabled || state === "busy"} aria-busy={state === "busy"}>
        {state === "busy" ? (
          <>
            <span className="spinner" aria-hidden="true" />
            {busy}
          </>
        ) : state === "done" ? (
          <>
            <span aria-hidden="true">✓ </span>
            {done}
          </>
        ) : (
          children
        )}
      </button>
      <span className="sr-only" aria-live="polite">
        {state === "busy" ? busy : state === "done" ? done : ""}
      </span>
      {error && (
        <span className="error action-error" role="alert">
          {error}
        </span>
      )}
    </>
  );
}

// Action is a button that runs run when clicked.
export function Action({ run, ...props }: Labels & { run: () => unknown } & Omit<ButtonHTMLAttributes<HTMLButtonElement>, "onClick">) {
  const action = useAction();
  return <ActionButton type="button" {...props} action={action} onClick={() => void action.run(run)} />;
}

// Submit is a form's submit button that shows the state of the action that
// the form's submit handler runs through useAction.
export function Submit({ action, ...props }: Labels & { action: ActionState } & ButtonHTMLAttributes<HTMLButtonElement>) {
  return <ActionButton type="submit" {...props} action={action} />;
}
