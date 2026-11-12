import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Action } from "./Action";

vi.mock("./api", () => ({ message: (e: Error) => e.message }));

afterEach(() => vi.useRealTimers());

test("an action shows busy, then done for two seconds, then its label again", async () => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  let finish = () => {};
  const run = () => new Promise<void>((r) => (finish = r));
  render(
    <Action run={run} busy="Saving…" done="Saved">
      Save
    </Action>,
  );
  await userEvent.click(screen.getByRole("button", { name: "Save" }));
  const button = screen.getByRole("button", { name: "Saving…" });
  expect(button).toBeDisabled();
  expect(button).toHaveAttribute("aria-busy", "true");
  expect(screen.getByText("Saving…", { selector: "[aria-live]" })).toBeInTheDocument();

  await act(async () => finish());
  expect(screen.getByRole("button", { name: "Saved" })).toBeEnabled();
  expect(screen.getByText("Saved", { selector: "[aria-live]" })).toBeInTheDocument();
  await act(async () => vi.advanceTimersByTime(2000));
  expect(screen.getByRole("button", { name: "Save" })).toBeInTheDocument();
});

test("an action that reports false goes back without a check, and one that throws shows why", async () => {
  const outcomes: (() => unknown)[] = [() => false, () => Promise.reject(new Error("the server said no"))];
  render(
    <Action run={() => outcomes.shift()!()} busy="Removing…" done="Removed">
      Remove
    </Action>,
  );
  await userEvent.click(screen.getByRole("button", { name: "Remove" }));
  expect(screen.getByRole("button", { name: "Remove" })).toBeEnabled();
  expect(screen.queryByRole("alert")).toBeNull();

  await userEvent.click(screen.getByRole("button", { name: "Remove" }));
  expect(screen.getByRole("alert")).toHaveTextContent("the server said no");
  expect(screen.getByRole("button", { name: "Remove" })).toBeEnabled();
});
