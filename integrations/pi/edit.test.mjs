import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, copyFile, writeFile, readFile, chmod, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

test("captures pre-images for edits and writes and skips overlapping calls", async () => {
  const root = await mkdtemp(join(tmpdir(), "swarmia-pi-edit-"));
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
    const target = join(root, "file");
    await writeFile(target, "before\n");
    const edit = { toolName: "edit", toolCallId: "e", input: { path: "file", edits: [{ oldText: "before", newText: "after" }] } };
    await handlers.tool_call(edit, ctx);
    await writeFile(target, "after\n");
    await handlers.tool_result({ ...edit, isError: false }, ctx);
    const write = { toolName: "write", toolCallId: "w", input: { path: "new", content: "new\n" } };
    await handlers.tool_call(write, ctx);
    await writeFile(join(root, "new"), "new\n");
    await handlers.tool_result({ ...write, isError: false }, ctx);
    const a = { ...edit, toolCallId: "a" }, b = { ...edit, toolCallId: "b" };
    await handlers.tool_call(a, ctx); await handlers.tool_call(b, ctx);
    await handlers.tool_result({ ...a, isError: false }, ctx); await handlers.tool_result({ ...b, isError: false }, ctx);
    const failed = { toolName: "edit", toolCallId: "parent/1", input: { path: "missing", edits: [] } };
    await handlers.tool_call(failed, ctx);
    await handlers.tool_result({ ...failed, isError: true }, ctx);
    const events = (await readFile(join(root, "events"), "utf8")).trim().split("\n").map(JSON.parse);
    assert.equal(events.length, 3);
    assert.equal(events[0].original_file, "before\n");
    assert.equal(events[0].after_file, "after\n");
    assert.equal(events[0].tool_input.file_path, target);
    assert.equal(events[1].original_file, "");
    assert.equal(events[2].hook_event_name, "PostToolUseFailure");
    assert.equal(events[2].tool_use_id, "parent/1");
    assert.equal(events[2].original_file, undefined);
    // The event includes the observed content for CLI-side verification, even
    // when another handler or process changes the file around the call.
    const changed = { toolName: "write", toolCallId: "changed", input: { path: "@file", content: "intended" } };
    await handlers.tool_call(changed, ctx);
    await writeFile(target, "different");
    await handlers.tool_result({ ...changed, isError: false }, ctx);
    const last = (await readFile(join(root, "events"), "utf8")).trim().split("\n").map(JSON.parse).at(-1);
    assert.equal(last.original_file, "after\n");
    assert.equal(last.after_file, "different");
    assert.equal(last.tool_input.file_path, target);
    // A cancelled/failed call must not poison a subsequent call to the file.
    const cancelled = { ...edit, toolCallId: "cancelled" };
    await handlers.tool_call(cancelled, ctx);
    await handlers.tool_result({ ...cancelled, isError: true }, ctx);
    const next = { toolName: "write", toolCallId: "next", input: { path: "file", content: "final" } };
    await handlers.tool_call(next, ctx);
    await writeFile(target, "final");
    await handlers.tool_result({ ...next, isError: false }, ctx);
    const final = (await readFile(join(root, "events"), "utf8")).trim().split("\n").map(JSON.parse).at(-1);
    assert.equal(final.original_file, "different");
    assert.equal(final.after_file, "final");
    // Reload starts with no pending captures, including calls with no result.
    await handlers.tool_call({ ...edit, toolCallId: "abandoned" }, ctx);
    await handlers.session_start({}, ctx);
    const afterReload = { ...next, toolCallId: "reload", input: { ...next.input, content: "reloaded" } };
    await handlers.tool_call(afterReload, ctx);
    await writeFile(target, "reloaded");
    await handlers.tool_result({ ...afterReload, isError: false }, ctx);
    const reloaded = (await readFile(join(root, "events"), "utf8")).trim().split("\n").map(JSON.parse).at(-1);
    assert.equal(reloaded.after_file, "reloaded");
  } finally { await rm(root, { recursive: true, force: true }); }
});
