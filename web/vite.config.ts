import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    strictPort: true,
    proxy: Object.fromEntries(
      ["/stackwright.", "/auth/", "/healthz"].map((path) => [
        path,
        { target: "http://127.0.0.1:8080", headers: { origin: "http://127.0.0.1:8080" } },
      ]),
    ),
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
  },
});
