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
// Usage: selfbuild-extract-text.mjs <raw-file> <out-file>
//
// The no-text branch is shape-based, never flag-based:
//   - NDJSON (first non-blank line parses with a string "type") that never
//     emitted assistant text = the dispatch timed out or died mid-turn. Write
//     a small "[extract-aborted] ..." diagnostic, NOT the raw blob — a raw
//     research file re-bloats the PO (loop-90), a raw goal file poisons the
//     ledger (loop-81/90). The diagnostic lacks the "Goal:" prefix so the
//     driver's ^Goal: gate rejects it and records the iteration invalid.
//   - Plain text (e.g. a `claude -p` worker): the raw output IS the result,
//     pass it through unchanged.
// Callers keep the raw stream under a `.ndjson`/`.raw` scratch name for
// debugging; this helper only decides what lands in the final phase file.

import { readFileSync, writeFileSync } from "node:fs";

const [rawPath, outPath] = process.argv.slice(2);
if (!rawPath || !outPath) {
  console.error("usage: selfbuild-extract-text.mjs <raw-file> <out-file>");
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
} else {
  // No assistant text: discriminate the two worker shapes before deciding
  // what raw is. NDJSON (omp/pi --mode json) vs plain text (claude -p).
  const first = lines.find((l) => l.trim());
  let isNdjson = false;
  try {
    const o = JSON.parse(first);
    isNdjson = typeof o?.type === "string";
  } catch {
    /* plain text */
  }
  if (isNdjson) {
    // Aborted stream: small diagnostic, never the raw blob. No error-marker
    // scanning — in loop-90 the only errorMessage literals were a stale 503
    // quoted from a file the agent had read, not a live failure; surfacing it
    // would misattribute the cause.
    const evs = lines.filter((l) => l.trim()).length;
    writeFileSync(outPath, `[extract-aborted] no assistant text in ${evs} NDJSON events`);
  } else {
    // Plain-text worker: raw output IS the result.
    writeFileSync(outPath, raw);
  }
}
