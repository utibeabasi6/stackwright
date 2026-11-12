import { defineConfig } from "@playwright/test";

// The checks mock the API in the browser, so no server needs to run.
export default defineConfig({
  testDir: "e2e",
  fullyParallel: true,
  reporter: "list",
  use: { baseURL: "http://127.0.0.1:5199" },
  projects: [
    { name: "light", use: { browserName: "chromium", colorScheme: "light", viewport: { width: 1440, height: 900 } } },
    { name: "dark", use: { browserName: "chromium", colorScheme: "dark", viewport: { width: 1440, height: 900 } } },
  ],
  webServer: {
    command: "npx vite --port 5199 --strictPort --host 127.0.0.1",
    url: "http://127.0.0.1:5199",
    reuseExistingServer: false,
  },
});
