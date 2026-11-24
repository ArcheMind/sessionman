// Live smokes run with a PATH holding only sm and the native agent CLIs, so a regression that
// needs a third-party ACP wrapper (claude-agent-acp, codex-acp) fails here instead of on a
// user's machine that only installed sm plus the native CLIs.
import { execFileSync } from "node:child_process";
import { mkdtempSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

export function nativeOnlyEnv(sm, env = process.env, tools = ["claude", "codex", "pi", "node"]) {
  const dir = mkdtempSync(join(tmpdir(), "sm-native-path-"));
  symlinkSync(sm, join(dir, "sm"));
  for (const tool of tools) {
    let real;
    try {
      real = execFileSync("sh", ["-c", `command -v ${tool}`], { env, encoding: "utf8" }).trim();
    } catch {
      continue;
    }
    symlinkSync(real, join(dir, tool));
  }
  const stripped = { ...env, PATH: `${dir}:/usr/bin:/bin:/usr/sbin:/sbin` };
  for (const wrapper of ["claude-agent-acp", "codex-acp"]) {
    try {
      execFileSync("sh", ["-c", `command -v ${wrapper}`], { env: stripped, stdio: "ignore" });
    } catch {
      continue;
    }
    throw new Error(`${wrapper} is still reachable on the stripped PATH`);
  }
  return stripped;
}
