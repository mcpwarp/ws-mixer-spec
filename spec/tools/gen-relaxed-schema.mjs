#!/usr/bin/env node
// Generates spec/control.relaxed.schema.json from spec/control.schema.json:
// the strict schema with every `additionalProperties: false` removed, so
// unknown fields on a known `t` are tolerated -- matching OVERVIEW.md section
// 2.7's forward-compatibility rule for the wire itself (the strict schema is
// deliberately more restrictive, as a test-time typo-catcher; the relaxed
// schema is what actually models wire validity).
//
// Node built-ins only. No dependencies, no package.json.
//
// Usage: node spec/tools/gen-relaxed-schema.mjs [--check]
//   --check   don't write; exit 1 if the checked-in file would change (used
//             by check-fixtures.mjs to catch drift between the two files)

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SPEC_DIR = path.resolve(__dirname, "..");

const STRICT_PATH = path.join(SPEC_DIR, "control.schema.json");
const RELAXED_PATH = path.join(SPEC_DIR, "control.relaxed.schema.json");

function stripAdditionalPropertiesFalse(node) {
  if (Array.isArray(node)) {
    return node.map(stripAdditionalPropertiesFalse);
  }
  if (node !== null && typeof node === "object") {
    const out = {};
    for (const [key, val] of Object.entries(node)) {
      if (key === "additionalProperties" && val === false) continue;
      out[key] = stripAdditionalPropertiesFalse(val);
    }
    return out;
  }
  return node;
}

export function generateRelaxedSchema() {
  const strict = JSON.parse(fs.readFileSync(STRICT_PATH, "utf8"));
  const relaxed = stripAdditionalPropertiesFalse(strict);
  relaxed.$id = "https://mcpwarp.io/ws-mixer/v1/control.relaxed.schema.json";
  relaxed.title = "ws-mixer.v1 control channel messages (relaxed, wire-accurate)";
  relaxed.description =
    "GENERATED from control.schema.json by tools/gen-relaxed-schema.mjs -- do not hand-edit. " +
    "Every additionalProperties:false from the strict schema is removed, so this schema " +
    "accepts any unknown field on a known `t`, matching OVERVIEW.md section 2.7's wire-level " +
    "forward-compatibility rule. This is the schema that models real wire_valid, as opposed to " +
    "the deliberately-stricter control.schema.json, which is a test-time typo-catcher.";
  return relaxed;
}

function serialize(obj) {
  return JSON.stringify(obj, null, 2) + "\n";
}

function main() {
  const check = process.argv.includes("--check");
  const relaxed = generateRelaxedSchema();
  const serialized = serialize(relaxed);

  if (check) {
    if (!fs.existsSync(RELAXED_PATH)) {
      console.error(`${path.relative(SPEC_DIR, RELAXED_PATH)} does not exist -- run without --check to generate it`);
      process.exit(1);
    }
    const onDisk = fs.readFileSync(RELAXED_PATH, "utf8");
    if (onDisk !== serialized) {
      console.error(`${path.relative(SPEC_DIR, RELAXED_PATH)} is stale -- re-run 'node spec/tools/gen-relaxed-schema.mjs' and commit the result`);
      process.exit(1);
    }
    console.log("control.relaxed.schema.json is up to date");
    return;
  }

  fs.writeFileSync(RELAXED_PATH, serialized);
  console.log(`wrote ${path.relative(SPEC_DIR, RELAXED_PATH)}`);
}

if (import.meta.url === `file://${process.argv[1]}`) {
  main();
}
