#!/usr/bin/env node
// Extract the assistant's final text from an omp/pi `--mode json` NDJSON
// event stream (or pass a plain-text worker's output through unchanged).
//
// Shared by the selfbuild loop's research and PO phases: both dispatch
// headless workers whose stdout is an event stream, and the phase files
// downstream agents read must contain the final text, not the raw stream.
// Loop-90 proved why this matters twice over: the PO's goal file received
// the raw stream (poison ledger row), and the PO itself timed out chewing
// the 2.3 MB raw research file its prompt told it to read.
//
// Usage: selfbuild-extract-text.mjs <raw-file> <out-file> [--sentinel]
//   --sentinel  NDJSON with no assistant text writes a small
//               "[extract-aborted] ..." diagnostic WITHOUT the "Goal:"
//               prefix (so the driver's ^Goal: gate rejects it) instead of
//               passing the raw stream through. Both phases want this: a
//               PO raw fallback poisons the ledger goal, and a research raw
//               fallback hands the PO a multi-megabyte stream to chew
//               through — the preventable cause of loop-90's 600s timeout.

import { readFileSync, writeFileSync } from "node:fs";

const [rawPath, outPath, ...flags] = process.argv.slice(2);
if (!rawPath || !outPath) {
  console.error("usage: selfbuild-extract-text.mjs <raw-file> <out-file> [--sentinel]");
  process.exit(2);
}

const raw = readFileSync(rawPath, "utf8");
const lines = raw.split("\n");
let out = "";
for (const line of lines) {
  try {
    const o = JSON.parse(line);
    if (o.type === "message_end" && o.message?.role === "assistant") {
      for (const c of o.message.content ?? []) {
        if (c.type === "text" && c.text) out = c.text;
      }
    }
  } catch {
    /* non-JSON line: not an event, ignore */
  }
}

if (out) {
  writeFileSync(outPath, out);
} else if (flags.includes("--sentinel")) {
  // NDJSON with no assistant text = dispatch timed out or died mid-turn.
  // NEVER fall back to raw: that dumped `{"type":"session",...}` into the
  // goal file, which record() published as the ledger goal (loop-81/90
  // poison rows). The sentinel deliberately lacks the "Goal:" prefix so
  // the ^Goal: gate rejects it and the iteration records invalid with a
  // readable diagnostic instead of dispatching a 7200s task on garbage.
  // No error-marker scanning: in loop-90 the only errorMessage literals
  // were a stale 503 quoted from a file the agent had read, not a live
  // failure — surfacing it would misattribute the cause.
  const evs = lines.filter((l) => l.trim()).length;
  const first = lines.find((l) => l.trim());
  let isNdjson = false;
  try {
    const o = JSON.parse(first);
    isNdjson = typeof o?.type === "string";
  } catch {
    /* plain text */
  }
  if (isNdjson) {
    writeFileSync(outPath, `[extract-aborted] no assistant text in ${evs} NDJSON events`);
  } else {
    // Plain-text worker (e.g. claude -p): raw output IS the goal.
    writeFileSync(outPath, raw);
  }
} else {
  // No sentinel requested: pass raw through (plain-text worker, or a
  // research stream kept for debugging when no final text arrived).
  writeFileSync(outPath, raw);
}
