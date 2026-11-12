import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import type { Option } from "./Select";

type Props = {
  options: Option[];
  name: string;
  type?: string;
  placeholder?: string;
};

// Combobox is a text box that suggests options as you type, and takes any
// text too.
// https://www.w3.org/WAI/ARIA/apg/patterns/combobox/examples/combobox-autocomplete-list/
export function Combobox({ options, name, type = "text", placeholder }: Props) {
  const [text, setText] = useState("");
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const id = useId();
  const list = useRef<HTMLUListElement>(null);
  const query = text.trim().toLowerCase();
  const shown = options.filter(
    (o) => o.value.toLowerCase().includes(query) || String(o.text ?? o.label).toLowerCase().includes(query),
  );
  const expanded = open && shown.length > 0;

  useEffect(() => {
    if (expanded && active >= 0) list.current?.children[active]?.scrollIntoView?.({ block: "nearest" });
  }, [expanded, active]);

  const choose = (i: number) => {
    setText(shown[i].value);
    setOpen(false);
    setActive(-1);
  };

  const keyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    const last = shown.length - 1;
    switch (e.key) {
      case "ArrowDown":
      case "ArrowUp":
        e.preventDefault();
        if (!open) {
          setOpen(true);
          setActive(e.key === "ArrowDown" ? 0 : last);
        } else {
          setActive((a) => (e.key === "ArrowDown" ? Math.min(a + 1, last) : Math.max(a - 1, 0)));
        }
        return;
      case "Enter":
        if (!expanded || active < 0) return;
        e.preventDefault();
        choose(active);
        return;
      case "Escape":
        if (!expanded) return;
        e.preventDefault();
        setOpen(false);
        setActive(-1);
        return;
    }
  };

  return (
    <div className="select">
      <input
        name={name}
        type={type}
        placeholder={placeholder}
        autoComplete="off"
        role="combobox"
        aria-autocomplete="list"
        aria-expanded={expanded}
        aria-controls={`${id}-list`}
        aria-activedescendant={expanded && active >= 0 ? `${id}-${active}` : undefined}
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          setOpen(true);
          setActive(-1);
        }}
        onClick={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onKeyDown={keyDown}
      />
      {expanded && (
        <ul role="listbox" id={`${id}-list`} className="select-list" ref={list}>
          {shown.map((o, i) => (
            <li
              key={o.value}
              id={`${id}-${i}`}
              role="option"
              aria-selected={o.value === text}
              className={i === active ? "active" : undefined}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setActive(i)}
              onClick={(e) => {
                // A wrapping label would pass the click on to the input and reopen the list.
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
