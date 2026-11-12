import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, copyFile, writeFile, readFile, chmod, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

test("project extension brackets bash calls and ignores other tools", async () => {
  const root = await mkdtemp(join(tmpdir(), "swarmia-pi-"));
  try {
    const dir = join(root, ".pi", "extensions");
    await mkdir(dir, { recursive: true });
    const extension = join(dir, "swarmia.mjs");
    await copyFile(new URL("./swarmia.js", import.meta.url), extension);
    const binary = join(root, "swarmia-cli");
    await writeFile(binary, `#!/bin/sh\nprintf '%s\n' "$*" >> args\ncat >> events\nprintf '\n' >> events\n`);
    await chmod(binary, 0o755);
    const handlers = {};
    (await import(pathToFileURL(extension))).default({ on: (name, fn) => { handlers[name] = fn; } });
    const ctx = { cwd: root, model: { id: "test-model" }, sessionManager: { getSessionId: () => "session" } };
    await handlers.session_start({}, ctx);
    await handlers.tool_call({ toolName: "read", input: {} }, ctx);
    await handlers.tool_call({ toolName: "bash", toolCallId: "nested/1", input: { command: "echo hi" } }, ctx);
    await handlers.tool_result({ toolName: "bash", toolCallId: "nested/1", input: { command: "echo hi" }, isError: true, content: [{ text: "secret output" }] }, ctx);
    await handlers.session_shutdown({}, ctx);
    const events = (await readFile(join(root, "events"), "utf8")).trim().split("\n").map(JSON.parse);
    assert.deepEqual(events.map((e) => e.hook_event_name), ["SessionStart", "PreToolUse", "PostToolUseFailure", "SessionEnd"]);
    assert.equal(events[1].tool_use_id, "nested/1");
    assert.equal(events[2].tool_input.command, "echo hi");
    assert.ok(!JSON.stringify(events).includes("secret output"));
    assert.equal((await readFile(join(root, "args"), "utf8")).trim().split("\n")[0], "log-hook --agent pi");
    await rm(binary);
    await handlers.tool_call({ toolName: "bash", toolCallId: "missing", input: { command: "true" } }, ctx);
    assert.equal((await readFile(join(root, "events"), "utf8")).trim().split("\n").length, 4);
  } finally {
    await rm(root, { recursive: true, force: true });
  }
});
