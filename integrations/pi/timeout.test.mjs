import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, mkdir, copyFile, writeFile, chmod, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

test("a hook that ignores SIGTERM and leaves a pipe open cannot stall Pi", { timeout: 7000 }, async () => {
  const root = await mkdtemp(join(tmpdir(), "swarmia-pi-timeout-"));
  try {
    const dir = join(root, ".pi", "extensions");
    await mkdir(dir, { recursive: true });
    const extension = join(dir, "swarmia.mjs");
    await copyFile(new URL("./swarmia.js", import.meta.url), extension);
    const binary = join(root, "swarmia-cli");
    await writeFile(binary, "#!/bin/sh\ntrap '' TERM\nsleep 10\n");
    await chmod(binary, 0o755);
    const handlers = {};
    (await import(pathToFileURL(extension))).default({ on: (name, fn) => { handlers[name] = fn; } });
    const ctx = { cwd: root, sessionManager: { getSessionId: () => "s" } };
    const start = Date.now();
    await handlers.session_start({}, ctx);
    assert.ok(Date.now() - start < 6000, "hook must finish without waiting for the child or its descendants");
  } finally { await rm(root, { recursive: true, force: true }); }
});
