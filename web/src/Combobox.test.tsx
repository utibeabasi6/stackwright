import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Combobox } from "./Combobox";

const repos = ["acme/infra", "acme/network", "acme/web", "globex/infra"].map((r) => ({
  value: `https://github.com/${r}`,
  label: r,
}));

let submitted: FormDataEntryValue | null | undefined;

function Form() {
  return (
    <form
      aria-label="form"
      onSubmit={(e) => {
        e.preventDefault();
        submitted = new FormData(e.currentTarget).get("repo");
      }}
    >
      <label>
        Repository <Combobox name="repo" options={repos} />
      </label>
      <button type="button">Elsewhere</button>
    </form>
  );
}

const names = () => screen.queryAllByRole("option").map((o) => o.textContent);

beforeEach(() => {
  submitted = undefined;
});
afterEach(cleanup);

test("typing filters the list and a click fills the box", async () => {
  render(<Form />);
  const box = screen.getByRole("combobox", { name: "Repository" });
  await userEvent.type(box, "infra");
  expect(names()).toEqual(["acme/infra", "globex/infra"]);
  await userEvent.click(screen.getByRole("option", { name: "globex/infra" }));
  expect(box).toHaveValue("https://github.com/globex/infra");
  expect(box).toHaveFocus();
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
});

test("arrows and Enter choose an option without submitting the form", async () => {
  render(<Form />);
  const box = screen.getByRole("combobox", { name: "Repository" });
  await userEvent.type(box, "acme");
  await userEvent.keyboard("{ArrowDown}{ArrowDown}");
  expect(box).toHaveAttribute("aria-activedescendant", screen.getByRole("option", { name: "acme/network" }).id);
  await userEvent.keyboard("{Enter}");
  expect(box).toHaveValue("https://github.com/acme/network");
  expect(submitted).toBeUndefined();
  await userEvent.keyboard("{Enter}");
  expect(submitted).toBe("https://github.com/acme/network");
});

test("any text is kept, and Escape and leaving close the list", async () => {
  render(<Form />);
  const box = screen.getByRole("combobox", { name: "Repository" });
  await userEvent.type(box, "https://gitlab.com/acme/infra");
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  await userEvent.clear(box);
  await userEvent.type(box, "a");
  expect(box).toHaveAttribute("aria-expanded", "true");
  await userEvent.keyboard("{Escape}");
  expect(box).toHaveAttribute("aria-expanded", "false");
  await userEvent.keyboard("{ArrowDown}");
  expect(box).toHaveAttribute("aria-expanded", "true");
  await userEvent.click(screen.getByRole("button", { name: "Elsewhere" }));
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  expect(box).toHaveValue("a");
});
