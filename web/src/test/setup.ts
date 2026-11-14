import "@testing-library/jest-dom/vitest";

globalThis.scrollTo = () => {};

// Node 25 and newer define a global localStorage of their own, which hides jsdom's.
const dom = (globalThis as { jsdom?: { window: Window } }).jsdom;
if (dom) Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });

// jsdom has <dialog> but not its modal methods.
if (typeof HTMLDialogElement !== "undefined") {
  HTMLDialogElement.prototype.showModal ??= function (this: HTMLDialogElement) {
    this.open = true;
  };
  HTMLDialogElement.prototype.close ??= function (this: HTMLDialogElement) {
    this.open = false;
  };
}
