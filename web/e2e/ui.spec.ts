import { expect, Page, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { stateFile } from "./setup";

const state = () => JSON.parse(readFileSync(stateFile, "utf8")) as { hub: string; token: string };

async function api<T = Record<string, unknown>>(method: string, body: object): Promise<T> {
  const s = state();
  const r = await fetch(`${s.hub}/agen.v1.HubService/${method}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${s.token}` },
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

async function signIn(page: Page) {
  await page.goto(`/#token=${encodeURIComponent(state().token)}`);
  await expect(page.getByRole("heading", { name: "Deployments" })).toBeVisible();
  expect(page.url()).not.toContain("token=");
}

test("sign-in: a wrong token is refused, the sign-in link works", async ({ page }) => {
  await page.goto("/");
  await page.getByLabel("API token").fill("agen_wrong");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.locator(".error")).toContainText(/token/i);
  await signIn(page);
});

test("create an agent from the form, run a task, open its trace", async ({ page }) => {
  await signIn(page);
  await page.getByRole("link", { name: "New agent" }).click();
  await page.getByLabel("Name", { exact: true }).fill("greeter");
  await page.getByLabel("Description").fill("Says hello");
  await page.getByLabel("Scripted reply").fill("Hello from the UI agent.");
  await page.getByRole("button", { name: "Create agent" }).click();
  await expect(page.getByRole("heading", { name: /default\/greeter/ })).toBeVisible();

  await page.getByLabel("Task input").fill("say hi");
  await page.getByRole("button", { name: "Submit task" }).click();
  await expect(page.locator(".note")).toContainText("queued");
  const row = page.locator("tr[data-task]").first();
  await expect(row.locator('[data-field="state"]')).toHaveText("succeeded", { timeout: 60_000 });
  await expect(row).toContainText("Hello from the UI agent.");

  await page.getByRole("link", { name: "trace" }).first().click();
  await expect(page.getByRole("heading", { name: /Trace/ })).toBeVisible();
  await expect(page.locator('[data-field="summary"]')).toContainText("1 runs");
  await expect(page.locator('tr[data-span="agen.run"]')).toContainText("default/greeter");
  await expect(page.locator('tr[data-span="gen_ai.chat"]')).toHaveCount(1);
});

test("scale, stop and start from the list; the fleet shows the instances", async ({ page }) => {
  await api("CreateDeployment", { name: "scaler", bundleFiles: bundle("scaler", { kind: "pool", scale: { min: 0, max: 2 } }, { cycle: true, responses: [{ text: "ok" }] }) });
  await signIn(page);
  const row = page.locator('tr[data-deployment="scaler"]');
  await expect(row.locator('[data-field="desired"]')).toHaveText("0");
  await page.getByRole("button", { name: "scale scaler up" }).click();
  await page.getByRole("button", { name: "scale scaler up" }).click();
  await expect(row.locator('[data-field="desired"]')).toHaveText("2");
  await expect(page.getByRole("button", { name: "scale scaler up" })).toBeDisabled(); // max 2
  await expect(row.locator('[data-field="ready"]')).toHaveText("2", { timeout: 60_000 });

  await page.getByRole("link", { name: "Fleet" }).click();
  await expect(page.locator("td", { hasText: "local" }).first()).toBeVisible();
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

  await page.goto("/#/deployments/default/editme");
  await page.getByLabel("Task input").fill("which version?");
  await page.getByRole("button", { name: "Submit task" }).click();
  await expect(page.locator("tr[data-task]").first()).toContainText("version two", { timeout: 60_000 });
});

test("approve an agent's pending ask from the approvals page", async ({ page }) => {
  await api("CreateDeployment", { name: "writer", bundleFiles: bundle("writer", { kind: "pool", scale: { min: 0, max: 1 } }, { cycle: true, responses: [{ text: "Draft ready." }] }) });
  await api("CreateDeployment", {
    name: "gated",
    bundleFiles: bundle(
      "gated",
      {
        kind: "pool",
        scale: { min: 0, max: 1 },
        delegates: [{ name: "writer" }],
        permissions: { default: "deny", rules: [{ tool: "call_agent", action: "ask" }] },
      },
      { perRun: true, responses: [{ toolCalls: [{ name: "call_agent", arguments: { agent: "writer", message: "publish" } }] }, { text: "published" }] },
    ),
  });
  // Submitted by a separate operator, so the admin (signed in) may approve.
  const tok = await api<{ secret: string }>("CreateApiToken", { name: "ops", scopes: ["operator"] });
  const r = await fetch(`${state().hub}/agen.v1.HubService/SubmitTask`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${tok.secret}` },
    body: JSON.stringify({ ref: { name: "gated" }, input: "go" }),
  });
  const task = (await r.json()).task;

  await signIn(page);
  await page.getByRole("link", { name: "Approvals" }).click();
  const row = page.locator("tr[data-approval]").first();
  await expect(row).toContainText("call_agent", { timeout: 60_000 });
  await expect(row).toContainText('"agent":"writer"');
  await row.getByRole("button", { name: "Approve" }).click();
  await expect(page.locator(".empty")).toContainText("No pending approvals");

  await expect
    .poll(async () => (await api<{ task: { state: string; output?: string } }>("GetTask", { id: task.id })).task.state, { timeout: 60_000 })
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
  await page.getByRole("link", { name: "New agent" }).click();
  await page.getByLabel("Bundle folder").setInputFiles(dir);
  await expect(page.getByRole("heading", { name: /default\/uploaded/ })).toBeVisible();
  // The binary arrived intact.
  const d = await api<{ deployment: { definitionDigest: string } }>("GetDeployment", { ref: { name: "uploaded" } });
  const def = await api<{ definition: { files: Record<string, string> } }>("GetDefinition", { digest: d.deployment.definitionDigest });
  expect(Buffer.from(def.definition.files["assets/blob.bin"], "base64").equals(big)).toBe(true);
});

test("cross-agent trace, deny and delete", async ({ page }) => {
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
  await page.goto("/#/deployments/default/lead");
  await page.getByLabel("Task input").fill("go");
  await page.getByRole("button", { name: "Submit task" }).click();
  await expect(page.locator("tr[data-task]").first()).toContainText("led", { timeout: 60_000 });
  await page.getByRole("link", { name: "trace" }).first().click();
  await expect(page.locator('[data-field="summary"]')).toContainText("2 runs");
  const lead = page.locator('tr[data-span="agen.run"]', { hasText: "default/lead" });
  const helper = page.locator('tr[data-span="agen.run"]', { hasText: "default/helper" });
  await expect(helper).toHaveAttribute("data-depth", "2"); // run > tool > run
  await expect(lead).toHaveAttribute("data-depth", "0");

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
  await fetch(`${state().hub}/agen.v1.HubService/SubmitTask`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${tok.secret}` },
    body: JSON.stringify({ ref: { name: "asker" }, input: "go" }),
  });
  await page.getByRole("link", { name: "Approvals" }).click();
  const row = page.locator("tr[data-approval]").first();
  await expect(row).toContainText("asker", { timeout: 60_000 });
  await row.getByRole("button", { name: "Deny" }).click();
  await page.getByLabel("Show decided").check();
  await expect(page.locator("tr[data-approval]", { hasText: "asker" }).locator('[data-field="state"]')).toHaveText("denied");

  // Delete (two-step).
  await page.goto("/#/deployments/default/asker");
  await page.getByRole("button", { name: "Delete…" }).click();
  await page.getByRole("button", { name: "Confirm delete" }).click();
  await expect(page.getByRole("heading", { name: "Deployments" })).toBeVisible();
  await expect(page.locator('tr[data-deployment="asker"]')).toHaveCount(0);
});

test("a namespace-scoped token sees only its namespace; a revoked token signs out", async ({ page }) => {
  await api("CreateDeployment", { namespace: "team", name: "teamapp", bundleFiles: bundle("teamapp", { kind: "pool", scale: { min: 0, max: 1 } }, { cycle: true, responses: [{ text: "t" }] }) });
  const tok = await api<{ secret: string; token: { id: string } }>("CreateApiToken", { name: "team-viewer", scopes: ["viewer"], namespaces: ["team"] });
  await page.goto(`/#token=${encodeURIComponent(tok.secret)}`);
  await expect(page.locator('tr[data-deployment="teamapp"]')).toBeVisible();
  await expect(page.locator("tr[data-deployment]")).toHaveCount(1); // nothing from "default"
  // A viewer cannot scale: the API error is shown, the page keeps working.
  await page.getByRole("button", { name: "scale teamapp up" }).click();
  await expect(page.locator(".error")).toContainText(/scope/i);
  await page.getByRole("link", { name: "Approvals" }).click();
  await expect(page.locator(".error")).toContainText(/scope/i); // approver scope needed
  await page.getByRole("link", { name: "Fleet" }).click();
  await expect(page.getByRole("heading", { name: "Fleet" })).toBeVisible();

  await api("RevokeApiToken", { id: tok.token.id });
  await page.getByRole("link", { name: "Deployments" }).click();
  await expect(page.getByLabel("API token")).toBeVisible(); // back to sign-in
});
