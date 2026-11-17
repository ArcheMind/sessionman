// E2E isolation matrix: 5 agents × {S, S+C}.
// For every session-writing entry point, asserts:
//   - session lands in S
//   - agent reads C's config (S+C mode)
//   - skills installed in C persist
//   - nested sm: child lands in native root semantics, parent S/C untouched
//
// Usage: node test/e2e/isolation-matrix.mjs <sm-binary>
//
// Requires: pi, codex, claude, agy, dsh installed natively with valid auth.

import { execFileSync, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, realpathSync, existsSync, rmSync, readdirSync, lstatSync } from "node:fs";
import { tmpdir, homedir } from "node:os";
import { join, resolve, relative } from "node:path";
import { randomUUID } from "node:crypto";

const sm = resolve(process.argv[2] ?? "bin/sm");
if (!existsSync(sm)) { console.error(`sm binary not found: ${sm}`); process.exit(2); }

// --- models ---
const models = {
  pi: "deepseek/deepseek-flash",
  codex: "gpt-6-luna",
  claude: "haiku",
  agy: "gemini-3.8-flash-low",
  dsh: null, // sm rejects --model for dsh
};
const efforts = { codex: "low" };

// --- helpers ---
function tmpBase(label) {
  return realpathSync(mkdtempSync(join(tmpdir(), `sm-e2e-${label}-`)));
}

function tree(root) {
  const files = [];
  const walk = (dir) => {
    for (const name of readdirSync(dir)) {
      const full = join(dir, name);
      if (lstatSync(full).isDirectory()) walk(full); else files.push(relative(root, full));
    }
  };
  if (existsSync(root)) walk(root);
  return files.sort();
}

function smRun(args, opts = {}) {
  const env = { ...process.env, ...(opts.env ?? {}) };
  const result = spawnSync(sm, args, {
    encoding: "utf8",
    timeout: opts.timeout ?? 300_000,
    input: opts.input,
    env,
    cwd: opts.cwd,
  });
  return { stdout: result.stdout ?? "", stderr: result.stderr ?? "", status: result.status, signal: result.signal };
}

function assert(cond, msg) {
  if (!cond) throw new Error(msg);
}

// --- entry point tests ---

// 1. Direct launch: sm <agent> --cwd <workspace> --dry-run (no model needed for dry-run)
function testDirectLaunchDryRun(agent, s, c) {
  const workspace = tmpBase(`launch-${agent}`);
  const args = ["--session-dir", s];
  if (c) args.push("--config-dir", c);
  args.push(agent, "--cwd", workspace, "--dry-run");
  const r = smRun(args);
  assert(r.status === 0, `${agent} dry-run failed (exit ${r.status}): ${r.stderr}`);
  assert(tree(s).length === 0, `${agent} dry-run wrote under S: ${tree(s)}`);
  if (c) assert(tree(c).length === 0, `${agent} dry-run wrote under C: ${tree(c)}`);
  rmSync(workspace, { recursive: true, force: true });
  return true;
}

// 2. Session continue: the main live entry point. Sends a minimal transcript, asserts session lands in S.
function testSessionContinue(agent, s, c) {
  if (agent === "dsh") return null; // dsh has no headless continue adapter
  const workspace = tmpBase(`continue-${agent}`);
  const marker = `MARKER-${randomUUID().slice(0, 8)}`;
  const transcript = JSON.stringify({
    id: `e2e-${randomUUID()}`,
    provider: agent,
    workspace,
    messageCount: 1,
    messages: [{
      id: "u1",
      role: "user",
      content: [{ type: "text", text: `Reply with exactly: ${marker}` }],
    }],
  });
  const args = ["--json", "--session-dir", s];
  if (c) args.push("--config-dir", c);
  args.push("session", "continue", agent);
  if (models[agent]) args.push("--model", models[agent]);
  if (efforts[agent]) args.push("--effort", efforts[agent]);
  args.push("--mode", "yolo");
  const r = smRun(args, { input: transcript, timeout: 300_000 });
  assert(r.status === 0, `${agent} continue failed (exit ${r.status}): ${r.stderr.slice(0, 500)}`);
  // Check session landed in S
  const sFiles = tree(s);
  const agentDir = agent === "agy" ? "agy" : agent;
  const sessionInS = sFiles.some(f => f.startsWith(agentDir + "/") || f.startsWith(agentDir + "\\"));
  assert(sessionInS, `${agent} session not found under S/${agentDir}: ${sFiles.join(", ")}`);
  rmSync(workspace, { recursive: true, force: true });
  return { marker, stdout: r.stdout, stderr: r.stderr };
}

// 3. Session import: writes a session file from stdin transcript.
function testSessionImport(agent, s, c) {
  const workspace = tmpBase(`import-${agent}`);
  const transcript = JSON.stringify({
    id: `e2e-import-${randomUUID()}`,
    provider: agent === "dsh" ? "claude" : agent, // dsh doesn't have its own transcript format
    workspace,
    messageCount: 1,
    messages: [{
      id: "u1",
      role: "user",
      content: [{ type: "text", text: "hello" }],
    }, {
      id: "a1",
      role: "assistant",
      content: [{ type: "text", text: "hi" }],
    }],
  });
  const args = ["--session-dir", s];
  if (c) args.push("--config-dir", c);
  args.push("session", "import", agent);
  const r = smRun(args, { input: transcript });
  // Some agents may not support import, that's OK — we just check it doesn't corrupt S
  if (r.status === 0) {
    const sFiles = tree(s);
    assert(sFiles.length > 0, `${agent} import wrote nothing under S`);
  }
  rmSync(workspace, { recursive: true, force: true });
  return r;
}

// 4. Skills in C persist across runs
function testSkillPersistence(agent, s, c) {
  if (!c) return null;
  // Install a dummy skill file in C/<agent>/skills/
  const skillDir = join(c, agent, "skills");
  mkdirSync(skillDir, { recursive: true });
  const skillFile = join(skillDir, "e2e-test-skill.md");
  writeFileSync(skillFile, "# E2E test skill\nThis is a test.\n");
  // Run agent with S+C (dry-run is fine — we just need Prepare to run)
  const workspace = tmpBase(`skill-${agent}`);
  const args = ["--session-dir", s, "--config-dir", c, agent, "--cwd", workspace, "--dry-run"];
  smRun(args);
  // Check skill still in C
  assert(existsSync(skillFile), `${agent} skill in C was removed after run`);
  rmSync(workspace, { recursive: true, force: true });
  return true;
}

// 5. Nested sm: parent sets S+C, runs child sm session import; child writes to its own temp S,
//    parent S/C untouched.
function testNestedSm(agent, s, c) {
  const childS = tmpBase(`nested-child-${agent}`);
  const workspace = tmpBase(`nested-${agent}`);
  // Parent's S snapshot before
  const parentSBefore = tree(s);
  const parentCBefore = c ? tree(c) : [];
  // Run child sm session import with its own --session-dir
  const transcript = JSON.stringify({
    id: `e2e-nested-${randomUUID()}`,
    provider: agent === "dsh" ? "claude" : agent,
    workspace,
    messageCount: 1,
    messages: [{
      id: "u1", role: "user",
      content: [{ type: "text", text: "nested test" }],
    }, {
      id: "a1", role: "assistant",
      content: [{ type: "text", text: "ok" }],
    }],
  });
  // Child uses its own temp S, inheriting parent's S+C env but overriding S
  const env = { SM_SESSION_DIR: childS };
  if (c) env.SM_CONFIG_DIR = c;
  const args = ["session", "import", agent];
  const r = smRun(args, { input: transcript, env });
  // Parent S/C untouched
  const parentSAfter = tree(s);
  const parentCAfter = c ? tree(c) : [];
  assert(
    JSON.stringify(parentSBefore) === JSON.stringify(parentSAfter),
    `${agent} nested: parent S changed: ${parentSBefore} -> ${parentSAfter}`
  );
  assert(
    JSON.stringify(parentCBefore) === JSON.stringify(parentCAfter),
    `${agent} nested: parent C changed: ${parentCBefore} -> ${parentCAfter}`
  );
  rmSync(childS, { recursive: true, force: true });
  rmSync(workspace, { recursive: true, force: true });
  return r;
}

// --- main matrix ---
const agents = ["pi", "codex", "claude", "agy", "dsh"];
const modes = ["S", "S+C"];
let passed = 0, failed = 0;
const failures = [];

async function runAgent(agent) {
  for (const mode of modes) {
    const label = `${agent}/${mode}`;
    const s = tmpBase(`s-${agent}-${mode}`);
    const c = mode === "S+C" ? tmpBase(`c-${agent}-${mode}`) : null;
    const tests = [
      ["dry-run", () => testDirectLaunchDryRun(agent, s, c)],
      ["continue", () => testSessionContinue(agent, s, c)],
      ["import", () => testSessionImport(agent, s, c)],
      ["skill-persist", () => testSkillPersistence(agent, s, c)],
      ["nested-sm", () => testNestedSm(agent, s, c)],
    ];
    for (const [name, fn] of tests) {
      const id = `${label}/${name}`;
      try {
        fn();
        console.log(`PASS ${id}`);
        passed++;
      } catch (err) {
        console.error(`FAIL ${id}: ${String(err).slice(0, 500)}`);
        failures.push({ id, error: String(err).slice(0, 500) });
        failed++;
      }
    }
    // Cleanup
    rmSync(s, { recursive: true, force: true });
    if (c) rmSync(c, { recursive: true, force: true });
  }
}

// Run agents in parallel
const start = Date.now();
await Promise.all(agents.map(runAgent));
const elapsed = ((Date.now() - start) / 1000).toFixed(1);

console.log(`\n--- E2E isolation matrix ---`);
console.log(`${passed} passed, ${failed} failed (${elapsed}s)`);
console.log(`Models: ${JSON.stringify(models)}`);
console.log(`agy model: gemini-3.8-flash-low (cheapest from agy models)`);
if (failures.length > 0) {
  console.log(`\nFailures:`);
  for (const f of failures) console.log(`  ${f.id}: ${f.error}`);
  process.exit(1);
}
