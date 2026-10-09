import { execFileSync, spawn } from "node:child_process";
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
export const repo = join(here, "..", "..");
export const stateFile = join(here, ".state.json");
const exe = process.platform === "win32" ? ".exe" : "";

export default async function setup() {
  const hub = process.env.AGEN_E2E_HUB ?? "http://127.0.0.1:17391";
  // UI into the binary, then the binary itself.
  execFileSync("npm", ["run", "build"], { cwd: join(repo, "web"), stdio: "inherit", shell: true });
 writeFileSync(join(repo, "platform", "internal", "ui", "dist", ".keep"), ""); // vite empties the directory
  const agen = join(here, ".bin", "agen" + exe);
  execFileSync("go", ["build", "-o", agen, "./cmd/agen"], { cwd: join(repo, "platform"), stdio: "inherit" });
  const host = process.env.AGEN_HOST_BIN ?? join(repo, "target", "debug", "agen-host" + exe);
  if (!existsSync(host)) execFileSync("cargo", ["build", "-q", "-p", "agen-host"], { cwd: repo, stdio: "inherit" });

  const home = mkdtempSync(join(tmpdir(), "agen-e2e-"));
  const env = { ...process.env, AGEN_HOME: home, AGEN_HUB: "", AGEN_TOKEN: "" };
  const listen = hub.replace(/^https?:\/\//, "");
  const up = spawn(agen, ["up", "--listen", listen, "--gateway-listen", "127.0.0.1:0", "--host-bin", host], { env, stdio: ["ignore", "pipe", "pipe"] });
  let out = "";
  up.stdout.on("data", (d) => (out += d));
  up.stderr.on("data", () => {});
  const deadline = Date.now() + 60_000;
  while (!out.includes("agen is up")) {
    if (Date.now() > deadline || up.exitCode !== null) throw new Error("agen up did not start:\n" + out);
    await new Promise((r) => setTimeout(r, 200));
  }
  const local = JSON.parse(readFileSync(join(home, "local.json"), "utf8"));
  writeFileSync(stateFile, JSON.stringify({ hub, token: local.token, home, pid: up.pid, agen }));
  up.unref();
}
