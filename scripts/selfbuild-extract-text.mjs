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
// HOT-LOADED: the driver invokes this BY PATH at extraction time
// (`node "$REPO/scripts/selfbuild-extract-text.mjs" ...`), so it is NOT
// baked into bash's in-memory script — editing it in place changes behavior
// for the running iteration with no restart boundary. Always stage a new
// version to a temp path, run the fixtures against the temp copy, then
// `mv` it into place (atomic rename, never a partial file).
//
// Usage: selfbuild-extract-text.mjs <raw-file> <out-file> [aborted-file]
//   <aborted-file> is where an aborted NDJSON stream is preserved for triage.
//   It is honoured only when it is a real (non-dash) path: the driver is
//   hot-loaded, so an older in-memory version may still pass `--sentinel`
//   here, and the PO's <out> lives in the repo cwd where a stray copy would
//   dirty the tree. Callers that want triage copies pass a path under $STATE
//   (gitignored, never pushed).
//
// The no-text branch is shape-based, never flag-based:
//   - assistant text found (omp/pi `message_end`, or a `claude -p
//     --output-format json` `result`) -> write that text.
//   - empty input (both dispatch paths died before writing) -> a small
//     "[extract-aborted] empty worker output" diagnostic, never a blank file
//     (a blank goal is a zero-signal invalid row).
//   - NDJSON with no assistant text = the dispatch timed out or died mid-turn.
//     Write a small "[extract-aborted] ..." diagnostic WITHOUT the "Goal:"
//     prefix (so the driver's ^Goal: gate rejects it) and, when an explicit
//     path is given, preserve the raw stream there for triage — never publish
//     the raw blob to <out> itself (a raw research file re-bloats the PO; a
//     raw goal poisons the ledger).
//   - plain text (e.g. a `claude -p` worker without --output-format json):
//     the raw output IS the result, pass it through unchanged.

import { readFileSync, writeFileSync } from "node:fs";

const [rawPath, outPath, abortedPathArg] = process.argv.slice(2);
if (!rawPath || !outPath) {
  console.error(
    "usage: selfbuild-extract-text.mjs <raw-file> <out-file> [aborted-file]",
  );
  process.exit(2);
}

const raw = readFileSync(rawPath, "utf8");

// Empty input: both dispatch paths died before writing anything. Publish a
// diagnostic, never an empty file — a blank goal yields a zero-signal
// `invalid` ledger row that tells the operator nothing.
if (!raw.trim()) {
  writeFileSync(outPath, "[extract-aborted] empty worker output");
  process.exit(0);
}

const lines = raw.split("\n");
let out = "";
for (const line of lines) {
  try {
    const o = JSON.parse(line);
    // omp/pi --mode json: assistant text arrives on message_end.
    if (o.type === "message_end" && o.message?.role === "assistant") {
      for (const c of o.message.content ?? []) {
        if (c.type === "text" && c.text) out = c.text;
      }
    }
    // claude -p --output-format json: a single {"type":"result","result":"..."}
    // line. Without this, its first line parses as NDJSON (string "type") with
    // no message_end, so the worker's actual answer would be discarded as an
    // abort. SELFBUILD_*_BIN is overridable, so this shape is a live possibility.
    if (o.type === "result" && typeof o.result === "string" && o.result) {
      out = o.result;
    }
  } catch {
    /* non-JSON line: not an event, ignore */
  }
}

if (out) {
  writeFileSync(outPath, out);
  process.exit(0);
}

// No assistant text: discriminate the two worker shapes before deciding what
// raw is. NDJSON (omp/pi --mode json) vs plain text (claude -p).
const first = lines.find((l) => l.trim());
let isNdjson = false;
try {
  const o = JSON.parse(first);
  isNdjson = typeof o?.type === "string";
} catch {
  /* plain text */
}

if (!isNdjson) {
  // Plain-text worker: raw output IS the result.
  writeFileSync(outPath, raw);
  process.exit(0);
}

// Aborted NDJSON stream. Preserve the raw for triage ONLY when the caller
// passed an explicit non-dash destination (a path under $STATE, gitignored and
// never pushed). This whole root-cause chain was diagnosable only because a
// prior version left raw on disk; the bare sentinel would otherwise name a
// count with nothing to inspect.
const abPath =
  abortedPathArg && !abortedPathArg.startsWith("-") ? abortedPathArg : null;
if (abPath) {
  try {
    writeFileSync(abPath, raw);
  } catch {
    /* best-effort; the diagnostic below still lands */
  }
}
const evs = lines.filter((l) => l.trim()).length;
writeFileSync(
  outPath,
  `[extract-aborted] no assistant text in ${evs} NDJSON events` +
    (abPath ? ` (raw kept at ${abPath})` : ""),
);
