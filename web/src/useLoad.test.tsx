import { renderHook, waitFor } from "@testing-library/react";
import { useLoad } from "./useLoad";

test("a refresh keeps the data on screen while it loads, and a new key does not", async () => {
  let resolve: (v: string) => void = () => {};
  let calls = 0;
  const load = () => {
    calls++;
    return calls === 1 ? Promise.resolve("first") : new Promise<string>((r) => (resolve = r));
  };
  const { result, rerender } = renderHook(({ k, refresh }) => useLoad(k, load, refresh), { initialProps: { k: "a", refresh: 0 } });
  await waitFor(() => expect(result.current.data).toBe("first"));

  rerender({ k: "a", refresh: 1 });
  expect(result.current.data).toBe("first");
  resolve("second");
  await waitFor(() => expect(result.current.data).toBe("second"));

  rerender({ k: "b", refresh: 1 });
  expect(result.current.data).toBeUndefined();
});
