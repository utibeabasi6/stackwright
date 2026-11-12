import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Select } from "./Select";

const fruit = [
  { value: "apple", label: "Apple" },
  { value: "banana", label: "Banana" },
  { value: "blueberry", label: "Blueberry" },
  { value: "cherry", label: "Cherry" },
];

function Form({ onChange }: { onChange?: (v: string) => void }) {
  return (
    <form aria-label="form">
      <label>
        Fruit <Select name="fruit" defaultValue="banana" options={fruit} onChange={onChange} />
      </label>
      <button type="button">Elsewhere</button>
    </form>
  );
}

const value = () => new FormData(screen.getByRole("form", { name: "form" }) as HTMLFormElement).get("fruit");

afterEach(cleanup);

test("a click opens the list and an option sets the value", async () => {
  const changes: string[] = [];
  render(<Form onChange={(v) => changes.push(v)} />);
  const box = screen.getByRole("combobox", { name: "Fruit" });
  expect(box).toHaveTextContent("Banana");
  expect(box).toHaveAttribute("aria-expanded", "false");
  await userEvent.click(box);
  expect(box).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByRole("option", { name: "Banana" })).toHaveAttribute("aria-selected", "true");
  await userEvent.click(screen.getByRole("option", { name: "Cherry" }));
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  expect(box).toHaveTextContent("Cherry");
  expect(box).toHaveFocus();
  expect(value()).toBe("cherry");
  expect(changes).toEqual(["cherry"]);
});

test("the keyboard moves, chooses and closes", async () => {
  render(<Form />);
  const box = screen.getByRole("combobox", { name: "Fruit" });
  box.focus();
  await userEvent.keyboard("{ArrowDown}");
  expect(screen.getByRole("listbox")).toBeInTheDocument();
  expect(box.getAttribute("aria-activedescendant")).toBe(screen.getByRole("option", { name: "Banana" }).id);
  await userEvent.keyboard("{ArrowDown}{ArrowDown}{Enter}");
  expect(value()).toBe("cherry");
  await userEvent.keyboard("{ArrowDown}{Home}{Enter}");
  expect(value()).toBe("apple");
  await userEvent.keyboard("{ArrowDown}{End}{Escape}");
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  expect(value()).toBe("apple");
});

test("typing finds an option by its first letters", async () => {
  render(<Form />);
  const box = screen.getByRole("combobox", { name: "Fruit" });
  box.focus();
  await userEvent.keyboard("c");
  expect(value()).toBe("cherry");
  await userEvent.keyboard("{Enter}bl{Enter}");
  expect(value()).toBe("blueberry");
});

test("a click outside closes the list without a change", async () => {
  render(<Form />);
  await userEvent.click(screen.getByRole("combobox", { name: "Fruit" }));
  await userEvent.click(screen.getByRole("button", { name: "Elsewhere" }));
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  expect(value()).toBe("banana");
});
