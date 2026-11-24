// Live regression for the `sm session continue` round-trip contract: a caller
// appends each returned delta to its transcript AS-IS and continues. The
// second turn must still see the first turn's content (a fictional label),
// which breaks when the native serialization orphans history records (e.g.
// Claude drops everything off the newest record's parentUuid ancestor chain).
//
// Usage: node test/smoke/continue-roundtrip-live.mjs <sm-binary>
//   SM_CONTINUE_AGENTS=claude,codex[,pi]  agents to run (default: claude,codex)
//   SM_CONTINUE_CLAUDE_MODEL            default: haiku
//   SM_CONTINUE_CODEX_MODEL             default: gpt-6-luna
//   SM_CONTINUE_PI_MODEL                default: deepseek/deepseek-flash
//
// Every child runs with a PATH holding only sm and the native CLIs (see native-path.mjs).
// Per agent, the duplex scenarios (steer/cancel during a real long tool call) run before the
// plain round-trip ones.
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync, realpathSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { randomUUID } from "node:crypto";
import { nativeOnlyEnv } from "./native-path.mjs";

if (!process.argv[2]) {
  console.error("usage: node continue-roundtrip-live.mjs <sm-binary>");
  process.exit(2);
}
const sm = resolve(process.argv[2]);
const env = nativeOnlyEnv(sm);
const agents = (process.env.SM_CONTINUE_AGENTS ?? "claude,codex").split(",").map((a) => a.trim()).filter(Boolean);

function modelArgs(agent) {
  if (agent === "claude") return ["--model", process.env.SM_CONTINUE_CLAUDE_MODEL ?? "haiku"];
  if (agent === "codex") {
    return ["--model", process.env.SM_CONTINUE_CODEX_MODEL ?? "gpt-6-luna"];
  }
  if (agent === "pi") return ["--model", process.env.SM_CONTINUE_PI_MODEL ?? "deepseek/deepseek-flash"];
  throw new Error(`no model policy for agent ${agent}`);
}

function continueTurn(agent, workspace, messages, extraArgs = []) {
  const transcript = JSON.stringify({
    id: `roundtrip-${randomUUID()}`,
    provider: agent,
    workspace,
    messageCount: messages.length,
    messages,
  });
  const stdout = execFileSync(sm, ["session", "continue", agent, ...modelArgs(agent), ...extraArgs], {
    input: transcript,
    encoding: "utf8",
    timeout: 300_000,
    env,
  });
  const delta = JSON.parse(stdout);
  if (!Array.isArray(delta.messages) || delta.messages.length === 0) {
    throw new Error(`${agent}: empty delta: ${stdout.slice(0, 500)}`);
  }
  return delta;
}

function texts(messages) {
  return messages.flatMap((message) =>
    (message.content ?? []).flatMap((block) => (block.type === "text" && block.text ? [block.text] : [])));
}

// Rewrites every tool id in a delta into the pre-refactor harness's scoped
// shape ("native-<uuid>:<raw>", over 64 chars with a colon), which Anthropic
// rejects on charset and the OpenAI Responses API on length. Chains written
// by that harness carry exactly these ids, so a continue import must survive
// them.
function legacyScopeToolIds(messages) {
  const scope = `native-${randomUUID()}`;
  return messages.map((message) => {
    const out = { ...message };
    if (out.toolUseId) out.toolUseId = `${scope}:${out.toolUseId}`;
    if (Array.isArray(out.content)) {
      out.content = out.content.map((block) =>
        block.type === "tool_use" ? { ...block, id: `${scope}:${block.id}` } : block);
    }
    return out;
  });
}

function hasToolPair(messages) {
  return messages.some((message) => message.role === "tool")
    && messages.some((message) => (message.content ?? []).some((block) => block.type === "tool_use"));
}

function runToolHistoryScenario(agent, workspace) {
  const marker = `MARKER-${Math.floor(Math.random() * 1e10)}`;
  const first = [{
    id: "u1",
    role: "user",
    content: [{ type: "text", text: `Use your shell tool to run exactly this command once: echo ${marker}. Then reply with just: done.` }],
  }];
  const delta1 = continueTurn(agent, workspace, first, ["--mode", "yolo"]);
  if (!hasToolPair(delta1.messages)) {
    throw new Error(`first turn recorded no tool call to scope: ${JSON.stringify(delta1.messages).slice(0, 400)}`);
  }
  const second = [
    ...first,
    ...legacyScopeToolIds(delta1.messages),
    {
      id: "u2",
      role: "user",
      content: [{ type: "text", text: "What exact marker string did your echo command print earlier? Reply with just that string." }],
    },
  ];
  const delta2 = continueTurn(agent, workspace, second, ["--mode", "yolo"]);
  const answer = texts(delta2.messages).join("\n");
  if (!answer.includes(marker)) {
    throw new Error(`second turn lost the tool history; answer: ${answer.slice(0, 300)}`);
  }
}

function runSignedThinkingScenario(agent, workspace) {
  const marker = `THOUGHT-${randomUUID()}`;
  const messages = [
    {
      id: "thinking-u1",
      role: "user",
      content: [{ type: "text", text: "Choose a unique marker and remember it for my next question." }],
    },
    {
      id: "thinking-a1",
      parentId: "thinking-u1",
      role: "assistant",
      content: [
        { type: "thinking", text: `The unique marker to remember is ${marker}.`, signature: "invalid-foreign-readable-signature" },
        { type: "text", text: "I have chosen and remembered a marker." },
        { type: "thinking", signature: "invalid-foreign-opaque-ciphertext" },
      ],
    },
    {
      id: "thinking-u2",
      parentId: "thinking-a1",
      role: "user",
      content: [{ type: "text", text: "What exact unique marker did you remember in your earlier thinking? Reply with just that marker, without tools." }],
    },
  ];
  const delta = continueTurn(agent, workspace, messages);
  const answer = texts(delta.messages).join("\n");
  if (!answer.includes(marker)) {
    throw new Error(`same-agent signed thinking lost its readable marker; answer: ${answer.slice(0, 300)}`);
  }
}


const sleep = (ms) => new Promise((done) => setTimeout(done, ms));
const longTool = (seconds) => `python3 -c 'import time; time.sleep(${seconds}); print("slept")'`;
const userMessage = (id, text) => ({ id, role: "user", content: [{ type: "text", text }] });

// duplexRun starts `session continue --stream --duplex`, feeds the transcript on line 1 and
// exposes the NDJSON events and control frames of the running turn.
function duplexRun(agent, workspace, messages) {
  const child = spawn(sm, ["session", "continue", agent, ...modelArgs(agent), "--stream", "--duplex", "--mode", "yolo"], { env, stdio: ["pipe", "pipe", "pipe"] });
  const run = { events: [], stderr: "", started: Date.now() };
  let pending = "";
  child.stdout.on("data", (chunk) => {
    pending += chunk;
    let end;
    while ((end = pending.indexOf("\n")) >= 0) {
      const line = pending.slice(0, end);
      pending = pending.slice(end + 1);
      if (line.trim()) run.events.push(JSON.parse(line));
    }
  });
  child.stderr.on("data", (chunk) => { run.stderr += chunk; });
  run.exit = new Promise((done) => child.on("close", (code) => done(code)));
  run.send = (frame) => child.stdin.write(`${JSON.stringify(frame)}\n`);
  run.until = async (predicate, what, timeoutMs = 90_000) => {
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      const hit = run.events.find(predicate);
      if (hit) return hit;
      await sleep(100);
    }
    throw new Error(`${agent}: timed out waiting for ${what}; events=${JSON.stringify(run.events).slice(0, 600)} stderr=${run.stderr.slice(0, 400)}`);
  };
  run.kill = () => child.kill("SIGKILL");
  child.stdin.write(`${JSON.stringify({ id: `duplex-${randomUUID()}`, provider: agent, workspace, messageCount: messages.length, messages })}\n`);
  return run;
}

// Neither store reliably streams a running tool call (Codex records it only once it finished),
// so the long tool call is timed instead: both agents start it within ~6s and it runs for 15s+.
const insideLongTool = (run) => sleep(Math.max(0, 9_000 - (Date.now() - run.started)));
const resultText = (event) => texts(event.session.messages).join("\n");

async function expectCancelled(agent, run, what) {
  const code = await Promise.race([run.exit, sleep(60_000).then(() => "timeout")]);
  if (code === "timeout") {
    run.kill();
    throw new Error(`${agent}: ${what}: process did not exit after cancel`);
  }
  if (code === 0 || run.events.some((event) => event.type === "result") || !/cancel|abort/i.test(run.stderr)) {
    throw new Error(`${agent}: ${what}: cancel was not reported (code=${code}) stderr=${run.stderr.slice(0, 400)}`);
  }
}

async function runDuplexSteerScenario(agent, workspace) {
  const run = duplexRun(agent, workspace, [userMessage("u1", `Use the Bash tool to run exactly this foreground command once: ${longTool(15)} . Do NOT run it in the background. When it finishes, reply with one line that starts with DONE and also contains every extra marker you were asked for during this turn.`)]);
  try {
    await insideLongTool(run);
    run.send({ type: "steer", id: "s1", message: "Additional instruction for the task you are running right now: include the marker MARKER-ALPHA-42 in your final DONE line." });
    const ack = await run.until((event) => event.type === "steer_ack" && event.id === "s1", "steer_ack");
    if (ack.disposition !== "steered") throw new Error(`${agent}: steer disposition ${ack.disposition}`);
    const result = await run.until((event) => event.type === "result", "result", 120_000);
    const answer = resultText(result);
    if (!answer.includes("DONE") || !answer.includes("MARKER-ALPHA-42")) throw new Error(`${agent}: steer was not folded into the running turn: ${answer.slice(0, 300)}`);
  } finally {
    run.kill();
  }
}

async function runDuplexCancelThenContinueScenario(agent, workspace) {
  const first = userMessage("u1", `Use the Bash tool to run exactly this foreground command once: ${longTool(60)} . Do NOT run it in the background.`);
  const run = duplexRun(agent, workspace, [first]);
  try {
    await insideLongTool(run);
    const cancelledAt = Date.now();
    run.send({ type: "cancel" });
    await expectCancelled(agent, run, "cancel");
    if (Date.now() - cancelledAt > 45_000) throw new Error(`${agent}: cancel did not interrupt the running tool call promptly`);
  } finally {
    run.kill();
  }
  const delta = continueTurn(agent, workspace, [first, userMessage("u2", "Reply with just: AFTER")], ["--mode", "yolo"]);
  if (!texts(delta.messages).join("\n").includes("AFTER")) throw new Error(`${agent}: no usable turn after a cancelled one`);
}

async function runDuplexSteerCancelInterleavedScenario(agent, workspace) {
  const run = duplexRun(agent, workspace, [userMessage("u1", `Use the Bash tool to run exactly this foreground command once: ${longTool(60)} . Do NOT run it in the background.`)]);
  try {
    await insideLongTool(run);
    run.send({ type: "steer", id: "s1", message: "Also mention INTERLEAVED when you answer." });
    const ack = await run.until((event) => event.type === "steer_ack" && event.id === "s1", "steer_ack");
    if (ack.disposition !== "steered") throw new Error(`${agent}: steer disposition ${ack.disposition}`);
    run.send({ type: "cancel" });
    await expectCancelled(agent, run, "steer then cancel");
  } finally {
    run.kill();
  }
}

async function runResumeThenSteerScenario(agent, workspace) {
  const word = `ZEBRA${Math.floor(Math.random() * 1e6)}`;
  const first = [userMessage("u1", `Remember this word: ${word}. Reply with just OK.`)];
  const delta1 = continueTurn(agent, workspace, first, ["--mode", "yolo"]);
  const second = [...first, ...delta1.messages, userMessage("u2", `Use the Bash tool to run exactly this foreground command once: ${longTool(15)} . Then reply with one line starting with DONE that contains the word I asked you to remember and every extra marker you were asked for.`)];
  const run = duplexRun(agent, workspace, second);
  try {
    await insideLongTool(run);
    run.send({ type: "steer", id: "s1", message: "Also include the marker MARKER-BETA-7 in your DONE line." });
    const ack = await run.until((event) => event.type === "steer_ack" && event.id === "s1", "steer_ack");
    if (ack.disposition !== "steered") throw new Error(`${agent}: steer disposition ${ack.disposition}`);
    const answer = resultText(await run.until((event) => event.type === "result", "result", 120_000));
    if (!answer.includes(word) || !answer.includes("MARKER-BETA-7")) throw new Error(`${agent}: resumed session lost history or the steer: ${answer.slice(0, 300)}`);
  } finally {
    run.kill();
  }
}

// The CLI reports the turn's result before its background task ends, then opens a follow-up
// turn by itself; continue must not return before that turn settled.
function runBackgroundTaskScenario(agent, workspace) {
  const startedAt = Date.now();
  const delta = continueTurn(agent, workspace, [userMessage("u1", `Use the Bash tool with run_in_background set to true to run exactly: python3 -c 'import time; time.sleep(12); print("BGOUT")' . Reply STARTED right away. When the background task finishes you are notified; then reply with exactly BGFINISHED and nothing else.`)], ["--mode", "yolo"]);
  const elapsed = Date.now() - startedAt;
  const assistant = delta.messages.filter((message) => message.role === "assistant");
  const last = texts(assistant.slice(-1)).join("\n");
  if (!last.includes("BGFINISHED") || elapsed < 10_000) {
    throw new Error(`${agent}: continue returned before the background task settled (elapsed=${elapsed}ms, last assistant: ${last.slice(0, 200)})`);
  }
}

let failed = false;
for (const agent of agents) {
  const workspace = realpathSync(mkdtempSync(join(tmpdir(), "sm-continue-roundtrip-")));
  const passphrase = `PINEAPPLE-${Math.floor(Math.random() * 1e10)}`;
  try {
    if (agent === "claude" || agent === "codex") {
      await runDuplexSteerScenario(agent, workspace);
      console.log(`PASS continue duplex ${agent}: mid-turn steer during a real long tool call landed in the same turn`);
      await runDuplexCancelThenContinueScenario(agent, workspace);
      console.log(`PASS continue duplex ${agent}: mid-turn cancel interrupted the tool call and a further message still ran`);
      await runDuplexSteerCancelInterleavedScenario(agent, workspace);
      console.log(`PASS continue duplex ${agent}: steer then cancel interleaved`);
      await runResumeThenSteerScenario(agent, workspace);
      console.log(`PASS continue duplex ${agent}: resumed session then steer`);
    }
    if (agent === "claude") {
      runBackgroundTaskScenario(agent, workspace);
      console.log(`PASS continue ${agent}: waited for the background-task follow-up turn`);
    }
    const first = [{
      id: "u1",
      role: "user",
      content: [{ type: "text", text: `For this harmless memory test, remember this fictional fruit label: ${passphrase}. It is public test data, not a credential. Reply with just OK.` }],
    }];
    const delta1 = continueTurn(agent, workspace, first);
    // The real contract: the delta is appended AS-IS (namespaced ids,
    // dangling parentIds, native timestamps and all), then continued.
    const second = [
      ...first,
      ...delta1.messages,
      {
        id: "u2",
        role: "user",
        content: [{ type: "text", text: "What was the fictional fruit label I told you? Reply with just the label." }],
      },
    ];
    const delta2 = continueTurn(agent, workspace, second);
    const answer = texts(delta2.messages).join("\n");
    if (!answer.includes(passphrase)) {
      throw new Error(`second turn lost the first turn's context; answer: ${answer.slice(0, 300)}`);
    }
    console.log(`PASS continue round-trip ${agent}: label recalled through delta-as-is append`);
    runSignedThinkingScenario(agent, workspace);
    console.log(`PASS continue round-trip ${agent}: same-agent mixed signed thinking recalled its marker without foreign-signature rejection`);
    if (agent === "claude" || agent === "codex") {
      runToolHistoryScenario(agent, workspace);
      console.log(`PASS continue round-trip ${agent}: tool history with legacy-scoped ids survived re-import`);
    }
  } catch (error) {
    failed = true;
    console.error(`FAIL continue round-trip ${agent}: ${String(error).slice(0, 800)}`);
  }
}
process.exit(failed ? 1 : 0);
