import { useRef, useState, type CSSProperties, type KeyboardEvent, type PointerEvent } from "react";

const keyStep = 16;

// useSavedWidth keeps a pane's width for this browser. Undefined means the
// pane's width from the tokens.
export function useSavedWidth(key: string) {
  const [width, setWidth] = useState<number | undefined>(() => {
    try {
      const saved = Number(window.localStorage.getItem(key));
      return saved > 0 ? saved : undefined;
    } catch {
      return undefined;
    }
  });
  const save = (w: number | undefined) => {
    setWidth(w);
    try {
      if (w === undefined) window.localStorage.removeItem(key);
      else window.localStorage.setItem(key, String(Math.round(w)));
    } catch {
      // Storage can be refused; the width then lasts until reload.
    }
  };
  return [width, save] as const;
}

export const paneWidth = (w: number | undefined): CSSProperties | undefined =>
  w === undefined ? undefined : ({ "--pane-width": `${Math.round(w)}px` } as CSSProperties);

type Props = {
  label: string;
  pane: () => HTMLElement | null;
  // edge is the pane's side that the divider is on.
  edge: "start" | "end";
  min: number;
  // keep is the width that the rest of the row keeps at least.
  keep: number;
  onResize: (width: number | undefined) => void;
};

// Divider resizes the pane beside it by drag or by arrow keys; a double
// click or Enter gives the pane its default width back.
export function Divider({ label, pane, edge, min, keep, onResize }: Props) {
  const drag = useRef<{ x: number; width: number; max: number }>(undefined);
  const [now, setNow] = useState<{ width: number; max: number }>();
  const sign = edge === "end" ? 1 : -1;
  const measure = () => {
    const el = pane();
    const width = el?.getBoundingClientRect().width ?? min;
    const max = Math.max(min, (el?.parentElement?.clientWidth ?? 0) - keep);
    return { width, max };
  };
  const resize = (width: number, max: number) => {
    const w = Math.min(Math.max(width, min), max);
    setNow({ width: w, max });
    onResize(w);
  };
  const onPointerDown = (e: PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.currentTarget.setPointerCapture(e.pointerId);
    drag.current = { x: e.clientX, ...measure() };
  };
  const onPointerMove = (e: PointerEvent<HTMLDivElement>) => {
    const d = drag.current;
    if (d) resize(d.width + sign * (e.clientX - d.x), d.max);
  };
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const m = measure();
    switch (e.key) {
      case "ArrowLeft":
      case "ArrowRight":
        resize(m.width + sign * (e.key === "ArrowRight" ? keyStep : -keyStep), m.max);
        break;
      case "Home":
        resize(min, m.max);
        break;
      case "End":
        resize(m.max, m.max);
        break;
      case "Enter":
        onResize(undefined);
        break;
      default:
        return;
    }
    e.preventDefault();
  };
  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label={label}
      aria-valuemin={min}
      aria-valuemax={Math.round(now?.max ?? min)}
      aria-valuenow={Math.round(now?.width ?? min)}
      tabIndex={0}
      className="divider"
      onFocus={() => setNow(measure())}
      onPointerDown={onPointerDown}
      onPointerMove={onPointerMove}
      onPointerUp={() => (drag.current = undefined)}
      onPointerCancel={() => (drag.current = undefined)}
      onDoubleClick={() => onResize(undefined)}
      onKeyDown={onKeyDown}
    />
  );
}
