import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { stateFile } from "./setup";

const state = () => JSON.parse(readFileSync(stateFile, "utf8")) as { hub: string; token: string };

// Scenario 9: scenario 3 (autoscale burst) driven entirely through the UI:
// create a pool agent, submit 50 tasks, watch it scale 0 → 5, finish, and go
// back to 0.
test("scenario 9: autoscale burst through the UI", async ({ page }) => {
  await page.goto(`/#token=${encodeURIComponent(state().token)}`);
  await page.goto("/#/new");
  await page.getByLabel("Name", { exact: true }).fill("burst");
  await page.getByLabel("Max instances").fill("5");
  await page.getByLabel("Idle timeout").fill("2s");
  await page.getByLabel("Model provider").selectOption("fake");
  await page.getByLabel("Scripted reply").fill("done");
  await page.getByLabel("Reply delay").fill("200");
  await page.getByRole("button", { name: "Create agent" }).click();
  await expect(page.getByRole("heading", { name: "burst" })).toBeVisible();
  await expect(page.locator('[data-field="status"]').first()).toContainText("asleep");

  const started = Date.now();
  for (let i = 0; i < 50; i++) {
    await page.getByRole("button", { name: "Run a task" }).click();
    await page.getByLabel("Task input").fill(`job ${i}`);
    await page.getByRole("button", { name: "Submit task" }).click();
    // Submitted when the task panel opens.
    await expect(page.getByRole("dialog", { name: "Task" })).toBeVisible();
    await page.keyboard.press("Escape");
  }
  const submitted = Date.now() - started;

  await page.getByRole("link", { name: "Deployments" }).click();
  const row = page.locator('tr[data-deployment="burst"]');
  await expect(row.locator('[data-field="ready"]')).toHaveText("5", { timeout: 60_000 });
  const scaled = Date.now() - started;

  // All 50 complete (counted through the same API the UI uses).
  await expect
    .poll(
      async () => {
        const r = await fetch(`${state().hub}/agen.v1.HubService/ListTasks`, {
          method: "POST",
          headers: { "Content-Type": "application/json", Authorization: `Bearer ${state().token}` },
          body: JSON.stringify({ deployment: "burst", limit: 100 }),
        });
        const data = await r.json();
        return (data.tasks ?? []).filter((t: { state: string }) => t.state === "TASK_STATE_SUCCEEDED").length;
      },
      { timeout: 120_000 },
    )
    .toBe(50);
  const done = Date.now() - started;

  await expect(row.locator('[data-field="ready"]')).toHaveText("0", { timeout: 60_000 });
  await expect(row.locator('[data-field="desired"]')).toHaveText("0");
  await expect(row.locator('[data-field="status"]')).toContainText("asleep");
  console.log(
    `scenario 9: 50 tasks submitted via UI in ${submitted} ms; 5 ready at ${scaled} ms; all done at ${done} ms; back to 0 at ${Date.now() - started} ms`,
  );
});
