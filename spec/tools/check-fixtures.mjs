#!/usr/bin/env node
// Dependency-free conformance check for spec/fixtures against spec/control.schema.json,
// spec/control.relaxed.schema.json, and the wire format described in docs/OVERVIEW.md
// section 2.2-2.9.
//
// No npm dependencies, no package.json, no network access. Node built-ins only.
//
// Usage: node spec/tools/check-fixtures.mjs

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { generateRelaxedSchema } from "./gen-relaxed-schema.mjs";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SPEC_DIR = path.resolve(__dirname, "..");

let pass = 0;
let fail = 0;
const failures = [];

function check(condition, label) {
  if (condition) {
    pass++;
  } else {
    fail++;
    failures.push(label);
  }
}

function fatal(label) {
  console.error(`FATAL: ${label}`);
  process.exit(1);
}

// ---------------------------------------------------------------------------
// A small, purpose-built JSON Schema (2020-12 subset) validator. It only
// implements the keywords control.schema.json / control.relaxed.schema.json
// actually use: type, const, required, properties, additionalProperties,
// items, pattern, minimum, maximum, minLength, maxLength, and $ref (to
// #/$defs/<name> only). This is intentionally not a general-purpose
// validator -- it exists so this repo carries no runtime dependency, per
// OVERVIEW.md section 6 decision 9 ("the schema is the conformance oracle,
// not a runtime dependency").
// ---------------------------------------------------------------------------

function typeOf(v) {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v; // "object" | "string" | "number" | "boolean"
}

function validateNode(schema, instance, defs, errors, pathStr) {
  if (schema.$ref) {
    const m = /^#\/\$defs\/(.+)$/.exec(schema.$ref);
    if (!m) throw new Error(`unsupported $ref: ${schema.$ref}`);
    return validateNode(defs[m[1]], instance, defs, errors, pathStr);
  }

  if (schema.const !== undefined) {
    if (instance !== schema.const) {
      errors.push(`${pathStr}: expected const ${JSON.stringify(schema.const)}, got ${JSON.stringify(instance)}`);
      return false;
    }
  }

  if (schema.type) {
    const t = typeOf(instance);
    const okType = schema.type === "integer" ? (t === "number" && Number.isInteger(instance)) : t === schema.type;
    if (!okType) {
      errors.push(`${pathStr}: expected type ${schema.type}, got ${t}`);
      return false;
    }
  }

  let ok = true;

  if (typeof instance === "string") {
    if (schema.minLength !== undefined && instance.length < schema.minLength) {
      errors.push(`${pathStr}: length ${instance.length} < minLength ${schema.minLength}`);
      ok = false;
    }
    if (schema.maxLength !== undefined && instance.length > schema.maxLength) {
      errors.push(`${pathStr}: length ${instance.length} > maxLength ${schema.maxLength}`);
      ok = false;
    }
    if (schema.pattern !== undefined && !new RegExp(schema.pattern).test(instance)) {
      errors.push(`${pathStr}: does not match pattern ${schema.pattern}`);
      ok = false;
    }
  }

  if (typeof instance === "number") {
    if (schema.minimum !== undefined && instance < schema.minimum) {
      errors.push(`${pathStr}: ${instance} < minimum ${schema.minimum}`);
      ok = false;
    }
    if (schema.maximum !== undefined && instance > schema.maximum) {
      errors.push(`${pathStr}: ${instance} > maximum ${schema.maximum}`);
      ok = false;
    }
  }

  if (Array.isArray(instance) && schema.items) {
    instance.forEach((item, i) => {
      if (!validateNode(schema.items, item, defs, errors, `${pathStr}[${i}]`)) ok = false;
    });
  }

  if (typeOf(instance) === "object" && !Array.isArray(instance)) {
    const props = schema.properties || {};
    for (const req of schema.required || []) {
      if (!(req in instance)) {
        errors.push(`${pathStr}: missing required property "${req}"`);
        ok = false;
      }
    }
    for (const [key, val] of Object.entries(instance)) {
      if (props[key]) {
        if (!validateNode(props[key], val, defs, errors, `${pathStr}.${key}`)) ok = false;
      } else if (schema.additionalProperties === false) {
        errors.push(`${pathStr}: additionalProperties -- unexpected property "${key}"`);
        ok = false;
      }
    }
  }

  return ok;
}

// oneOf per JSON Schema semantics: valid iff EXACTLY ONE $defs entry matches
// (finding 13 -- the previous implementation treated this as anyOf, which
// would silently accept an instance matching two branches at once).
function validateOneOf(schemaDoc, instance) {
  const defs = schemaDoc.$defs;
  const attempts = {};
  let matchCount = 0;
  let matchedName = null;
  for (const name of Object.keys(defs)) {
    const errors = [];
    const ok = validateNode(defs[name], instance, defs, errors, name);
    attempts[name] = { ok, errors };
    if (ok) {
      matchCount++;
      matchedName = name;
    }
  }
  return { valid: matchCount === 1, matchCount, matchedName, attempts };
}

// ---------------------------------------------------------------------------
// OVERVIEW.md section 2.8 error code table.
// ---------------------------------------------------------------------------

const ERROR_CODES = {
  NO_ERROR: 0x00,
  PROTOCOL_ERROR: 0x01,
  INTERNAL_ERROR: 0x02,
  FLOW_CONTROL_ERROR: 0x03,
  FRAME_SIZE_ERROR: 0x04,
  STREAM_CLOSED: 0x05,
  REFUSED_STREAM: 0x06,
  CANCEL: 0x07,
  STREAM_LIMIT: 0x08,
  ENHANCE_YOUR_CALM: 0x09,
  UNSUPPORTED: 0x0a,
  UNAUTHORIZED: 0x0b,
  GOING_AWAY: 0x0c,
  KEEPALIVE_TIMEOUT: 0x0d,
  APPLICATION_CLOSE: 0x0e,
};

function wsCloseForCode(name) {
  const code = ERROR_CODES[name];
  if (code === undefined) return undefined;
  return name === "NO_ERROR" ? 1000 : 4000 + code;
}

const KNOWN_CONTROL_TYPES = new Set(["hello", "welcome", "ping", "pong", "drain", "error", "app"]);

// ---------------------------------------------------------------------------
// Frame decoder -- structural (stateless) subset of OVERVIEW.md section 2.2-2.4.
// Deliberately does not implement the parts of section 2.5's state table that
// need connection state (highest_opened, per-stream state); those are
// exercised by spec/fixtures/sequences instead. This only re-derives what a
// single frame's bytes alone determine.
// ---------------------------------------------------------------------------

const FRAME_TYPE_NAMES = { 0x00: "OPEN", 0x01: "DATA", 0x02: "WINDOW", 0x03: "CLOSE", 0x04: "RESET" };
const MAX_MESSAGE_BYTES = 65544; // 8 header + 65536 payload
const STREAM_ZERO_MAX_PAYLOAD = 16384; // 16 KiB

const HEX_RE = /^([0-9a-f]{2})*$/;

function decodeFrame(hex) {
  // finding 11: validate hex is even-length [0-9a-f]* before ever touching Buffer.from,
  // which silently drops trailing garbage instead of failing.
  if (typeof hex !== "string" || !HEX_RE.test(hex)) {
    return { ok: false, error_code: "PROTOCOL_ERROR", connection_fatal: true, reason: `hex is not even-length lowercase [0-9a-f]*: ${JSON.stringify(hex)}` };
  }

  const buf = Buffer.from(hex, "hex");

  if (buf.length < 8) {
    return { ok: false, error_code: "PROTOCOL_ERROR", connection_fatal: true, reason: "message shorter than the 8-byte header" };
  }
  if (buf.length > MAX_MESSAGE_BYTES) {
    return { ok: false, error_code: "FRAME_SIZE_ERROR", connection_fatal: true, reason: `message ${buf.length}B exceeds ${MAX_MESSAGE_BYTES}B max` };
  }

  const type = buf.readUInt8(0);
  const flags = buf.readUInt8(1);
  const reserved = buf.readUInt16BE(2);
  const streamId = buf.readUInt32BE(4);
  const payload = buf.subarray(8);

  if (streamId & 0x80000000) {
    return { ok: false, error_code: "PROTOCOL_ERROR", connection_fatal: true, reason: "stream_id high bit set" };
  }

  const typeName = FRAME_TYPE_NAMES[type];
  if (typeName === undefined) {
    // Unknown type: ignore + count, not an error (OVERVIEW.md section 2.2).
    return { ok: true, decoded: { type: `UNKNOWN(0x${type.toString(16).padStart(2, "0")})`, flags, stream_id: streamId, payload_hex: payload.toString("hex") } };
  }

  const isControlStream = streamId === 0;
  if (isControlStream && typeName !== "DATA") {
    return { ok: false, error_code: "PROTOCOL_ERROR", connection_fatal: true, reason: `${typeName} is illegal on stream 0` };
  }
  if (isControlStream && typeName === "DATA" && payload.length > STREAM_ZERO_MAX_PAYLOAD) {
    return { ok: false, error_code: "ENHANCE_YOUR_CALM", connection_fatal: true, reason: `stream-0 payload ${payload.length}B exceeds ${STREAM_ZERO_MAX_PAYLOAD}B limit` };
  }

  if (typeName === "OPEN" && streamId % 2 === 0) {
    return { ok: false, error_code: "PROTOCOL_ERROR", connection_fatal: true, reason: "OPEN with an even (client-reserved) stream id" };
  }

  if (typeName === "WINDOW") {
    if (payload.length !== 4) {
      return { ok: false, error_code: "FRAME_SIZE_ERROR", connection_fatal: true, reason: `WINDOW payload is ${payload.length}B, must be exactly 4` };
    }
    const increment = payload.readUInt32BE(0);
    // finding 8: an increment with the high bit set would push the send window past
    // 2^31-1 -- OVERVIEW.md section 2.6 decision 1: connection-fatal FLOW_CONTROL_ERROR.
    if (increment & 0x80000000) {
      return { ok: false, error_code: "FLOW_CONTROL_ERROR", connection_fatal: true, reason: `WINDOW increment 0x${increment.toString(16)} has the high bit set` };
    }
    if (increment === 0) {
      return { ok: false, error_code: "PROTOCOL_ERROR", connection_fatal: false, reason: "WINDOW increment of 0" };
    }
    return { ok: true, decoded: { type: typeName, flags, stream_id: streamId, increment } };
  }

  if (typeName === "RESET") {
    if (payload.length < 4) {
      return { ok: false, error_code: "FRAME_SIZE_ERROR", connection_fatal: true, reason: `RESET payload is ${payload.length}B, must be >= 4` };
    }
    const code = payload.readUInt32BE(0);
    const message = payload.subarray(4).toString("utf8"); // Node replaces invalid sequences with U+FFFD
    return { ok: true, decoded: { type: typeName, flags, stream_id: streamId, code, message } };
  }

  return { ok: true, decoded: { type: typeName, flags, stream_id: streamId, payload_hex: payload.toString("hex"), payload_length: payload.length } };
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function listJsonFiles(dir) {
  return fs.readdirSync(dir).filter((f) => f.endsWith(".json")).map((f) => path.join(dir, f));
}

function readJson(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function requireDir(dir, label) {
  if (!fs.existsSync(dir) || !fs.statSync(dir).isDirectory()) {
    fatal(`missing required directory: ${label} (${path.relative(SPEC_DIR, dir)})`);
  }
}

// ---------------------------------------------------------------------------
// 0. Hard-fail on missing top-level directories/files (finding 12).
// ---------------------------------------------------------------------------

const controlDir = path.join(SPEC_DIR, "fixtures", "control");
const framesDir = path.join(SPEC_DIR, "fixtures", "frames");
const sequencesDir = path.join(SPEC_DIR, "fixtures", "sequences");

requireDir(controlDir, "fixtures/control");
requireDir(framesDir, "fixtures/frames");
requireDir(sequencesDir, "fixtures/sequences");

const strictSchemaPath = path.join(SPEC_DIR, "control.schema.json");
const relaxedSchemaPath = path.join(SPEC_DIR, "control.relaxed.schema.json");
if (!fs.existsSync(strictSchemaPath)) fatal("missing control.schema.json");
if (!fs.existsSync(relaxedSchemaPath)) fatal("missing control.relaxed.schema.json");

const strictSchemaDoc = readJson(strictSchemaPath);
const relaxedSchemaDoc = readJson(relaxedSchemaPath);

// --- finding 2: the checked-in relaxed schema must equal a fresh generation ---

const freshRelaxed = generateRelaxedSchema();
const freshRelaxedSerialized = JSON.stringify(freshRelaxed, null, 2) + "\n";
const onDiskRelaxedSerialized = fs.readFileSync(relaxedSchemaPath, "utf8");
check(
  freshRelaxedSerialized === onDiskRelaxedSerialized,
  "control.relaxed.schema.json is stale -- run 'node spec/tools/gen-relaxed-schema.mjs' and commit the result"
);

// ---------------------------------------------------------------------------
// 1. Control message fixtures: schema_valid vs control.schema.json (strict),
//    wire_valid vs control.relaxed.schema.json (finding 1, 2, 12, 13).
// ---------------------------------------------------------------------------

const controlTypes = fs.readdirSync(controlDir).filter((f) => fs.statSync(path.join(controlDir, f)).isDirectory());

// "envelope" is not one of the 7 message types (OVERVIEW.md section 2.7) -- it holds
// envelope-level violations (non-object, bare scalar, missing/non-string t, malformed
// JSON) that by construction never validate against any $defs entry and have no
// "valid" counterpart. It is exempt from the "needs >=1 schema_valid" rule below,
// documented in spec/README.md.
const ENVELOPE_TYPE = "envelope";

let controlSchemaValidCount = 0;
let controlSchemaInvalidCount = 0;
let controlWireValidCount = 0;

for (const type of controlTypes) {
  const perTypeSchemaValid = { count: 0 };
  const perTypeInvalid = { count: 0 };

  for (const bucket of ["valid", "invalid"]) {
    const dir = path.join(controlDir, type, bucket);
    if (!fs.existsSync(dir)) {
      if (type === ENVELOPE_TYPE && bucket === "valid") continue; // documented exception
      fatal(`missing required directory: fixtures/control/${type}/${bucket}`);
    }
    for (const file of listJsonFiles(dir)) {
      const fixture = readJson(file);
      const rel = path.relative(SPEC_DIR, file);

      if (!("schema_valid" in fixture) || !("wire_valid" in fixture)) {
        check(false, `${rel}: missing schema_valid/wire_valid (control fixtures no longer use a single "valid" field)`);
        continue;
      }

      if ("raw" in fixture) {
        // Malformed-JSON envelope case: there is no parsed object to run through
        // either schema. The fixture asserts the raw bytes don't even parse.
        let parsed = true;
        try {
          JSON.parse(fixture.raw);
        } catch {
          parsed = false;
        }
        check(parsed === false, `${rel}: "raw" was expected to fail JSON.parse but it parsed successfully`);
        check(fixture.schema_valid === false && fixture.wire_valid === false, `${rel}: malformed JSON must have schema_valid:false, wire_valid:false`);
        controlSchemaInvalidCount++;
        perTypeInvalid.count++;
        continue;
      }

      const strictResult = validateOneOf(strictSchemaDoc, fixture.message);
      const relaxedResult = validateOneOf(relaxedSchemaDoc, fixture.message);

      check(
        strictResult.valid === fixture.schema_valid,
        `${rel}: schema_valid=${fixture.schema_valid} but strict schema says ${strictResult.valid} (matched ${strictResult.matchCount} defs)`
      );
      check(
        relaxedResult.valid === fixture.wire_valid,
        `${rel}: wire_valid=${fixture.wire_valid} but relaxed schema says ${relaxedResult.valid} (matched ${relaxedResult.matchCount} defs)`
      );

      if (fixture.schema_valid) {
        controlSchemaValidCount++;
        perTypeSchemaValid.count++;
      } else {
        controlSchemaInvalidCount++;
        perTypeInvalid.count++;
      }
      if (fixture.wire_valid) controlWireValidCount++;
    }
  }

  if (type !== ENVELOPE_TYPE) {
    check(perTypeSchemaValid.count >= 1, `control/${type}: needs >=1 schema_valid fixture, has ${perTypeSchemaValid.count}`);
  }
  check(perTypeInvalid.count >= 3, `control/${type}: needs >=3 invalid (schema_valid:false) fixtures, has ${perTypeInvalid.count}`);
}

// ---------------------------------------------------------------------------
// 2. Frame fixtures vs the hand-decoded wire format.
// ---------------------------------------------------------------------------

let frameValidCount = 0;
let frameInvalidCount = 0;

function decodedMatches(expected, actual) {
  for (const [key, val] of Object.entries(expected)) {
    if (key === "reserved_ignored" || key === "message_sanitized") continue; // informational-only keys
    if (JSON.stringify(actual[key]) !== JSON.stringify(val)) return `field "${key}": expected ${JSON.stringify(val)}, got ${JSON.stringify(actual[key])}`;
  }
  return null;
}

for (const file of listJsonFiles(framesDir)) {
  const fixture = readJson(file);
  const rel = path.relative(SPEC_DIR, file);
  const result = decodeFrame(fixture.hex);
  if (fixture.valid) {
    frameValidCount++;
    if (!result.ok) {
      check(false, `${rel}: expected a valid decode, got error ${result.error_code} (${result.reason})`);
    } else {
      const mismatch = decodedMatches(fixture.decoded, result.decoded);
      check(mismatch === null, `${rel}: decoded mismatch -- ${mismatch}`);
    }
  } else {
    frameInvalidCount++;
    if (result.ok) {
      check(false, `${rel}: expected decode to fail, but it succeeded`);
    } else {
      const codeOk = result.error_code === fixture.expect.error_code;
      const fatalOk = result.connection_fatal === fixture.expect.connection_fatal;
      check(codeOk && fatalOk, `${rel}: expected ${JSON.stringify(fixture.expect)}, got {error_code: ${result.error_code}, connection_fatal: ${result.connection_fatal}} (${result.reason})`);
    }
  }
}

// ---------------------------------------------------------------------------
// 3. Sequence fixtures (finding 3): role, step shape, control payloads against
//    the relaxed schema, close_code == 4000+code (1000 for NO_ERROR), and
//    error_code/stream_reset_code names are real table entries.
// ---------------------------------------------------------------------------

let sequenceCount = 0;

for (const file of listJsonFiles(sequencesDir)) {
  const fixture = readJson(file);
  const rel = path.relative(SPEC_DIR, file);
  sequenceCount++;

  check(fixture.role === "server" || fixture.role === "client", `${rel}: role must be "server" or "client", got ${JSON.stringify(fixture.role)}`);

  if (!Array.isArray(fixture.steps)) {
    check(false, `${rel}: "steps" must be an array`);
    continue;
  }

  let lastErrorCode = null;
  let lastResetCode = null;

  fixture.steps.forEach((step, i) => {
    const stepPath = `${rel}#steps[${i}]`;
    const presentKeys = ["recv", "send", "wait_ms", "expect"].filter((k) => k in step);
    check(presentKeys.length === 1, `${stepPath}: must have exactly one of recv|send|wait_ms|expect, has [${presentKeys.join(", ")}]`);

    // finding: a step carrying a numeric error.code or RESET code must agree with
    // the named error_code/stream_reset_code a later expect step asserts on.
    const msg = step.recv || step.send;
    if (msg && msg.t === "error" && typeof msg.code === "number") lastErrorCode = msg.code;
    if (msg && msg.type === "RESET" && typeof msg.code === "number") lastResetCode = msg.code;
    if (step.expect && step.expect.error_code && lastErrorCode !== null) {
      check(ERROR_CODES[step.expect.error_code] === lastErrorCode, `${stepPath}.expect.error_code: "${step.expect.error_code}" (${ERROR_CODES[step.expect.error_code]}) does not match the numeric error.code ${lastErrorCode} seen earlier in this sequence`);
    }
    if (step.expect && step.expect.stream_reset_code && lastResetCode !== null) {
      check(ERROR_CODES[step.expect.stream_reset_code] === lastResetCode, `${stepPath}.expect.stream_reset_code: "${step.expect.stream_reset_code}" (${ERROR_CODES[step.expect.stream_reset_code]}) does not match the numeric RESET code ${lastResetCode} seen earlier in this sequence`);
    }

    for (const kind of ["recv", "send"]) {
      const payload = step[kind];
      if (payload && typeof payload === "object" && typeof payload.t === "string") {
        // Control-channel message. Skip validation for a deliberately-unknown `t`
        // (e.g. sequences/unknown_control_message_type.json) -- that IS the case
        // under test and by definition matches no $defs entry. Also skip when the
        // step is explicitly marked schema_exempt: true -- a known t whose payload
        // is deliberately schema-invalid on purpose (e.g. hello.v mismatch in
        // sequences/hello_version_mismatch.json, which the protocol detects as
        // UNSUPPORTED, not merely "failed the strict/relaxed schema").
        if (KNOWN_CONTROL_TYPES.has(payload.t) && !step.schema_exempt) {
          const result = validateOneOf(relaxedSchemaDoc, payload);
          check(result.valid, `${stepPath}.${kind}: control payload with t="${payload.t}" does not validate against control.relaxed.schema.json (matched ${result.matchCount} defs)`);
        }
      }
    }

    if (step.expect && typeof step.expect === "object") {
      const { error_code, stream_reset_code, close_code } = step.expect;
      for (const [field, value] of [["error_code", error_code], ["stream_reset_code", stream_reset_code]]) {
        if (value !== undefined && value !== null) {
          check(Object.prototype.hasOwnProperty.call(ERROR_CODES, value), `${stepPath}.expect.${field}: "${value}" is not a name in OVERVIEW.md section 2.8's error table`);
        }
      }
      if (close_code !== undefined && close_code !== null && error_code) {
        const expectedClose = wsCloseForCode(error_code);
        if (expectedClose !== undefined) {
          check(close_code === expectedClose, `${stepPath}.expect: close_code ${close_code} != ${expectedClose} for error_code ${error_code} (ws_close = 4000 + error_code, or 1000 for NO_ERROR)`);
        }
      }
    }
  });
}

// ---------------------------------------------------------------------------
// 4. Checked-in total-count floor (finding 12).
// ---------------------------------------------------------------------------

const countsPath = path.join(SPEC_DIR, "fixtures", "COUNTS.json");
if (!fs.existsSync(countsPath)) fatal("missing spec/fixtures/COUNTS.json (checked-in total-count floor)");
const counts = readJson(countsPath);

function countJsonFilesRecursive(dir) {
  let n = 0;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "COUNTS.json") continue; // metadata, not a fixture
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) n += countJsonFilesRecursive(full);
    else if (entry.name.endsWith(".json")) n += 1;
  }
  return n;
}

const actualTotal = countJsonFilesRecursive(path.join(SPEC_DIR, "fixtures"));
check(actualTotal >= counts.minimum_total_fixtures, `total fixture count dropped: ${actualTotal} < checked-in floor ${counts.minimum_total_fixtures} (spec/fixtures/COUNTS.json)`);

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

console.log(`control fixtures: ${controlSchemaValidCount} schema_valid, ${controlSchemaInvalidCount} schema_invalid (${controlWireValidCount} wire_valid), across ${controlTypes.length} types`);
console.log(`frame fixtures:   ${frameValidCount} valid, ${frameInvalidCount} invalid`);
console.log(`sequence fixtures: ${sequenceCount}`);
console.log(`total fixture files: ${actualTotal} (floor ${counts.minimum_total_fixtures})`);
console.log(`\n${pass}/${pass + fail} checks passed`);

if (failures.length) {
  console.log("\nFAILURES:");
  for (const f of failures) console.log(`  - ${f}`);
  process.exit(1);
}
