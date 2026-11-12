import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, copyFile, writeFile, readFile, chmod, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

test("captures user and final assistant text, not tool output or reasoning", async () => {
  const root = await mkdtemp(join(tmpdir(), "swarmia-pi-messages-"));
  try {
    const dir = join(root, ".pi", "extensions");
    await mkdir(dir, { recursive: true });
    const extension = join(dir, "swarmia.mjs");
    await copyFile(new URL("./swarmia.js", import.meta.url), extension);
    await writeFile(join(root, "swarmia-cli"), "#!/bin/sh\ncat >> events\nprintf '\\n' >> events\n");
    await chmod(join(root, "swarmia-cli"), 0o755);
    const handlers = {};
    (await import(pathToFileURL(extension))).default({ on: (name, fn) => { handlers[name] = fn; } });
    const ctx = { cwd: root, sessionManager: { getSessionId: () => "s" } };
    const message = (m) => handlers.message_end({ message: m }, ctx);
    await message({ role: "user", content: [{ type: "text", text: "first" }, { type: "image", data: "private image" }] });
    await message({ role: "assistant", stopReason: "toolUse", content: [{ type: "text", text: "narration" }] });
    await message({ role: "user", content: "steering" });
    await message({ role: "toolResult", content: [{ type: "text", text: "private tool output" }] });
    await message({ role: "assistant", stopReason: "error", content: [{ type: "text", text: "retry" }] });
    await message({ role: "assistant", stopReason: "aborted", content: [{ type: "text", text: "partial" }] });
    await message({ role: "assistant", stopReason: "stop", content: [{ type: "thinking", thinking: "private reasoning" }, { type: "text", text: "final" }] });
    await message({ role: "user", content: "follow-up" });
    await message({ role: "assistant", stopReason: "stop", content: [{ type: "text", text: "done" }] });
    const events = (await readFile(join(root, "events"), "utf8")).trim().split("\n").map(JSON.parse);
    assert.deepEqual(events.map((e) => [e.hook_event_name, e.prompt ?? e.last_assistant_message]), [
      ["UserPromptSubmit", "first"], ["UserPromptSubmit", "steering"], ["Stop", "final"],
      ["UserPromptSubmit", "follow-up"], ["Stop", "done"],
    ]);
    for (const secret of ["private image", "private tool output", "private reasoning", "narration", "partial", "retry"]) {
      assert.ok(!JSON.stringify(events).includes(secret));
    }
  } finally { await rm(root, { recursive: true, force: true }); }
});
