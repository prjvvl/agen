import { readFileSync, rmSync } from "node:fs";
import { stateFile } from "./setup";

export default async function teardown() {
  const s = JSON.parse(readFileSync(stateFile, "utf8"));
  try {
    await fetch(`${s.hub}/local/shutdown`, { method: "POST", headers: { Authorization: `Bearer ${s.token}` } });
  } catch {
    /* already down */
  }
  const deadline = Date.now() + 30_000;
  while (Date.now() < deadline) {
    try {
      await fetch(`${s.hub}/healthz`);
      await new Promise((r) => setTimeout(r, 200));
    } catch {
      break;
    }
  }
  try {
    process.kill(s.pid);
  } catch {
    /* exited */
  }
  rmSync(stateFile, { force: true });
}
