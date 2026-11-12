import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode } from "react";

export type Option = { value: string; label: ReactNode; text?: string };

type Props = {
  options: Option[];
  // value makes it controlled; defaultValue leaves the value to the Select.
  value?: string;
  defaultValue?: string;
  onChange?: (value: string) => void;
  // name puts the value in a hidden input, for forms that read FormData.
  name?: string;
  disabled?: boolean;
  className?: string;
  "aria-label"?: string;
};

const typeaheadReset = 700;

// Select is a select-only combobox: a button that opens a listbox.
// https://www.w3.org/WAI/ARIA/apg/patterns/combobox/examples/combobox-select-only/
export function Select({ options, value, defaultValue, onChange, name, disabled, className, "aria-label": ariaLabel }: Props) {
  const [own, setOwn] = useState(defaultValue ?? options[0]?.value ?? "");
  const current = value ?? own;
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const id = useId();
  const root = useRef<HTMLDivElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const list = useRef<HTMLUListElement>(null);
  const typed = useRef({ text: "", at: 0 });
  const index = options.findIndex((o) => o.value === current);

  useEffect(() => {
    if (!open) return;
    const outside = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener("mousedown", outside);
    return () => document.removeEventListener("mousedown", outside);
  }, [open]);

  useEffect(() => {
    if (open) list.current?.children[active]?.scrollIntoView?.({ block: "nearest" });
  }, [open, active]);

  const show = () => {
    setActive(Math.max(index, 0));
    setOpen(true);
  };
  const choose = (i: number) => {
    const next = options[i]?.value;
    setOpen(false);
    button.current?.focus();
    if (next === undefined || next === current) return;
    if (value === undefined) setOwn(next);
    onChange?.(next);
  };
  const textOf = (o: Option) => (o.text ?? (typeof o.label === "string" ? o.label : o.value)).toLowerCase();
  const find = (key: string) => {
    const now = Date.now();
    typed.current.text = now - typed.current.at > typeaheadReset ? key : typed.current.text + key;
    typed.current.at = now;
    const from = open ? active : Math.max(index, 0);
    const order = [...options.keys()].map((k) => (from + 1 + k) % options.length);
    return order.find((i) => textOf(options[i]).startsWith(typed.current.text.toLowerCase())) ?? -1;
  };

  const keyDown = (e: KeyboardEvent<HTMLButtonElement>) => {
    const last = options.length - 1;
    if (e.key.length !== 1) typed.current.text = "";
    switch (e.key) {
      case "ArrowDown":
      case "ArrowUp":
        e.preventDefault();
        if (!open) show();
        else setActive((a) => (e.key === "ArrowDown" ? Math.min(a + 1, last) : Math.max(a - 1, 0)));
        return;
      case "Home":
      case "End":
        if (!open) return;
        e.preventDefault();
        setActive(e.key === "Home" ? 0 : last);
        return;
      case "Enter":
      case " ":
        e.preventDefault();
        if (open) choose(active);
        else show();
        return;
      case "Escape":
        if (!open) return;
        e.preventDefault();
        setOpen(false);
        return;
      case "Tab":
        setOpen(false);
        return;
    }
    if (e.key.length === 1 && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const i = find(e.key);
      if (i < 0) return;
      if (open) setActive(i);
      else choose(i);
    }
  };

  return (
    <div className={className ? `select ${className}` : "select"} ref={root}>
      <button
        ref={button}
        id={`${id}-button`}
        type="button"
        role="combobox"
        aria-haspopup="listbox"
        aria-expanded={open}
        aria-controls={`${id}-list`}
        aria-activedescendant={open ? `${id}-${active}` : undefined}
        aria-label={ariaLabel}
        disabled={disabled}
        onClick={() => (open ? setOpen(false) : show())}
        onKeyDown={keyDown}
      >
        <span className="select-value">{options[index]?.label ?? current}</span>
      </button>
      {name && <input type="hidden" name={name} value={current} />}
      {open && (
        <ul role="listbox" id={`${id}-list`} className="select-list" ref={list} aria-labelledby={`${id}-button`}>
          {options.map((o, i) => (
            <li
              key={o.value}
              id={`${id}-${i}`}
              role="option"
              aria-selected={o.value === current}
              className={i === active ? "active" : undefined}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setActive(i)}
              onClick={(e) => {
                // A wrapping label would pass the click on to the button and reopen the list.
                e.preventDefault();
                choose(i);
              }}
            >
              {o.label}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
