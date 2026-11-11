import "@testing-library/jest-dom/vitest";

globalThis.scrollTo = () => {};

// Node 25 and newer define a global localStorage of their own, which hides jsdom's.
const dom = (globalThis as { jsdom?: { window: Window } }).jsdom;
if (dom) Object.defineProperty(globalThis, "localStorage", { value: dom.window.localStorage, configurable: true });
