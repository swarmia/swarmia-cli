// Project-scoped Pi extension prototype. Copy to .pi/extensions/swarmia.js
// after building swarmia-cli in the project root. See README.md in this folder.
import { spawn } from "node:child_process";
import { access, open, realpath } from "node:fs/promises";
import { homedir } from "node:os";
import { constants } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const projectRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const timeoutMs = 3000;
const maxFileBytes = 2 * 1024 * 1024;

// Match Pi's resolveToCwd normalization for the supported local paths.
function toolPath(input, cwd) {
  let path = input.replace(/[\u00a0\u2000-\u200a\u202f\u205f\u3000]/g, " ");
  if (path.startsWith("@")) path = path.slice(1);
  if (process.platform === "win32" && path.startsWith("/") && !path.startsWith("//") && !path.includes("\\")) {
    const match = path.match(/^\/(?:mnt\/|cygdrive\/)?([a-z])(?:\/(.*))?$/i);
    if (match) path = `${match[1].toUpperCase()}:\\${(match[2] ?? "").replaceAll("/", "\\")}`;
  }
  if (path === "~" || path.startsWith("~/") || (process.platform === "win32" && path.startsWith("~\\"))) path = homedir() + path.slice(1);
  if (path.startsWith("file://")) path = fileURLToPath(path);
  return resolve(cwd, path);
}

async function readText(path, missingIsEmpty = false) {
  let file;
  try {
    file = await open(path, "r");
    const stat = await file.stat();
    if (!stat.isFile() || stat.size > maxFileBytes) return;
    // Read at most the limit even if the file grows while being read.
    const buffer = Buffer.alloc(maxFileBytes + 1);
    const { bytesRead } = await file.read(buffer, 0, buffer.length, 0);
    const afterStat = await file.stat();
    if (bytesRead > maxFileBytes || bytesRead !== stat.size || afterStat.size !== stat.size ||
        afterStat.mtimeMs !== stat.mtimeMs || afterStat.ctimeMs !== stat.ctimeMs) return;
    const bytes = buffer.subarray(0, bytesRead);
    if (bytes.includes(0)) return;
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch (err) {
    if (err.code === "ENOENT" && missingIsEmpty) return "";
  } finally {
    await file?.close();
  }
}

async function findBinary() {
  for (const path of [resolve(projectRoot, "swarmia-cli"), resolve(projectRoot, "node_modules/.bin/swarmia-cli")]) {
    try {
      await access(path, constants.X_OK);
      return path;
    } catch {
      // A project without its own CLI must never fall back to a global binary.
    }
  }
}

async function logHook(ctx, event) {
  try {
    const binary = await findBinary();
    if (!binary) return;
    const payload = JSON.stringify({
      session_id: ctx.sessionManager.getSessionId(),
      cwd: ctx.cwd,
      model: ctx.model?.id,
      ...event,
    });
    await new Promise((done) => {
      const env = { ...process.env };
      delete env.CLAUDE_PROJECT_DIR;
      const child = spawn(binary, ["log-hook", "--agent", "pi"], {
        cwd: ctx.cwd, env, stdio: ["pipe", "ignore", "ignore"],
      });
      const timer = setTimeout(() => child.kill(), timeoutMs);
      const finish = () => { clearTimeout(timer); done(); };
      child.on("error", finish);
      child.on("close", finish);
      child.stdin.on("error", () => {});
      child.stdin.end(payload);
    });
  } catch {
    // Tracking is best-effort and must never prevent a tool from running.
  }
}

export default function (pi) {
  const pending = new Map();
  pi.on("session_start", async (_event, ctx) => {
    pending.clear();
    await logHook(ctx, { hook_event_name: "SessionStart" });
  });
  pi.on("tool_call", async (event, ctx) => {
    if (event.toolName === "bash") {
      await logHook(ctx, {
        hook_event_name: "PreToolUse", tool_name: "bash",
        tool_use_id: event.toolCallId, tool_input: { command: event.input.command },
      });
      return;
    }
    if (!["edit", "write"].includes(event.toolName) || typeof event.input.path !== "string") return;
    try {
      const path = toolPath(event.input.path, ctx.cwd);
      // Existing symlink aliases point at the same mutation queue.
      const key = await realpath(path).catch(() => path);
      // Concurrent writes to the same file cannot be reliably bracketed by
      // extension events. Skip attribution for all overlapping calls.
      const snapshot = { path, key, original: undefined, unsafe: false };
      for (const other of pending.values()) {
        if (other.key === key) other.unsafe = snapshot.unsafe = true;
      }
      if (pending.size >= 100) return;
      pending.set(event.toolCallId, snapshot);
      snapshot.original = await readText(path, event.toolName === "write");
    } catch {
      // Never block Pi's tool if capturing a pre-image fails.
    }
  });
  pi.on("tool_result", async (event, ctx) => {
    if (event.toolName === "bash") {
      await logHook(ctx, {
        hook_event_name: event.isError ? "PostToolUseFailure" : "PostToolUse",
        tool_name: "bash", tool_use_id: event.toolCallId,
        tool_input: { command: event.input.command },
      });
      return;
    }
    if (!["edit", "write"].includes(event.toolName)) return;
    const snapshot = pending.get(event.toolCallId);
    try {
      if (typeof event.input.path !== "string") return;
      const path = toolPath(event.input.path, ctx.cwd);
      if (event.isError) {
        await logHook(ctx, {
          hook_event_name: "PostToolUseFailure", tool_name: event.toolName,
          tool_use_id: event.toolCallId, tool_input: { file_path: path },
        });
        return;
      }
      if (!snapshot || snapshot.unsafe || snapshot.original === undefined || path !== snapshot.path) return;
      // Verify the observed result in the CLI before attributing. It detects
      // stale pre-images and inputs changed by later extension handlers.
      const after = await readText(snapshot.path);
      if (snapshot.unsafe || after === undefined) return;
      await logHook(ctx, {
        hook_event_name: "PostToolUse",
        tool_name: event.toolName, tool_use_id: event.toolCallId,
        tool_input: { file_path: snapshot.path, content: event.input.content, edits: event.input.edits },
        original_file: snapshot.original,
        after_file: after,
      });
    } catch {
      // Tracking errors must not affect the tool result.
    } finally {
      pending.delete(event.toolCallId);
    }
  });
  pi.on("session_shutdown", async (_event, ctx) => {
    await logHook(ctx, { hook_event_name: "SessionEnd" });
  });
}
