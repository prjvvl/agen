import { defineConfig } from "@playwright/test";

// E2E against a real `agen up` (Hub + Nest + agen-host processes) started by
// e2e/setup.ts on this port, with the UI built into the binary.
export const HUB = process.env.AGEN_E2E_HUB ?? "http://127.0.0.1:17391";

export default defineConfig({
  testDir: "e2e",
  timeout: 180_000,
  expect: { timeout: 30_000 },
  workers: 1,
  reporter: [["list"]],
  globalSetup: "./e2e/setup.ts",
  globalTeardown: "./e2e/teardown.ts",
  use: {
    baseURL: HUB,
    // The installed Chrome (no browser download); PW_CHANNEL=chromium in CI.
    channel: process.env.PW_CHANNEL ?? "chrome",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
});
