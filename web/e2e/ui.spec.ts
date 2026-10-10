import AxeBuilder from "@axe-core/playwright";
import { expect, Page, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { stateFile } from "./setup";

const state = () => JSON.parse(readFileSync(stateFile, "utf8")) as { hub: string; token: string };

async function api<T = Record<string, unknown>>(method: string, body: object, token = state().token): Promise<T> {
  const r = await fetch(`${state().hub}/agen.v1.HubService/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${token}` },
    body: JSON.stringify(body),
  });
  const data = await r.json();
  if (!r.ok) throw new Error(`${method}: ${JSON.stringify(data)}`);
  return data as T;
}

const b64 = (s: string) => Buffer.from(s).toString("base64");

function bundle(name: string, config: object, script: object): Record<string, string> {
  return {
    "plugin.json": b64(JSON.stringify({ name })),
    "x-agen/agent.md": b64(`---\nname: ${name}\ndescription: ${name}\n---\nDo it.\n`),
    "x-agen/harness.json": b64(JSON.stringify({ provider: "fake", model: "fake-1", script: "x-agen/fake-script.json" })),
    "x-agen/config.json": b64(JSON.stringify(config)),
    "x-agen/fake-script.json": b64(JSON.stringify(script)),
  };
}

async function signIn(page: Page, token = state().token) {
  await page.goto(`/#token=${encodeURIComponent(token)}`);
  await expect(page.getByRole("heading", { name: "Overview" })).toBeVisible();
  expect(page.url()).not.toContain("token=");
}

/** Submits a task from a deployment page and returns to it with the task panel closed. */
async function runTask(page: Page, input: string) {
  await page.getByRole("button", { name: "Run a task" }).click();
  await page.getByLabel("Task input").fill(input);
  await page.getByRole("button", { name: "Submit task" }).click();
  await expect(page.getByRole("dialog", { name: "Task" })).toBeVisible();
  await page.keyboard.press("Escape");
}

async function accessible(page: Page) {
  const r = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa"]).analyze();
  const serious = r.violations.filter((v) => v.impact === "serious" || v.impact === "critical");
  expect(serious.map((v) => `${v.id}: ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`)).toEqual([]);
}

test("sign-in: a wrong token is refused, the sign-in link works", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("API token").fill("agen_wrong");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("alert")).toContainText(/token/i);
  await signIn(page);
});

test("create an agent from the form, run a task, follow it into its trace and transcript", async ({ page }) => {
  await signIn(page);
  await page.getByRole("link", { name: "Deployments" }).click();
  await page.getByRole("link", { name: "New agent" }).click();
  await page.getByLabel("Name", { exact: true }).fill("greeter");
  await page.getByLabel("Description").fill("Says hello");
  await expect(page.getByLabel("Model provider")).toHaveValue("openrouter");
  await page.getByLabel("Model provider").selectOption("fake");
  await page.getByLabel("Scripted reply").fill("Hello from the UI agent.");
  await page.getByRole("button", { name: "Create agent" }).click();
  await expect(page.getByRole("heading", { name: "greeter" })).toBeVisible();

  await runTask(page, "say hi");
  await page.getByRole("tab", { name: "Tasks" }).click();
  const row = page.locator("tr[data-task]").first();
  await expect(row.locator('[data-field="state"]')).toHaveText("succeeded", { timeout: 60_000 });
  await expect(row).toContainText("Hello from the UI agent.");

  await row.click();
  await page.getByRole("link", { name: "Open trace" }).click();
  await expect(page.locator('[data-field="summary"]')).toContainText("1 run");
  await expect(page.locator('[data-span="agen.run"]')).toContainText("greeter");
  await page.locator('[data-span="gen_ai.chat"]').click();
  const inspector = page.getByRole("complementary", { name: "Span details" });
  await expect(inspector).toContainText("Model call");
  await expect(inspector).toContainText("Hello from the UI agent.");
  await page.getByRole("tab", { name: "Transcript" }).click();
  await expect(page.locator('.msg[data-role="user"]')).toContainText("say hi");
  await expect(page.locator('.msg[data-role="assistant"]')).toContainText("Hello from the UI agent.");
});

test("scale, stop and start from the list; the fleet shows the instances", async ({ page }) => {
  await api("CreateDeployment", { name: "scaler", bundleFiles: bundle("scaler", { kind: "pool", scale: { min: 0, max: 2 } }, { cycle: true, responses: [{ text: "ok" }] }) });
  await signIn(page);
  await page.getByRole("link", { name: "Deployments" }).click();
  const row = page.locator('tr[data-deployment="scaler"]');
  await expect(row.locator('[data-field="desired"]')).toHaveText("0");
  await page.getByRole("button", { name: "scale scaler up" }).click();
  await page.getByRole("button", { name: "scale scaler up" }).click();
  await expect(row.locator('[data-field="desired"]')).toHaveText("2");
  await expect(page.getByRole("button", { name: "scale scaler up" })).toBeDisabled(); // max 2
  await expect(row.locator('[data-field="ready"]')).toHaveText("2", { timeout: 60_000 });

  await page.getByRole("link", { name: "Fleet" }).click();
  await expect(page.getByRole("heading", { name: "local" })).toBeVisible();
  await expect(page.locator('tr[data-instance="scaler"]')).toHaveCount(2);

  await page.getByRole("link", { name: "Deployments" }).click();
  await row.getByRole("button", { name: "Stop" }).click();
  await expect(row.locator('[data-field="status"]')).toContainText("stopped");
  await expect(row.locator('[data-field="desired"]')).toHaveText("0");
  await expect(page.getByRole("button", { name: "scale scaler up" })).toBeDisabled();
  await row.getByRole("button", { name: "Start" }).click();
  await expect(row.locator('[data-field="status"]')).not.toContainText("stopped");
});

test("edit a bundle and roll out the new version", async ({ page }) => {
  await api("CreateDeployment", { name: "editme", bundleFiles: bundle("editme", { kind: "pool", scale: { min: 0, max: 1 } }, { cycle: true, responses: [{ text: "version one" }] }) });
  await signIn(page);
  await page.goto("/#/edit/default/editme");
  await page.getByRole("button", { name: "x-agen/fake-script.json" }).click();
  const editor = page.getByLabel("content of x-agen/fake-script.json");
  await expect(editor).toContainText("version one");
  await editor.fill(JSON.stringify({ cycle: true, responses: [{ text: "version two" }] }));
  await page.getByRole("button", { name: "Save new version" }).click();
  await expect(page.locator(".note")).toContainText("rolling out");
  // A broken bundle is refused with the validator's message.
  await page.getByRole("button", { name: "x-agen/harness.json" }).click();
  await page.getByLabel("content of x-agen/harness.json").fill('{"provider":"nope"}');
  await page.getByRole("button", { name: "Save new version" }).click();
  await expect(page.locator(".error")).toContainText("harness.json");

  await page.goto("/#/deployments/default/editme/tasks");
  await runTask(page, "which version?");
  await expect(page.locator("tr[data-task]").first()).toContainText("version two", { timeout: 60_000 });
});

test("approve an agent's pending ask from the inbox", async ({ page }) => {
  await api("CreateDeployment", { name: "drafter", bundleFiles: bundle("drafter", { kind: "pool", scale: { min: 0, max: 1 } }, { cycle: true, responses: [{ text: "Draft ready." }] }) });
  await api("CreateDeployment", {
    name: "gated",
    bundleFiles: bundle(
      "gated",
      {
        kind: "pool",
        scale: { min: 0, max: 1 },
        delegates: [{ name: "drafter" }],
        permissions: { default: "deny", rules: [{ tool: "call_agent", action: "ask" }] },
      },
      { perRun: true, responses: [{ toolCalls: [{ name: "call_agent", arguments: { agent: "drafter", message: "publish" } }] }, { text: "published" }] },
    ),
  });
  // Submitted by a separate operator, so the admin (signed in) may approve.
  const tok = await api<{ secret: string }>("CreateApiToken", { name: "ops", scopes: ["operator"] });
  const task = (await api<{ task: { id: string } }>("SubmitTask", { ref: { name: "gated" }, input: "go" }, tok.secret)).task;

  await signIn(page);
  await expect(page.locator(".list li", { hasText: "waits for a decision" })).toBeVisible({ timeout: 60_000 }); // on the overview
  await page.getByRole("link", { name: /Inbox/ }).click();
  const item = page.locator("[data-approval]").first();
  await expect(item).toContainText("call_agent", { timeout: 60_000 });
  await expect(item).toContainText('"agent": "drafter"');
  await item.getByRole("button", { name: "Show what the agent was doing" }).click();
  await expect(item).toContainText("go");
  await item.getByRole("button", { name: "Approve" }).click();
  await expect(page.getByText("No approvals are waiting.")).toBeVisible();

  await expect
    .poll(async () => (await api<{ task: { state: string } }>("GetTask", { id: task.id })).task.state, { timeout: 60_000 })
    .toBe("TASK_STATE_SUCCEEDED");
  const done = await api<{ task: { output: string } }>("GetTask", { id: task.id });
  expect(done.task.output).toBe("published");
});

test("upload a bundle folder (with a large binary file)", async ({ page }) => {
  const { mkdtempSync, mkdirSync, writeFileSync, cpSync } = await import("node:fs");
  const { tmpdir } = await import("node:os");
  const { join } = await import("node:path");
  const dir = join(mkdtempSync(join(tmpdir(), "agen-upload-")), "uploaded");
  cpSync(join(process.cwd(), "..", "examples", "bundles", "hello"), dir, { recursive: true });
  mkdirSync(join(dir, "assets"), { recursive: true });
  const big = Buffer.alloc(200 * 1024);
  for (let i = 0; i < big.length; i++) big[i] = i % 251;
  writeFileSync(join(dir, "assets", "blob.bin"), big);
  writeFileSync(join(dir, "plugin.json"), JSON.stringify({ name: "uploaded", version: "0.1.0" }));
  writeFileSync(join(dir, "x-agen", "agent.md"), "---\nname: uploaded\ndescription: Uploaded bundle\nskills: [greeting]\n---\nBe brief.\n");

  await signIn(page);
  await page.goto("/#/new");
  await page.getByLabel("Bundle folder").setInputFiles(dir);
  await expect(page.getByRole("heading", { name: "uploaded" })).toBeVisible();
  // The binary arrived intact.
  const d = await api<{ deployment: { definitionDigest: string } }>("GetDeployment", { ref: { name: "uploaded" } });
  const def = await api<{ definition: { files: Record<string, string> } }>("GetDefinition", { digest: d.deployment.definitionDigest });
  expect(Buffer.from(def.definition.files["assets/blob.bin"], "base64").equals(big)).toBe(true);
});

test("cross-agent trace with inspector, deny and delete", async ({ page }) => {
  await api("CreateDeployment", { name: "helper", bundleFiles: bundle("helper", { kind: "pool", scale: { min: 0, max: 1 } }, { cycle: true, responses: [{ text: "helped" }] }) });
  await api("CreateDeployment", {
    name: "lead",
    bundleFiles: bundle(
      "lead",
      { kind: "pool", scale: { min: 0, max: 1 }, delegates: [{ name: "helper" }], permissions: { default: "deny", rules: [{ tool: "call_agent", action: "allow" }] } },
      { perRun: true, responses: [{ toolCalls: [{ name: "call_agent", arguments: { agent: "helper", message: "help" } }] }, { text: "led" }] },
    ),
  });
  await signIn(page);
  await page.goto("/#/deployments/default/lead/tasks");
  await runTask(page, "go");
  await expect(page.locator("tr[data-task]").first()).toContainText("led", { timeout: 60_000 });
  await page.locator("tr[data-task]").first().click();
  await page.getByRole("link", { name: "Open trace" }).click();
  await expect(page.locator('[data-field="summary"]')).toContainText("2 runs");
  const helper = page.locator('[data-span="agen.run"]', { hasText: "helper" });
  await expect(helper).toHaveAttribute("aria-level", "3"); // run > tool > run
  await expect(page.locator('[data-span="agen.run"]', { hasText: "lead" })).toHaveAttribute("aria-level", "1");

  // The delegated call shows its arguments and result.
  await page.locator('[data-span="agen.tool"]').click();
  const inspector = page.getByRole("complementary", { name: "Span details" });
  await expect(inspector).toContainText('"agent": "helper"');
  await expect(inspector).toContainText("helped");
  // Keyboard: down moves to the helper run.
  await page.getByRole("tree", { name: "Spans" }).press("ArrowDown");
  await expect(inspector).toContainText("Agent run");
  // Runs list: one row per trace, with both agents' runs counted.
  await page.goto("/#/runs?deployment=lead");
  await expect(page.locator("tr[data-run]").first()).toContainText("2");

  // Deny an ask.
  await api("CreateDeployment", {
    name: "asker",
    bundleFiles: bundle(
      "asker",
      { kind: "pool", scale: { min: 0, max: 1 }, delegates: [{ name: "helper" }], permissions: { default: "deny", rules: [{ tool: "call_agent", action: "ask" }] } },
      { perRun: true, responses: [{ toolCalls: [{ name: "call_agent", arguments: { agent: "helper", message: "x" } }] }, { text: "asked" }] },
    ),
  });
  const tok = await api<{ secret: string }>("CreateApiToken", { name: "ops2", scopes: ["operator"] });
  await api("SubmitTask", { ref: { name: "asker" }, input: "go" }, tok.secret);
  await page.goto("/#/inbox");
  const item = page.locator("[data-approval]", { hasText: "asker" });
  await expect(item).toBeVisible({ timeout: 60_000 });
  await item.getByRole("button", { name: "Deny" }).click();
  await page.getByRole("tab", { name: "Decided approvals" }).click();
  await expect(page.locator("tr[data-approval]", { hasText: "asker" }).locator('[data-field="state"]')).toHaveText("denied");

  // Delete (two-step).
  await page.goto("/#/deployments/default/asker");
  await page.getByRole("button", { name: "Delete…" }).click();
  await page.getByRole("button", { name: "Delete deployment" }).click();
  await expect(page.getByRole("heading", { name: "Deployments" })).toBeVisible();
  await expect(page.locator('tr[data-deployment="asker"]')).toHaveCount(0);
});

test("deploy from a template; costs and notification targets", async ({ page }) => {
  await signIn(page);
  await page.getByRole("link", { name: "Templates" }).click();
  await page.getByRole("link", { name: "Use the Hello template" }).click();
  await page.getByLabel("Name", { exact: true }).fill("hello-tpl");
  await page.getByRole("button", { name: "Deploy" }).click();
  await expect(page.getByRole("heading", { name: "hello-tpl" })).toBeVisible();
  await page.getByRole("tab", { name: "Bundle" }).click();
  await expect(page.getByLabel("content of x-agen/agent.md")).toContainText("friendly assistant");

  await page.getByRole("link", { name: "Costs" }).click();
  await expect(page.getByRole("heading", { name: "Costs" })).toBeVisible();

  await page.goto("/#/settings/notifications");
  await page.getByRole("button", { name: "Add target" }).click();
  await page.getByLabel("Name", { exact: true }).fill("ops-chat");
  await page.getByLabel("URL").fill("https://example.invalid/hook");
  await page.getByRole("dialog").getByRole("button", { name: "Add target" }).click();
  await expect(page.getByRole("status")).toContainText("Signing secret");
  await expect(page.locator("td", { hasText: "ops-chat" })).toBeVisible();
});

test("command palette jumps to a deployment", async ({ page }) => {
  await signIn(page);
  await page.keyboard.press("Control+k");
  await page.getByRole("combobox", { name: "Search" }).fill("scaler");
  await page.keyboard.press("Enter");
  await expect(page.getByRole("heading", { name: "scaler" })).toBeVisible();
});

test("pages are accessible and fit a phone screen", async ({ page }) => {
  await signIn(page);
  for (const path of ["/#/", "/#/deployments", "/#/runs", "/#/inbox", "/#/costs", "/#/templates", "/#/fleet", "/#/settings"]) {
    await page.goto(path);
    await expect(page.locator("main h1")).toBeVisible();
    await expect(page.locator('[aria-busy="true"]')).toHaveCount(0);
    await accessible(page);
  }
  await page.setViewportSize({ width: 390, height: 844 });
  for (const path of ["/#/", "/#/deployments", "/#/runs", "/#/inbox"]) {
    await page.goto(path);
    await expect(page.locator("main h1")).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, `${path} scrolls sideways`).toBeLessThanOrEqual(0);
  }
  await page.getByRole("button", { name: "Menu" }).click();
  await page.getByRole("link", { name: "Runs" }).click();
  await expect(page.getByRole("heading", { name: "Runs" })).toBeVisible();
});

test("a namespace-scoped token sees only its namespace; a revoked token signs out", async ({ page }) => {
  await api("CreateDeployment", { namespace: "team", name: "teamapp", bundleFiles: bundle("teamapp", { kind: "pool", scale: { min: 0, max: 1 } }, { cycle: true, responses: [{ text: "t" }] }) });
  const tok = await api<{ secret: string; token: { id: string } }>("CreateApiToken", { name: "team-viewer", scopes: ["viewer"], namespaces: ["team"] });
  await signIn(page, tok.secret);
  await page.getByRole("link", { name: "Deployments" }).click();
  await expect(page.locator('tr[data-deployment="teamapp"]')).toBeVisible();
  await expect(page.locator("tr[data-deployment]")).toHaveCount(1); // nothing from "default"
  // A viewer gets no buttons that would fail.
  await expect(page.getByRole("button", { name: "scale teamapp up" })).toHaveCount(0);
  await expect(page.getByRole("link", { name: "New agent" })).toHaveCount(0);
  await page.getByRole("link", { name: /Inbox/ }).click();
  await expect(page.getByText(/approver scope/)).toBeVisible();
  await page.getByRole("link", { name: "Fleet" }).click();
  await expect(page.getByRole("heading", { name: "Fleet" })).toBeVisible();

  await api("RevokeApiToken", { id: tok.token.id });
  await page.getByRole("link", { name: "Deployments" }).click();
  await expect(page.getByLabel("API token")).toBeVisible(); // back to sign-in
});
