#!/usr/bin/env node
// Dependency-free conformance check for spec/fixtures against spec/control.schema.json,
// spec/control.relaxed.schema.json, and the wire format described in docs/OVERVIEW.md
// section 2.2-2.9, plus spec/fixtures/mcpwarp against spec/mcpwarp.schema.json and the
// rules of docs/MCPWARP.md.
//
// No npm dependencies, no package.json, no network access. Node built-ins only.
//
// Usage: node spec/tools/check-fixtures.mjs

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { generateRelaxedSchema, stripAdditionalPropertiesFalse } from "./gen-relaxed-schema.mjs";

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
    // JSON Schema counts a string's length in Unicode code points (WIRE.md §2.7 String limits),
    // not UTF-16 units: a non-BMP character is 1, where String.length says 2.
    const codePoints = [...instance].length;
    if (schema.minLength !== undefined && codePoints < schema.minLength) {
      errors.push(`${pathStr}: length ${codePoints} < minLength ${schema.minLength}`);
      ok = false;
    }
    if (schema.maxLength !== undefined && codePoints > schema.maxLength) {
      errors.push(`${pathStr}: length ${codePoints} > maxLength ${schema.maxLength}`);
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

      // A file name like message_1024_code_points_… states the length of that field in code points.
      const named = /^([a-z]+(?:_[a-z]+)*?)_(\d+)_code_points/.exec(path.basename(file, ".json"));
      if (named) {
        const value = isPlainObject(fixture.message) ? fixture.message[named[1]] : undefined;
        const actual = typeof value === "string" ? [...value].length : undefined;
        check(actual === Number(named[2]), `${rel}: file name says ${named[1]} has ${named[2]} code points, it has ${actual ?? "no string"}`);
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
// 4. mcpwarp fixtures (docs/MCPWARP.md). Data every mcpwarp SDK runs in its own
//    CI; this repo checks their shape and internal consistency. envelope/ and
//    bridging/ are required; tunnel/ and status-text/ are checked when present.
// ---------------------------------------------------------------------------

const mcpwarpDir = path.join(SPEC_DIR, "fixtures", "mcpwarp");
const mcpwarpSchemaPath = path.join(SPEC_DIR, "mcpwarp.schema.json");
requireDir(mcpwarpDir, "fixtures/mcpwarp");
if (!fs.existsSync(mcpwarpSchemaPath)) fatal("missing mcpwarp.schema.json");

const mcpwarpSchemaDoc = readJson(mcpwarpSchemaPath);
// The receiver of an agent -> tunnel op ignores unknown fields, like the wire_valid side of control fixtures.
const mcpwarpRelaxedDoc = stripAdditionalPropertiesFalse(mcpwarpSchemaDoc);
const mcpwarpNotes = [];

function isPlainObject(v) {
  return v !== null && typeof v === "object" && !Array.isArray(v);
}

function readJsonChecked(file) {
  try {
    return readJson(file);
  } catch (e) {
    check(false, `${path.relative(SPEC_DIR, file)}: not valid JSON (${e.message})`);
    return undefined;
  }
}

// --- 4.1 envelope ops ------------------------------------------------------

const MCPWARP_OUTBOUND_OPS = ["register", "unregister"];
const MCPWARP_INBOUND_OPS = ["registered", "unregistered", "disable", "enable", "conflict", "error"];
const MCPWARP_OPS = [...MCPWARP_OUTBOUND_OPS, ...MCPWARP_INBOUND_OPS];

// mcpwarp.schema.json states `enum`, `minItems` and `maxItems`, which validateNode ignores
// (section 0's keyword subset). They are applied here, to the matched $defs
// entry only, so the control schemas' behaviour is untouched.
function mcpwarpKeywordErrors(schema, instance, defs, pathStr, errors) {
  if (schema.$ref) {
    const m = /^#\/\$defs\/(.+)$/.exec(schema.$ref);
    if (!m) throw new Error(`unsupported $ref: ${schema.$ref}`);
    return mcpwarpKeywordErrors(defs[m[1]], instance, defs, pathStr, errors);
  }
  if (Array.isArray(schema.enum) && !schema.enum.includes(instance)) {
    errors.push(`${pathStr}: ${JSON.stringify(instance)} is not one of the schema's enum values`);
  }
  if (Array.isArray(instance)) {
    if (schema.minItems !== undefined && instance.length < schema.minItems) {
      errors.push(`${pathStr}: ${instance.length} items < minItems ${schema.minItems}`);
    }
    if (schema.maxItems !== undefined && instance.length > schema.maxItems) {
      errors.push(`${pathStr}: ${instance.length} items > maxItems ${schema.maxItems}`);
    }
    if (schema.items) instance.forEach((item, i) => mcpwarpKeywordErrors(schema.items, item, defs, `${pathStr}[${i}]`, errors));
  } else if (isPlainObject(instance) && schema.properties) {
    for (const [key, val] of Object.entries(instance)) {
      if (schema.properties[key]) mcpwarpKeywordErrors(schema.properties[key], val, defs, `${pathStr}.${key}`, errors);
    }
  }
  return errors;
}

function validateMcpwarp(schemaDoc, message, op) {
  const result = validateOneOf(schemaDoc, message);
  if (!result.valid) {
    const own = result.attempts[op];
    return { valid: false, matchedName: null, errors: own ? own.errors : [`matched ${result.matchCount} $defs`] };
  }
  const errors = mcpwarpKeywordErrors(schemaDoc.$defs[result.matchedName], message, schemaDoc.$defs, result.matchedName, []);
  return { valid: errors.length === 0, matchedName: result.matchedName, errors };
}

// Reference for the docs/MCPWARP.md §2 decode rules a receiving SDK applies to a
// tunnel -> agent op: v other than 1 or an unknown op is ignored; unknown fields are
// ignored; absent or null lists are []; created may be absent, null or boolean;
// registered.services[].url is a required non-empty string; any other type
// mismatch, including null in an optional string, is MALFORMED_ENVELOPE.
function decodeMcpwarpInbound(env) {
  if (!isPlainObject(env)) return { ok: false, code: "NOT_OURS", reason: "body.mcpwarp is not an object" };
  if (env.v !== 1) return { ok: false, code: "UNSUPPORTED_VERSION", reason: `v ${JSON.stringify(env.v)} is not 1, decoded as unknown and ignored` };
  if (!MCPWARP_INBOUND_OPS.includes(env.op)) return { ok: false, code: "UNKNOWN_OP", reason: `op ${JSON.stringify(env.op)} is unknown and ignored` };

  const problems = [];
  const str = (obj, key, where, required) => {
    if (obj[key] === undefined && !required) return;
    if (typeof obj[key] !== "string") problems.push(`${where}.${key} must be a string`);
  };
  const list = (obj, key, where) => {
    const v = obj[key];
    if (v === undefined || v === null) return [];
    if (!Array.isArray(v) || !v.every(isPlainObject)) {
      problems.push(`${where}.${key} must be an array of objects`);
      return [];
    }
    return v;
  };

  switch (env.op) {
    case "registered":
      for (const s of list(env, "services", "registered")) {
        str(s, "name", "registered.services[]", true);
        str(s, "id", "registered.services[]", true);
        if (typeof s.url !== "string" || s.url === "") problems.push("registered.services[].url must be a non-empty string");
        if (s.created !== undefined && s.created !== null && typeof s.created !== "boolean") {
          problems.push("registered.services[].created must be a boolean");
        }
      }
      for (const e of list(env, "errors", "registered")) {
        str(e, "name", "registered.errors[]", true);
        str(e, "code", "registered.errors[]", true);
        str(e, "message", "registered.errors[]", false);
        str(e, "id", "registered.errors[]", false);
      }
      break;
    case "unregistered":
      for (const s of list(env, "services", "unregistered")) {
        str(s, "name", "unregistered.services[]", true);
        str(s, "id", "unregistered.services[]", false);
      }
      break;
    case "disable":
      str(env, "id", "disable", true);
      str(env, "reason", "disable", false);
      break;
    case "enable":
      str(env, "id", "enable", true);
      str(env, "name", "enable", true);
      break;
    case "conflict":
      str(env, "id", "conflict", true);
      str(env, "name", "conflict", true);
      str(env, "reason", "conflict", false);
      break;
    case "error":
      str(env, "code", "error", true);
      str(env, "message", "error", false);
      break;
  }
  return problems.length ? { ok: false, code: "MALFORMED_ENVELOPE", reason: problems[0] } : { ok: true };
}

// expect.error_code hints: a code a receiver reports, or SCHEMA_* for a finding
// only the schema makes. An agent -> tunnel op is answered by the edge, so its
// hint is an edge-side code (register or app error) or SCHEMA_*, never an
// agent-side decode outcome (MALFORMED_ENVELOPE, NOT_OURS).
const MCPWARP_EDGE_CODES = new Set([
  ...mcpwarpSchemaDoc.$defs.registered.properties.errors.items.properties.code.enum,
  ...mcpwarpSchemaDoc.$defs.error.properties.code.enum,
]);
const MCPWARP_HINT_CODES = new Set(["MALFORMED_ENVELOPE", "NOT_OURS", ...MCPWARP_EDGE_CODES]);
const MCPWARP_SCHEMA_HINT_RE = /^SCHEMA_[A-Z_]+$/;

const envelopeDir = path.join(mcpwarpDir, "envelope");
requireDir(envelopeDir, "fixtures/mcpwarp/envelope");

const envelopeCoverage = Object.fromEntries(MCPWARP_OPS.map((op) => [op, { valid: 0, invalid: 0 }]));
let mcpwarpEnvelopeValid = 0;
let mcpwarpEnvelopeInvalid = 0;
let mcpwarpEnvelopeWireValid = 0;

for (const bucket of ["valid", "invalid"]) {
  const dir = path.join(envelopeDir, bucket);
  requireDir(dir, `fixtures/mcpwarp/envelope/${bucket}`);
  for (const file of listJsonFiles(dir)) {
    const rel = path.relative(SPEC_DIR, file);
    const fixture = readJsonChecked(file);
    if (fixture === undefined) continue;

    if (!MCPWARP_OPS.includes(fixture.op)) {
      check(false, `${rel}: "op" must be one of ${MCPWARP_OPS.join(", ")}, got ${JSON.stringify(fixture.op)}`);
      continue;
    }
    const base = path.basename(file);
    check(base === `${fixture.op}.json` || base.startsWith(`${fixture.op}_`), `${rel}: file name must start with its op "${fixture.op}"`);
    if (typeof fixture.schema_valid !== "boolean" || typeof fixture.wire_valid !== "boolean" || !("message" in fixture)) {
      check(false, `${rel}: needs "message" and boolean "schema_valid" / "wire_valid"`);
      continue;
    }
    check(fixture.schema_valid === (bucket === "valid"), `${rel}: lives in ${bucket}/ but schema_valid is ${fixture.schema_valid}`);

    const strict = validateMcpwarp(mcpwarpSchemaDoc, fixture.message, fixture.op);
    check(
      strict.valid === fixture.schema_valid,
      `${rel}: schema_valid=${fixture.schema_valid} but mcpwarp.schema.json (with enum/minItems/maxItems) says ${strict.valid}${strict.errors.length ? ` (${strict.errors.join("; ")})` : ""}`
    );
    if (strict.valid) check(strict.matchedName === fixture.op, `${rel}: message validates as "${strict.matchedName}", fixture says op "${fixture.op}"`);

    let wire;
    if (MCPWARP_OUTBOUND_OPS.includes(fixture.op)) {
      const r = validateMcpwarp(mcpwarpRelaxedDoc, fixture.message, fixture.op);
      wire = { valid: r.valid, code: null, reason: r.errors.join("; ") };
    } else {
      const d = decodeMcpwarpInbound(fixture.message);
      wire = { valid: d.ok, code: d.ok ? null : d.code, reason: d.reason || "" };
    }
    check(
      wire.valid === fixture.wire_valid,
      `${rel}: wire_valid=${fixture.wire_valid} but ${MCPWARP_OUTBOUND_OPS.includes(fixture.op) ? "the relaxed mcpwarp schema says" : "the MCPWARP.md §2 decode rules say"} ${wire.valid}${wire.reason ? ` (${wire.reason})` : ""}`
    );

    if (fixture.schema_valid) {
      check(!("expect" in fixture), `${rel}: "expect" is only for schema_valid:false fixtures`);
      mcpwarpEnvelopeValid++;
      envelopeCoverage[fixture.op].valid++;
    } else {
      const code = isPlainObject(fixture.expect) ? fixture.expect.error_code : undefined;
      const isSchemaHint = typeof code === "string" && MCPWARP_SCHEMA_HINT_RE.test(code);
      if (fixture.wire_valid) {
        check(isSchemaHint, `${rel}: schema_valid:false with wire_valid:true needs a SCHEMA_* expect.error_code, got ${JSON.stringify(code)}`);
      } else if (MCPWARP_OUTBOUND_OPS.includes(fixture.op)) {
        check(isSchemaHint || MCPWARP_EDGE_CODES.has(code), `${rel}: expect.error_code of an agent -> tunnel op must be a SCHEMA_* hint or an edge-side register/app error code, got ${JSON.stringify(code)}`);
      } else {
        check(MCPWARP_HINT_CODES.has(code), `${rel}: expect.error_code must be MALFORMED_ENVELOPE or a register/app error code, got ${JSON.stringify(code)}`);
        if (wire.code !== null) {
          check(code === wire.code, `${rel}: expect.error_code "${code}" but the MCPWARP.md §2 decode rules give ${wire.code} (${wire.reason})`);
        }
      }
      const contains = isPlainObject(fixture.expect) ? fixture.expect.message_contains : undefined;
      if (contains !== undefined) {
        // A decode failure names its first defect in MCPWARP.md §2.8 rule 10's order, so only that one counts.
        const findings = (wire.code === "MALFORMED_ENVELOPE" ? [wire.reason] : [...strict.errors, wire.reason]).join("\n");
        check(
          typeof contains === "string" && contains.length > 0 && findings.includes(contains),
          `${rel}: expect.message_contains ${JSON.stringify(contains)} does not occur in the checker's findings (${findings.replace(/\n/g, "; ")})`
        );
      }
      mcpwarpEnvelopeInvalid++;
      envelopeCoverage[fixture.op].invalid++;
    }
    if (fixture.wire_valid) mcpwarpEnvelopeWireValid++;
  }
}

for (const [op, cov] of Object.entries(envelopeCoverage)) {
  check(cov.valid >= 1, `mcpwarp/envelope op ${op}: needs >=1 schema_valid fixture, has ${cov.valid}`);
  check(cov.invalid >= 3, `mcpwarp/envelope op ${op}: needs >=3 schema_valid:false fixtures, has ${cov.invalid}`);
}

// --- 4.2 request bridging --------------------------------------------------

const BRIDGING_STATES = ["online", "disabled", "closing"];
const BRIDGING_MODES = ["handler", "target"];
const BRIDGING_BEHAVIOURS = ["echo", "throw", "hang", "status"];
const BRIDGING_LIMIT_KEYS = ["maxRequestBodyBytes", "targetTimeoutMs"];
const BRIDGING_MAX_HEAD_BYTES = 64 * 1024;
const BRIDGING_DEFAULT_BODY_CAP = 4 * 1024 * 1024;
// Fixture names without a trailing _<status>.
const BRIDGING_NAMED_STATUS = {
  mcp_post_ok: 200,
  notification_passthrough: 200,
  target_utf8_query: 200,
  target_lone_percent: 200,
  target_fragment_dropped: 200,
  target_fragment_dropped_absolute_form: 200,
  target_allowed_set: 200,
  head_mcp_400_headonly: 400,
  multi_service_route_by_header: 200,
  multi_service_route_by_host: 200,
};
const REASON_PHRASES = {
  200: "OK",
  202: "Accepted",
  400: "Bad Request",
  404: "Not Found",
  405: "Method Not Allowed",
  413: "Content Too Large",
  431: "Request Header Fields Too Large",
  500: "Internal Server Error",
  502: "Bad Gateway",
  503: "Service Unavailable",
  504: "Gateway Timeout",
};
// The {"error": ...} bodies the SDK itself writes (MCPWARP.md §4 status table).
const SDK_ERROR_MESSAGES = {
  400: ["unsupported request target form", "request cannot be represented as a web Request"],
  404: ["not_found"],
  413: ["request body too large"],
  431: ["request head too large"],
  500: ["handler error"],
  503: ["service disabled", "service closing", "service unavailable"],
  504: ["local server timed out"],
};
const NO_APP_STATUSES = new Set([400, 404, 413, 431, 503]);
const HOP_BY_HOP = new Set(["connection", "keep-alive", "proxy-connection", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade"]);
const TOKEN_RE = /^[!#$%&'*+.^_`|~0-9A-Za-z-]+$/;

function decodeChunked(buf) {
  const chunks = [];
  let pos = 0;
  for (;;) {
    const eol = buf.indexOf("\r\n", pos);
    if (eol < 0) return { error: "chunk-size line without CRLF" };
    const sizeText = buf.subarray(pos, eol).toString("latin1").split(";")[0];
    if (!/^[0-9a-fA-F]+$/.test(sizeText)) return { error: `malformed chunk size ${JSON.stringify(sizeText)}` };
    const size = parseInt(sizeText, 16);
    pos = eol + 2;
    if (size === 0) {
      if (buf.subarray(pos).toString("latin1") !== "\r\n") return { error: "last chunk not followed by exactly CRLF" };
      return { body: Buffer.concat(chunks) };
    }
    if (pos + size + 2 > buf.length) return { error: "chunk shorter than its size" };
    chunks.push(buf.subarray(pos, pos + size));
    pos += size;
    if (buf.subarray(pos, pos + 2).toString("latin1") !== "\r\n") return { error: "missing CRLF after chunk data" };
    pos += 2;
  }
}

// Chunk data bytes present in a prefix of a chunked body; a chunk cut short counts
// the bytes that are there. Stops at the last chunk or an incomplete size line.
function chunkedBytesSoFar(buf) {
  let pos = 0;
  let total = 0;
  for (;;) {
    const eol = buf.indexOf("\r\n", pos);
    if (eol < 0) return total;
    const size = parseInt(buf.subarray(pos, eol).toString("latin1").split(";")[0], 16);
    if (!(size > 0)) return total;
    pos = eol + 2;
    const have = Math.min(size, buf.length - pos);
    total += have;
    if (have < size) return total;
    pos += size + 2;
  }
}

// Parses one HTTP/1.1 message as written on the stream. Header names are
// lower-cased; framing follows Content-Length or chunked, else the body is empty.
// headOnly: nothing may follow the head and its framing headers describe no
// bytes here (a reply to HEAD, or the head before an outReset).
function parseHttpMessage(text, kind, { headOnly = false } = {}) {
  const buf = Buffer.from(text, "utf8");
  const end = buf.indexOf("\r\n\r\n");
  if (end < 0) return { error: "no blank line (CRLF CRLF) ends the head" };
  const head = buf.subarray(0, end).toString("utf8");
  if (/(^|[^\r])\n/.test(head) || /\r(?!\n)/.test(head)) return { error: "head lines must end in CRLF" };
  const lines = head.split("\r\n");
  const start = lines[0];
  const msg = { headBytes: end + 4, headers: [], start };
  if (kind === "request") {
    const m = /^(\S+) (\S+) HTTP\/1\.1$/.exec(start);
    if (!m || !TOKEN_RE.test(m[1])) return { error: `not a request line: ${JSON.stringify(start.slice(0, 80))}` };
    msg.method = m[1];
    msg.target = m[2];
  } else {
    const m = /^HTTP\/1\.1 (\d{3}) (.+)$/.exec(start);
    if (!m) return { error: `not a status line: ${JSON.stringify(start.slice(0, 80))}` };
    msg.status = Number(m[1]);
    msg.reason = m[2];
  }
  for (const line of lines.slice(1)) {
    const idx = line.indexOf(":");
    if (idx <= 0 || !TOKEN_RE.test(line.slice(0, idx))) return { error: `malformed header line ${JSON.stringify(line.slice(0, 80))}` };
    msg.headers.push([line.slice(0, idx).toLowerCase(), line.slice(idx + 1).trim()]);
  }
  msg.get = (name) => msg.headers.filter(([n]) => n === name).map(([, v]) => v);
  const rest = buf.subarray(end + 4);
  const cl = msg.get("content-length");
  const te = msg.get("transfer-encoding");
  if (cl.length && te.length) return { error: "both Content-Length and Transfer-Encoding" };
  if (headOnly) {
    if (cl.length && (cl.length !== 1 || !/^\d+$/.test(cl[0]))) return { error: `malformed Content-Length ${JSON.stringify(cl)}` };
    if (te.length && (te.length !== 1 || te[0].toLowerCase() !== "chunked")) return { error: `unsupported Transfer-Encoding ${JSON.stringify(te)}` };
    if (rest.length !== 0) return { error: `${rest.length} bytes after a head that must stand alone` };
    if (cl.length) msg.contentLength = Number(cl[0]);
    msg.chunked = te.length > 0;
    msg.body = rest;
    return msg;
  }
  if (cl.length) {
    if (cl.length !== 1 || !/^\d+$/.test(cl[0])) return { error: `malformed Content-Length ${JSON.stringify(cl)}` };
    msg.contentLength = Number(cl[0]);
    if (rest.length !== msg.contentLength) return { error: `Content-Length ${msg.contentLength} but the body is ${rest.length} bytes` };
    msg.body = rest;
  } else if (te.length) {
    if (te.length !== 1 || te[0].toLowerCase() !== "chunked") return { error: `unsupported Transfer-Encoding ${JSON.stringify(te)}` };
    const d = decodeChunked(rest);
    if (d.error) return { error: d.error };
    msg.body = d.body;
    msg.chunked = true;
  } else {
    if (rest.length !== 0) return { error: `${rest.length} body bytes without Content-Length or Transfer-Encoding` };
    msg.body = rest;
  }
  return msg;
}

function requestPath(target) {
  const noAuthority = target.replace(/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\/[^/?#]*/, "");
  return noAuthority.split(/[?#]/)[0];
}

// MCPWARP.md §4.2 Target row: origin-form or absolute-form http(s)://, anything else 400.
const ABSOLUTE_FORM_RE = /^https?:\/\/[^/?#]*/i;
function supportedTargetForm(target) {
  return target.startsWith("/") || ABSOLUTE_FORM_RE.test(target);
}

// MCPWARP.md §4.3 allowed set: unreserved, sub-delims, ":" "@" "/" "?", "[" "]", and "%" (never touched).
const TARGET_KEEP_RE = /^[A-Za-z0-9\-._~!$&'()*+,;=:@/?[\]%]$/;

// The target the app receives (MCPWARP.md §4.3): the fragment cut at the first "#", absolute-form cut
// to path + query as written, then every received byte outside the allowed set %XX-escaped, once,
// in uppercase hex. Existing escapes and a lone "%" pass through unchanged.
function forwardedTarget(target) {
  let t = target.split("#")[0];
  const authority = ABSOLUTE_FORM_RE.exec(t);
  if (authority) {
    t = t.slice(authority[0].length);
    if (!t.startsWith("/")) t = `/${t}`;
  }
  let out = "";
  for (const byte of Buffer.from(t, "utf8")) {
    const ch = String.fromCharCode(byte);
    out += byte < 0x80 && TARGET_KEEP_RE.test(ch) ? ch : `%${byte.toString(16).toUpperCase().padStart(2, "0")}`;
  }
  return out;
}

// A forwarded header value (Host included) holding NUL, CR or LF, or a fetch-forbidden method (CONNECT,
// TRACE, TRACK, case-insensitive), is one no web Request / HTTP client can carry: 400 "request cannot be
// represented as a web Request" (MCPWARP.md §4.5), in both modes.
const FORBIDDEN_METHOD_RE = /^(CONNECT|TRACE|TRACK)$/i;
function representable(req) {
  if (FORBIDDEN_METHOD_RE.test(req.method)) return false;
  const values = [...req.get("host"), ...Object.values(forwardableHeaders(req))];
  return values.every((v) => !/[\0\r\n]/.test(v));
}

function forwardableHeaders(req) {
  const connectionListed = new Set(req.get("connection").flatMap((v) => v.split(",").map((s) => s.trim().toLowerCase())));
  const out = {};
  for (const [name, value] of req.headers) {
    if (HOP_BY_HOP.has(name) || connectionListed.has(name) || name === "expect" || name === "content-length") continue;
    out[name] = name in out ? `${out[name]}, ${value}` : value;
  }
  return out;
}

// MCPWARP.md §4.5 error reply, byte-exact; to HEAD the same head and no body.
function sdkErrorReply(status, message, method) {
  const body = JSON.stringify({ error: message });
  const head = `HTTP/1.1 ${status} ${REASON_PHRASES[status] ?? "Unknown"}\r\nContent-Type: application/json\r\nContent-Length: ${Buffer.byteLength(body)}\r\n\r\n`;
  return method === "HEAD" ? head : head + body;
}

function sameJson(a, b) {
  const sort = (o) => Object.fromEntries(Object.entries(o).sort(([x], [y]) => (x < y ? -1 : x > y ? 1 : 0)));
  return JSON.stringify(sort(a)) === JSON.stringify(sort(b));
}

// MCPWARP.md §4.8 multi-service selection. Step 1, only with the SDK option trustServerIdHeader (default
// off; a fixture that needs it carries runnerRequirement "trust-server-id-header"): an X-Mcpwarp-Server-Id
// header (name matched case-insensitively, parseHttpMessage lower-cases names; the first if repeated)
// selects the service with that registered id, or none, with no fallback. Step 2: else the Host's first
// label, port dropped and lowercased, split at its LAST "-" into service name and username
// (^[a-z0-9]{3,20}$). Returns the selected services[] entry or null.
const BRIDGING_USERNAME_RE = /^[a-z0-9]{3,20}$/;
// §6A.4 states a bridging fixture can hold; taken_over is §4.8 row 3c, a RESET.
const BRIDGING_SERVICE_STATES = ["active", "pending", "disabled", "rejected", "unregistered"];
// A runner without the capability skips the fixture with a note (spec/README.md bridging/).
const BRIDGING_RUNNER_REQUIREMENTS = ["trust-server-id-header"];
function bridgingRoute(fx, req) {
  const ids = req.get("x-mcpwarp-server-id");
  if (fx.runnerRequirement === "trust-server-id-header" && ids.length > 0) return fx.services.find((s) => s.id === ids[0]) ?? null;
  const label = req.get("host")[0].toLowerCase().replace(/:\d+$/, "").split(".")[0];
  const cut = label.lastIndexOf("-");
  if (cut <= 0) return null;
  const name = label.slice(0, cut);
  if (!BRIDGING_USERNAME_RE.test(label.slice(cut + 1)) || !/^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(name)) return null;
  return fx.services.find((s) => s.name === name) ?? null;
}

// The status the MCPWARP.md §4.4 rules give, in precedence order: 431 (head size, the blank line
// counted) > 400 (target form) > 404 (path) > 503 (disabled/closing) > 413 (body cap) > 400 (not
// representable) > the app. With fx.services, §4.8 puts selection after the path check: no service
// selected -> 503 service unavailable (3b); a dashboard-disabled one -> 503 service disabled (3d);
// a locally disabled, rejected, unregistered or never-active pending one -> 503 service unavailable
// (3e); an active one is served. `routed` is the selected service's name, or null.
function bridgingExpectedStatus(fx, req) {
  if (req.headBytes > BRIDGING_MAX_HEAD_BYTES) return { status: 431, routed: null };
  if (!supportedTargetForm(req.target)) return { status: 400, message: "unsupported request target form", routed: null };
  if (requestPath(req.target) !== "/mcp") return { status: 404, routed: null };
  if (!fx.services) return bridgingServiceStatus(fx, req);
  const svc = bridgingRoute(fx, req);
  if (svc === null) return { status: 503, message: "service unavailable", routed: null };
  if (svc.state === "disabled" && svc.code === "DISABLED") return { status: 503, message: "service disabled", routed: svc.name };
  if (svc.state !== "active") return { status: 503, message: "service unavailable", routed: svc.name };
  return { ...bridgingServiceStatus({ ...fx, sdkState: "online" }, req), routed: svc.name };
}

function bridgingServiceStatus(fx, req) {
  if (fx.sdkState !== "online") return { status: 503, message: fx.sdkState === "disabled" ? "service disabled" : "service closing" };
  const cap = fx.limits.maxRequestBodyBytes ?? BRIDGING_DEFAULT_BODY_CAP;
  if (req.body.length > cap) return { status: 413 };
  if (!representable(req)) return { status: 400, message: "request cannot be represented as a web Request" };
  const behaviour = fx.handler ? fx.handler.behaviour : "echo";
  if (behaviour === "throw") return fx.mode === "handler" ? { status: 500 } : { error: 'behaviour "throw" is handler-only' };
  if (behaviour === "hang") return fx.mode === "target" ? { status: 504 } : { error: 'behaviour "hang" in handler mode has no SDK-generated response' };
  // MCPWARP.md §4.5: an app status below 200 or above 599 becomes 500 "handler error".
  if (behaviour === "status") return fx.handler.status < 200 || fx.handler.status > 599 ? { status: 500 } : { status: fx.handler.status };
  return { status: 200 };
}

const bridgingDir = path.join(mcpwarpDir, "bridging");
requireDir(bridgingDir, "fixtures/mcpwarp/bridging");
let mcpwarpBridgingCount = 0;

for (const file of listJsonFiles(bridgingDir)) {
  const rel = path.relative(SPEC_DIR, file);
  const fx = readJsonChecked(file);
  if (fx === undefined) continue;
  mcpwarpBridgingCount++;

  // A multi-service fixture (MCPWARP.md §4.8) has `services` (the held services and their §6A.4 states)
  // in place of `sdkState`, plus `username` and `routedTo`; every service shares mode/target/handler.
  const multi = "services" in fx;
  const requiredKeys = multi
    ? ["description", "username", "services", "routedTo", "mode", "limits", "in", "out", "upstream"]
    : ["description", "sdkState", "mode", "limits", "in", "out", "upstream"];
  const missing = requiredKeys.filter((k) => !(k in fx));
  if (missing.length) {
    check(false, `${rel}: missing required keys ${missing.join(", ")}`);
    continue;
  }
  check(typeof fx.description === "string" && fx.description.length > 0, `${rel}: description must be a non-empty string`);
  if (multi) {
    check(!("sdkState" in fx), `${rel}: a multi-service fixture has services[].state, not sdkState`);
    check(typeof fx.username === "string" && BRIDGING_USERNAME_RE.test(fx.username), `${rel}: username must match ${BRIDGING_USERNAME_RE}`);
    check(fx.routedTo === null || typeof fx.routedTo === "string", `${rel}: routedTo must be a service name or null`);
    if (!Array.isArray(fx.services) || fx.services.length < 2 || !fx.services.every(isPlainObject)) {
      check(false, `${rel}: services must be an array of at least two objects`);
      continue;
    }
    fx.services.forEach((s, i) => {
      const where = `${rel}: services[${i}]`;
      for (const k of Object.keys(s)) check(["name", "id", "url", "state", "code"].includes(k), `${where}: unknown key "${k}" (name, id, url, state, code?)`);
      check(typeof s.name === "string" && s.name.length <= 30 && /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/.test(s.name), `${where}: name must be a service name (SERVICE_NAME_RE, at most 30)`);
      check(typeof s.id === "string" && s.id !== "", `${where}: id must be a non-empty string (registered.services[].id)`);
      check(BRIDGING_SERVICE_STATES.includes(s.state), `${where}: state must be one of ${BRIDGING_SERVICE_STATES.join(", ")} (taken_over RESETs, which a bridging fixture cannot state)`);
      check(!("code" in s) || (s.state === "disabled" && s.code === "DISABLED"), `${where}: code is only "DISABLED" on a dashboard-disabled service`);
      let label;
      try {
        label = new URL(s.url).hostname.split(".")[0];
      } catch {
        label = undefined;
      }
      check(label === `${s.name}-${fx.username}`, `${where}: url (registered.services[].url) must have the host label "${s.name}-${fx.username}"`);
    });
    const names = fx.services.map((s) => s.name);
    const ids = fx.services.map((s) => s.id);
    check(new Set(names).size === names.length && new Set(ids).size === ids.length, `${rel}: services[] names and ids must be unique`);
    check(fx.routedTo === null || names.includes(fx.routedTo), `${rel}: routedTo ${JSON.stringify(fx.routedTo)} is not a services[] name`);
  }
  if ("runnerRequirement" in fx) {
    check(multi && BRIDGING_RUNNER_REQUIREMENTS.includes(fx.runnerRequirement), `${rel}: runnerRequirement must be one of ${BRIDGING_RUNNER_REQUIREMENTS.join(", ")} on a multi-service fixture, got ${JSON.stringify(fx.runnerRequirement)}`);
  }
  if (!multi) {
    check(BRIDGING_STATES.includes(fx.sdkState), `${rel}: sdkState must be one of ${BRIDGING_STATES.join(", ")}`);
  }
  check(BRIDGING_MODES.includes(fx.mode), `${rel}: mode must be one of ${BRIDGING_MODES.join(", ")}`);
  check(fx.mode !== "target" || typeof fx.target === "string", `${rel}: mode "target" needs a "target" URL`);
  check(fx.mode === "target" || !("target" in fx), `${rel}: "target" only goes with mode "target"`);
  if (!isPlainObject(fx.limits)) {
    check(false, `${rel}: limits must be an object`);
    continue;
  }
  for (const [k, v] of Object.entries(fx.limits)) {
    check(BRIDGING_LIMIT_KEYS.includes(k), `${rel}: unknown limits key "${k}"`);
    check(Number.isInteger(v) && v > 0, `${rel}: limits.${k} must be a positive integer`);
  }
  check(fx.mode === "target" || !("targetTimeoutMs" in fx.limits), `${rel}: limits.targetTimeoutMs applies only to mode "target"`);
  if ("handler" in fx) {
    check(isPlainObject(fx.handler) && BRIDGING_BEHAVIOURS.includes(fx.handler.behaviour), `${rel}: handler.behaviour must be one of ${BRIDGING_BEHAVIOURS.join(", ")}`);
    if (isPlainObject(fx.handler)) {
      const isStatus = fx.handler.behaviour === "status";
      check(Object.keys(fx.handler).every((k) => k === "behaviour" || (isStatus && k === "status")), `${rel}: handler has only "behaviour" (and "status" with behaviour "status")`);
      check(!isStatus || (Number.isInteger(fx.handler.status) && fx.handler.status >= 100 && fx.handler.status <= 999), `${rel}: behaviour "status" needs a three-digit integer handler.status`);
    }
  }
  if (typeof fx.in !== "string" || typeof fx.out !== "string") {
    check(false, `${rel}: "in" and "out" must be strings`);
    continue;
  }

  let inText = fx.in;
  if ("inExpand" in fx) {
    if (!isPlainObject(fx.inExpand)) {
      check(false, `${rel}: inExpand must be an object`);
      continue;
    }
    for (const [marker, spec] of Object.entries(fx.inExpand)) {
      const ok = isPlainObject(spec) && typeof spec.repeat === "string" && spec.repeat.length > 0 && Number.isInteger(spec.count) && spec.count > 0;
      check(ok, `${rel}: inExpand["${marker}"] must be {repeat: non-empty string, count: positive integer}`);
      check(inText.split(marker).length === 2, `${rel}: inExpand marker "${marker}" must occur exactly once in "in"`);
      if (ok) inText = inText.replace(marker, spec.repeat.repeat(spec.count));
    }
  }
  const inAlone = inText;
  if ("inRest" in fx) {
    check(typeof fx.inRest === "string" && fx.inRest.length > 0, `${rel}: inRest must be a non-empty string`);
    if (typeof fx.inRest === "string") inText += fx.inRest;
  }

  // outReset: the stream ends with RESET(code) instead of CLOSE, and "out" is the
  // response head written before it (MCPWARP.md §4.2 streaming adapters).
  const outReset = "outReset" in fx;
  if (outReset) {
    const ok = isPlainObject(fx.outReset) && Object.keys(fx.outReset).length === 1 && Object.values(ERROR_CODES).includes(fx.outReset.code);
    check(ok, `${rel}: outReset must be {code} with a WIRE.md §2.8 error code number`);
  }

  check(/^\S+ \S+ HTTP\/1\.1\r\n/.test(inText), `${rel}: "in" must start with a request line ending in CRLF`);
  check(/^HTTP\/1\.1 \d{3} [^\r\n]+\r\n/.test(fx.out), `${rel}: "out" must start with a status line ending in CRLF`);
  const req = parseHttpMessage(inText, "request");
  if (req.error) {
    check(false, `${rel}: "in": ${req.error}`);
    continue;
  }
  const res = parseHttpMessage(fx.out, "response", { headOnly: outReset || req.method === "HEAD" });
  if (res.error) {
    check(false, `${rel}: "out": ${res.error}`);
    continue;
  }

  check(req.get("host").length === 1 && req.get("host")[0] !== "", `${rel}: "in" must carry exactly one non-empty Host`);
  check(req.get("authorization").length === 0, `${rel}: "in" carries Authorization, which the edge strips`);

  const base = path.basename(file, ".json");
  const fromName = /_(\d{3})$/.exec(base);
  const intended = fromName ? Number(fromName[1]) : BRIDGING_NAMED_STATUS[base];
  if (outReset) {
    check(base.endsWith("_reset"), `${rel}: a fixture with outReset is named *_reset`);
  } else {
    check(intended !== undefined, `${rel}: file name must end in _<status> or be one of ${Object.keys(BRIDGING_NAMED_STATUS).join(", ")}`);
    check(intended === undefined || res.status === intended, `${rel}: "out" status ${res.status} does not match the file name's ${intended}`);
  }
  check(REASON_PHRASES[res.status] === res.reason, `${rel}: reason phrase for ${res.status} must be ${JSON.stringify(REASON_PHRASES[res.status])}, got ${JSON.stringify(res.reason)}`);

  const expected = bridgingExpectedStatus(fx, req);
  if (expected.error) {
    check(false, `${rel}: ${expected.error}`);
    continue;
  }
  if (multi) {
    check(fx.routedTo === expected.routed, `${rel}: routedTo ${JSON.stringify(fx.routedTo)}, but the MCPWARP.md §4.8 selection gives ${JSON.stringify(expected.routed)} (X-Mcpwarp-Server-Id first under runnerRequirement "trust-server-id-header", else the Host label split at its last "-")`);
  }
  if (outReset) {
    // The only §4 RESET after response bytes: a streamed body over the cap (413 by
    // precedence) once the app has already answered. CANCEL, chunked, response from the app.
    check(expected.status === 413, `${rel}: outReset is only for a body over the cap after the response head (MCPWARP.md §4.2), but the precedence rules give ${expected.status}`);
    check(fx.outReset.code === ERROR_CODES.CANCEL, `${rel}: a body cap that trips after response bytes RESETs with CANCEL (${ERROR_CODES.CANCEL}), got ${JSON.stringify(fx.outReset.code)}`);
    check(req.chunked === true, `${rel}: only a chunked body can trip the cap after the app was called (a Content-Length over the cap is refused first)`);
    check(typeof fx.inRest === "string", `${rel}: outReset needs inRest, written after the response head, so the head precedes the cap`);
    if (typeof fx.inRest === "string") {
      const cap = fx.limits.maxRequestBodyBytes ?? BRIDGING_DEFAULT_BODY_CAP;
      const alone = Buffer.from(inAlone, "utf8");
      const headEnd = alone.indexOf("\r\n\r\n");
      check(headEnd >= 0, `${rel}: with outReset "in" alone must hold the whole request head`);
      const soFar = headEnd < 0 ? 0 : chunkedBytesSoFar(alone.subarray(headEnd + 4));
      check(soFar <= cap, `${rel}: with outReset the body bytes in "in" alone (${soFar}) must stay within the cap (${cap}), so the cap trips only once inRest arrives, after the response head`);
    }
    check(!SDK_ERROR_MESSAGES[res.status], `${rel}: with outReset "out" is the app's response head, not an SDK error reply (${res.status})`);
    check(!fx.handler || fx.handler.behaviour !== "echo" || res.status === 200, `${rel}: an echo handler answers 200, "out" has ${res.status}`);
  } else {
    check(res.status === expected.status, `${rel}: the MCPWARP.md §4 precedence rules give ${expected.status}, "out" has ${res.status}`);
  }

  if (SDK_ERROR_MESSAGES[res.status]) {
    const allowed = expected.message ? [expected.message] : SDK_ERROR_MESSAGES[res.status];
    const exact = allowed.map((m) => sdkErrorReply(res.status, m, req.method));
    check(exact.includes(fx.out), `${rel}: an SDK error reply is byte-exact (MCPWARP.md §4.5): "out" must be ${exact.map((s) => JSON.stringify(s)).join(" or ")}`);
  }

  if (NO_APP_STATUSES.has(res.status)) {
    check(fx.upstream === null, `${rel}: a ${res.status} is answered without calling the app, so "upstream" must be null`);
  } else if (!isPlainObject(fx.upstream)) {
    check(false, `${rel}: "upstream" must describe the request the app receives`);
  } else {
    const up = fx.upstream;
    const host = req.get("host")[0];
    const target = forwardedTarget(req.target);
    let wantUrl;
    let wantHeaders = forwardableHeaders(req);
    if (fx.mode === "handler") {
      wantUrl = `https://${host}${target}`;
    } else {
      // Target mode (MCPWARP.md §4.6): the target URL's own query first, then the request's, joined
      // with "&", both verbatim (no URLSearchParams-style rebuild). An empty target path is "/", which
      // is what the local server receives (http://127.0.0.1:3000 + ?x=1 -> /?x=1).
      let [base, ownQuery = ""] = fx.target.split("#")[0].split(/\?(.*)/s);
      if (/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\/[^/]*$/.test(base)) base += "/";
      const reqQuery = target.includes("?") ? target.slice(target.indexOf("?") + 1) : "";
      const query = [ownQuery, reqQuery].filter((q) => q !== "").join("&");
      wantUrl = query === "" ? base : `${base}?${query}`;
      delete wantHeaders.host;
      wantHeaders = { ...wantHeaders, "accept-encoding": "identity", "x-forwarded-host": host, "x-forwarded-proto": "https" };
    }
    check(up.method === req.method, `${rel}: upstream.method ${JSON.stringify(up.method)} != request method ${req.method}`);
    check(up.url === wantUrl, `${rel}: upstream.url must be ${wantUrl}, got ${JSON.stringify(up.url)}`);
    check(isPlainObject(up.headers) && sameJson(up.headers, wantHeaders), `${rel}: upstream.headers must be ${JSON.stringify(wantHeaders)}`);
    if (outReset) {
      check(!("body" in up), `${rel}: with outReset upstream.body is absent (the app reads a body stream that fails)`);
    } else {
      const wantBody = req.method === "GET" || req.method === "HEAD" ? undefined : req.body.toString("utf8");
      check(up.body === wantBody, `${rel}: upstream.body must be the request body unchanged${wantBody === undefined ? " (absent for GET/HEAD)" : ""}`);
    }
    if (res.status === 200 && fx.handler && fx.handler.behaviour === "echo") {
      check(outReset || res.body.equals(req.body), `${rel}: an echo handler's response body must equal the request body`);
      check(res.get("content-type").join() === req.get("content-type").join(), `${rel}: an echo handler's response content-type must equal the request's`);
    }
  }
}

// --- 4.3 tunnel controller sequences (writer of tunnel/: shape in its README) ---

const TUNNEL_STATES = ["off", "connecting", "online", "disabled", "conflict", "error"];
// Legal (state, code) pairs of a TunnelStatus, 13 codes; null = no code.
// connecting+INTERNAL: a non-refusal mcpwarp error while connecting (e.g. MALFORMED_ENVELOPE).
const TUNNEL_PAIRS = {
  off: [null],
  connecting: [null, "DISCONNECTED", "RETRYING", "INTERNAL"],
  online: [null],
  disabled: ["DISABLED"],
  conflict: ["TAKEN_OVER"],
  error: ["UNAUTHORIZED", "INVALID_CONFIG", "INVALID_NAME", "USERNAME_REQUIRED", "QUOTA_EXCEEDED", "CONFLICT", "UNSUPPORTED_VERSION", "DISCONNECTED_FATAL", "INTERNAL"],
};
const TUNNEL_CODES = new Set(Object.values(TUNNEL_PAIRS).flat().filter((c) => c !== null));
const TUNNEL_SNAPSHOT_KEYS = new Set(["state", "code", "lastError", "url"]);
// MCPWARP.md §6.6 lastError constants per code. INTERNAL has one text for both
// connecting/INTERNAL and error/INTERNAL.
const TUNNEL_LAST_ERRORS = {
  RETRYING: ["Can't reach mcpwarp; retrying.", "mcpwarp couldn't register the service; retrying."],
  DISCONNECTED: ["Connection lost; retrying."],
  TAKEN_OVER: ["Another connection took over this name."],
  DISABLED: ["This service is disabled in the mcpwarp dashboard."],
  UNAUTHORIZED: ["The token was rejected.", "The token is not allowed to publish this service."],
  INVALID_NAME: ["The service name is invalid: use lowercase letters, digits and dashes (not at the start or end), at most 30 characters."],
  INVALID_CONFIG: ["The tunnel settings are invalid.", "The token is not set.", "The service name is not set."],
  USERNAME_REQUIRED: ["Choose a username in your mcpwarp account first."],
  QUOTA_EXCEEDED: ["Your mcpwarp account has no services left in its quota."],
  CONFLICT: ["The name is in use by another server."],
  UNSUPPORTED_VERSION: ["This SDK version is no longer supported; please update."],
  DISCONNECTED_FATAL: ["The connection to mcpwarp failed."],
  INTERNAL: ["Something went wrong; the log has the details."],
};

// A snapshot's code is absent (no code) or a TunnelStatusCode string; null and
// "none" are not ways to spell "no code" in a fixture.
function tunnelCode(snap) {
  return "code" in snap ? snap.code : null;
}

// The event vocabulary is tunnel/README.md's Events table: `name` | payload | meaning.
// A payload written as {a, b?} lists its keys (b optional); any other payload
// (partial TunnelConfig, the burst array) is checked by name below.
function parseTunnelVocabulary(markdown) {
  const vocab = {};
  const section = (markdown.split(/^## Events\s*$/m)[1] ?? "").split(/^## /m)[0];
  for (const line of section.split("\n")) {
    const m = /^\| `([a-z_]+)` \| (.+?) \| /.exec(line);
    if (!m) continue;
    const payload = m[2].replace(/`/g, "").trim();
    if (!payload.startsWith("{")) {
      vocab[m[1]] = null;
      continue;
    }
    let inner = payload.slice(1, payload.lastIndexOf("}"));
    for (let prev; prev !== inner; ) {
      prev = inner;
      inner = inner.replace(/\[[^[\]]*\]|\{[^{}]*\}/g, "");
    }
    const keys = inner.split(",").map((s) => s.replace(/:.*$/, "").trim()).filter(Boolean);
    vocab[m[1]] = {
      required: keys.filter((k) => !k.endsWith("?")),
      optional: keys.filter((k) => k.endsWith("?")).map((k) => k.slice(0, -1)),
    };
  }
  return vocab;
}

const TUNNEL_RETRY_CAUSES = ["dial", "lost", "register", "overloaded", "drain", "token-refresh"];
const TUNNEL_PROVIDER_RESULTS = ["ok", "unavailable", "error"];
// Edge messages that disconnect.flush_after may hold: app messages the SDK delivers after the close.
const TUNNEL_FLUSHABLE = ["registered", "register_error", "app_error", "disabled", "enabled", "conflict"];
const TUNNEL_FIXTURE_KEYS = new Set(["description", "config", "events", "expect", "expectDials", "runnerRequirement"]);
// A runner without the capability skips the fixture with a note (tunnel/README.md).
const TUNNEL_RUNNER_REQUIREMENTS = ["post-disconnect-delivery"];

// nested: "burst" (configure / stop calls) or "flush" (disconnect.flush_after edge messages).
function checkTunnelEvent(ev, where, vocab, dials, nested) {
  if (!isPlainObject(ev) || Object.keys(ev).length !== 1) {
    check(false, `${where}: an event is an object with exactly one key`);
    return;
  }
  const [name, payload] = Object.entries(ev)[0];
  if (!(name in vocab)) {
    check(false, `${where}: unknown event "${name}" (tunnel/README.md Events: ${Object.keys(vocab).join(", ")})`);
    return;
  }
  if (nested === "burst") check(name === "configure" || name === "stop", `${where}: a burst holds only configure / stop events, got "${name}"`);
  if (nested === "flush") {
    check(TUNNEL_FLUSHABLE.includes(name), `${where}: flush_after holds only edge messages (${TUNNEL_FLUSHABLE.join(", ")}), got "${name}"`);
    check(!isPlainObject(payload) || !("conn" in payload), `${where}.${name}: a flush_after message belongs to the disconnecting connection, no "conn"`);
  }
  if (name === "burst") {
    if (!Array.isArray(payload) || payload.length === 0) {
      check(false, `${where}.burst: a non-empty array of events`);
      return;
    }
    payload.forEach((b, j) => checkTunnelEvent(b, `${where}.burst[${j}]`, vocab, dials, "burst"));
    return;
  }
  if (!isPlainObject(payload)) {
    check(false, `${where}.${name}: the payload must be an object`);
    return;
  }
  const spec = vocab[name];
  if (spec === null) return;
  for (const k of Object.keys(payload)) {
    check(spec.required.includes(k) || spec.optional.includes(k), `${where}.${name}: unknown payload key "${k}" (allowed: ${[...spec.required, ...spec.optional.map((o) => `${o}?`)].join(", ") || "none"})`);
  }
  for (const k of spec.required) check(k in payload, `${where}.${name}: missing payload key "${k}"`);
  if ("conn" in payload) {
    check(Number.isInteger(payload.conn) && payload.conn >= 1 && (dials === undefined || payload.conn <= dials), `${where}.${name}: conn must be a dial ordinal 1..expectDials`);
  }
  // MCPWARP.md §1 Upgrade-status mapping: the final HTTP status of a refused upgrade (no 1xx: 101 is success).
  if (name === "dial_failed" && "httpStatus" in payload) {
    const s = payload.httpStatus;
    check(Number.isInteger(s) && s >= 200 && s <= 599, `${where}.${name}: httpStatus must be an HTTP status 200..599, got ${JSON.stringify(s)}`);
  }
  if (name === "retry") {
    check(TUNNEL_RETRY_CAUSES.includes(payload.cause), `${where}.retry: cause must be one of ${TUNNEL_RETRY_CAUSES.join(", ")}, got ${JSON.stringify(payload.cause)}`);
    check(Number.isInteger(payload.delayMs) && payload.delayMs >= 0, `${where}.retry: delayMs must be a non-negative integer`);
    if (payload.cause === "token-refresh") check(payload.delayMs === 0, `${where}.retry: the token-refresh redial is immediate, delayMs 0`);
  }
  if (name === "token_provider") {
    check(TUNNEL_PROVIDER_RESULTS.includes(payload.result), `${where}.token_provider: result must be one of ${TUNNEL_PROVIDER_RESULTS.join(", ")}, got ${JSON.stringify(payload.result)}`);
  }
  if (name === "drain" && "retry_after_ms" in payload) {
    check(Number.isInteger(payload.retry_after_ms) && payload.retry_after_ms >= 0, `${where}.drain: retry_after_ms must be a non-negative integer`);
  }
  if (name === "disabled" && "id" in payload) {
    check(typeof payload.id === "string" && payload.id !== "", `${where}.disabled: id must be a non-empty string`);
  }
  if (name === "disconnect" && "flush_after" in payload) {
    if (!Array.isArray(payload.flush_after) || payload.flush_after.length === 0) {
      check(false, `${where}.disconnect: flush_after must be a non-empty array of edge messages`);
    } else {
      payload.flush_after.forEach((m, j) => checkTunnelEvent(m, `${where}.disconnect.flush_after[${j}]`, vocab, dials, "flush"));
    }
  }
}

// drain.retry_after_ms binds the replacement dial (WIRE.md §2.9): the next drain retry must allow at
// least that long, and at most 2000 ms of jitter more.
function checkDrainRetryAfter(events, rel) {
  events.forEach((ev, i) => {
    const hint = isPlainObject(ev) && isPlainObject(ev.drain) ? ev.drain.retry_after_ms : undefined;
    if (!Number.isInteger(hint)) return;
    const next = events.slice(i + 1).find((e) => isPlainObject(e) && isPlainObject(e.retry) && e.retry.cause === "drain");
    check(next !== undefined, `${rel}#events[${i}]: a drain with retry_after_ms needs a later retry {cause: "drain"}`);
    if (next) check(next.retry.delayMs >= hint && next.retry.delayMs <= hint + 2000, `${rel}#events[${i}]: the drain retry after retry_after_ms ${hint} must allow ${hint}..${hint + 2000} ms, got delayMs ${next.retry.delayMs}`);
  });
}

function collectConfigs(events, out) {
  for (const ev of events) {
    if (!isPlainObject(ev)) continue;
    if (isPlainObject(ev.configure)) out.push(ev.configure);
    if (Array.isArray(ev.burst)) collectConfigs(ev.burst, out);
  }
  return out;
}

// --- group (multi-service controller) fixtures: tunnel/README.md "Group fixtures" ---

const GROUP_SERVICE_STATES = ["pending", "active", "disabled", "rejected", "taken_over", "unregistered"];
const GROUP_ONLY_EVENTS = ["unregistered", "add_service", "remove_service", "enable_service", "disable_service", "sent"];
const REGISTER_ERROR_CODES = mcpwarpSchemaDoc.$defs.registered.properties.errors.items.properties.code.enum;
// MCPWARP.md §2.5 TRANSIENT_REGISTER_ERRORS: re-sent after the backoff, never a service state of their own.
const TRANSIENT_REGISTER_CODES = ["INTERNAL", "NO_RESPONDERS", "TIMEOUT", "ABORTED", "CONNECTION_LOST", "MALFORMED_REPLY", "UNAVAILABLE", "SERVICE_ERROR"];
// §6A.6 G7: whole-frame error ops that answer an outstanding register (any code but these three).
const NOT_FRAME_ANSWERS = ["OVERLOADED", "CONNECTION_LIMIT", "UNSUPPORTED_VERSION"];
const GROUP_FATAL_LAST_ERROR = "No service could be registered.";
const SERVICE_NAME_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;
const SERVICE_KINDS = ["http", "stdio"];
const groupOverloadDelay = (streak) => Math.min(1000 * 2 ** (streak - 1), 30000);
// §6A.4 ServiceStatus lastError of a rejected service, by its wire code.
function rejectedLastError(code) {
  if (code === "UNAUTHENTICATED") return TUNNEL_LAST_ERRORS.UNAUTHORIZED[0];
  if (code === "FORBIDDEN") return TUNNEL_LAST_ERRORS.UNAUTHORIZED[1];
  if (["INVALID_NAME", "USERNAME_REQUIRED", "QUOTA_EXCEEDED", "CONFLICT"].includes(code)) return TUNNEL_LAST_ERRORS[code][0];
  return TUNNEL_LAST_ERRORS.INTERNAL[0];
}

function checkGroupServiceList(list, where) {
  if (!Array.isArray(list) || list.length < 1 || list.length > 100) {
    check(false, `${where}: services must be an array of 1..100 {name, kind?}`);
    return [];
  }
  list.forEach((s, i) => {
    const ok = isPlainObject(s) && Object.keys(s).every((k) => k === "name" || k === "kind");
    check(ok && typeof s.name === "string" && s.name.length <= 30 && SERVICE_NAME_RE.test(s.name), `${where}[${i}]: needs a valid name (SERVICE_NAME_RE, at most 30) and only name, kind?`);
    check(ok && (!("kind" in s) || SERVICE_KINDS.includes(s.kind)), `${where}[${i}]: kind must be one of ${SERVICE_KINDS.join(", ")}`);
  });
  const names = list.map((s) => (isPlainObject(s) ? s.name : undefined));
  check(new Set(names).size === names.length, `${where}: a name appears twice`);
  return names;
}

// Walks a group fixture's events: payload rules the vocabulary cannot state, the one-frame-in-flight rule,
// registered/unregistered against the outstanding frame, the OVERLOADED backoff, and the facts a snapshot's
// per-service state must be backed by. Returns those facts.
function checkGroupEvents(fx, rel) {
  const facts = {
    known: new Set(),
    active: new Set(),
    rejected: new Set(), // "<name> <code>"
    disabledDash: new Set(),
    disabledIds: new Set(),
    disabledLocal: new Set(),
    ids: new Map(), // name -> every server id learned for it
    urls: new Map(), // name -> every url registered for it
    conflicts: new Set(),
    removed: new Set(),
    fatalReply: false, // a register answer that can end in G5: no success and no SERVER_DISABLED, or a whole-frame refusal
  };
  const learn = (map, name, value) => map.set(name, new Set([...(map.get(name) ?? []), value]));
  let configured = new Set(checkGroupServiceList(fx.config.services, `${rel}: config.services`)); // names configured now
  for (const n of configured) facts.known.add(n);
  check(!("name" in fx.config), `${rel}: a group config has services, not name`);
  let inFlight = null;
  let dropped = null; // the frame an OVERLOADED dropped, until it is re-sent
  let streak = 0; // §2.6 / §3.2 counter, per controller: kept across reconnects, reset by a reply with a non-transient entry
  let current = null; // the merged config of the last configure, null before it and after stop / dispose
  const endConnection = () => {
    inFlight = null;
    dropped = null;
  };
  // A configure restarts (redials) unless it changes only services: same trimmed token, enabled unchanged, every
  // other key equal.
  const restarts = (prev, next) => {
    if (prev === null) return true;
    const trim = (t) => (typeof t === "string" ? t.trim() : t);
    return [...new Set([...Object.keys(prev), ...Object.keys(next)])].some((k) => {
      if (k === "services") return false;
      if (k === "token") return trim(prev.token) !== trim(next.token);
      return JSON.stringify(prev[k]) !== JSON.stringify(next[k]);
    });
  };
  fx.events.forEach((ev, i) => {
    if (!isPlainObject(ev) || Object.keys(ev).length !== 1) return;
    const [name, p] = Object.entries(ev)[0];
    const where = `${rel}#events[${i}].${name}`;
    if (name === "burst" || (name === "disconnect" && isPlainObject(p) && "flush_after" in p)) {
      check(false, `${where}: burst and flush_after are not used in group fixtures`);
      return;
    }
    if (!isPlainObject(p)) return;
    switch (name) {
      case "configure": {
        if ("services" in p) {
          configured = new Set(checkGroupServiceList(p.services, `${where}.services`));
          for (const n of configured) facts.known.add(n);
        }
        check(!("name" in p), `${where}: a group configure has services, not name`);
        const next = { ...(current ?? fx.config), ...p };
        if (restarts(current, next)) {
          endConnection();
          streak = 0;
        }
        current = next;
        break;
      }
      case "stop":
      case "dispose":
        endConnection();
        streak = 0;
        current = null;
        break;
      case "connected":
      case "welcome":
      case "dial_failed":
      case "disconnect":
        endConnection();
        break;
      case "add_service":
        // A name removed earlier (taken_over ones included) may be added again; only a currently configured one may not.
        check(typeof p.name === "string" && p.name.length <= 30 && SERVICE_NAME_RE.test(p.name) && !configured.has(p.name), `${where}: name must be a valid service name not currently configured`);
        check(!("kind" in p) || SERVICE_KINDS.includes(p.kind), `${where}: kind must be one of ${SERVICE_KINDS.join(", ")}`);
        configured.add(p.name);
        facts.known.add(p.name);
        break;
      case "remove_service":
      case "enable_service":
      case "disable_service":
        check(facts.known.has(p.name), `${where}: ${JSON.stringify(p.name)} is not a configured service`);
        if (name === "remove_service") {
          facts.removed.add(p.name);
          configured.delete(p.name);
        }
        if (name === "disable_service") facts.disabledLocal.add(p.name);
        break;
      case "sent": {
        check(p.op === "register" || p.op === "unregister", `${where}: op must be register or unregister`);
        const names = Array.isArray(p.names) ? p.names : [];
        check(names.length >= 1 && names.length <= 100 && new Set(names).size === names.length, `${where}: names must be 1..100 distinct names`);
        for (const n of names) check(facts.known.has(n), `${where}: ${JSON.stringify(n)} is not a configured service`);
        check(inFlight === null, `${where}: a ${p.op} is sent while ${inFlight ? `the ${inFlight.op} of [${inFlight.names.join(", ")}]` : ""} is still outstanding (one frame in flight per connection)`);
        if (dropped) {
          check(dropped.retried, `${where}: the frame OVERLOADED dropped is re-sent before its retry {cause: "overloaded"}`);
          check(dropped.op === p.op && JSON.stringify(dropped.names) === JSON.stringify(names), `${where}: after OVERLOADED the next frame is the dropped ${dropped.op} [${dropped.names.join(", ")}] again`);
          dropped = null;
        }
        inFlight = { op: p.op, names };
        break;
      }
      case "registered": {
        const services = Array.isArray(p.services) ? p.services : [];
        const errors = Array.isArray(p.errors) ? p.errors : [];
        check(inFlight !== null && inFlight.op === "register", `${where}: no register is outstanding`);
        const answered = [];
        services.forEach((s, j) => {
          const ok = isPlainObject(s) && typeof s.name === "string" && typeof s.id === "string" && s.id !== "" && typeof s.url === "string" && s.url !== "";
          check(ok, `${where}.services[${j}]: needs name, id, url`);
          if (!ok) return;
          answered.push(s.name);
          facts.active.add(s.name);
          learn(facts.ids, s.name, s.id);
          learn(facts.urls, s.name, s.url);
        });
        let nonTransient = services.length > 0;
        errors.forEach((e, j) => {
          const ok = isPlainObject(e) && typeof e.name === "string" && REGISTER_ERROR_CODES.includes(e.code);
          check(ok, `${where}.errors[${j}]: needs name and a RegisterErrorCode`);
          if (!ok) return;
          check(("id" in e) === (e.code === "SERVER_DISABLED"), `${where}.errors[${j}]: id is carried by SERVER_DISABLED and only by it`);
          answered.push(e.name);
          if (!TRANSIENT_REGISTER_CODES.includes(e.code)) nonTransient = true;
          if (e.code === "SERVER_DISABLED") {
            facts.disabledDash.add(e.name);
            learn(facts.ids, e.name, e.id);
          } else if (!TRANSIENT_REGISTER_CODES.includes(e.code)) facts.rejected.add(`${e.name} ${e.code}`);
        });
        if (inFlight && inFlight.op === "register") {
          check(JSON.stringify([...answered].sort()) === JSON.stringify([...inFlight.names].sort()), `${where}: lists [${answered.join(", ")}], but a registered answers exactly the names of its register [${inFlight.names.join(", ")}], each once`);
        }
        if (services.length === 0 && !errors.some((e) => isPlainObject(e) && e.code === "SERVER_DISABLED")) facts.fatalReply = true;
        inFlight = null;
        // §2.6: the streak counts OVERLOADED and all-transient replies; any other entry ends it.
        streak = nonTransient ? 0 : streak + 1;
        break;
      }
      case "unregistered": {
        const services = Array.isArray(p.services) ? p.services : [];
        check(inFlight !== null && inFlight.op === "unregister", `${where}: no unregister is outstanding`);
        services.forEach((s, j) => {
          check(isPlainObject(s) && typeof s.name === "string" && typeof s.id === "string", `${where}.services[${j}]: needs name, id`);
          if (inFlight && isPlainObject(s)) check(inFlight.names.includes(s.name), `${where}.services[${j}]: ${JSON.stringify(s.name)} was not in the unregister`);
        });
        inFlight = null;
        streak = 0;
        break;
      }
      case "app_error":
        if (p.code === "OVERLOADED") {
          check(inFlight !== null, `${where}: OVERLOADED with no register/unregister outstanding`);
          if (inFlight) dropped = { ...inFlight, retried: false };
          inFlight = null;
          streak++;
        } else if (!NOT_FRAME_ANSWERS.includes(p.code)) {
          if (p.code === "TOO_MANY_SERVICES") check(inFlight !== null && inFlight.op === "register", `${where}: TOO_MANY_SERVICES answers an outstanding register`);
          // §6A.6 G7: the whole-frame refusal answers the outstanding frame; a register's names are rejected.
          if (inFlight && inFlight.op === "register") {
            for (const n of inFlight.names) facts.rejected.add(`${n} ${p.code}`);
            facts.fatalReply = true;
          }
          inFlight = null;
        }
        break;
      case "retry":
        if (p.cause === "overloaded") {
          check(dropped !== null && !dropped.retried, `${where}: an overloaded retry needs a frame OVERLOADED dropped`);
          check(p.delayMs === groupOverloadDelay(streak), `${where}: the ${streak}. consecutive OVERLOADED / all-transient reply backs off ${groupOverloadDelay(streak)} ms, got ${p.delayMs}`);
          if (dropped) dropped.retried = true;
        }
        break;
      case "conflict":
        check(typeof p.name === "string" && p.name !== "", `${where}: a group conflict carries the name`);
        // §3.3: a conflict for a name never held (unconfigured, or configured but never registered) is ignored,
        // so it backs no taken_over.
        if (facts.known.has(p.name) && (facts.active.has(p.name) || facts.disabledDash.has(p.name))) facts.conflicts.add(p.name);
        break;
      case "disabled":
        check(typeof p.id === "string" && p.id !== "", `${where}: a group disable carries the service id`);
        facts.disabledIds.add(p.id);
        break;
      case "enabled":
        check((typeof p.id === "string" && p.id !== "") || (typeof p.name === "string" && p.name !== ""), `${where}: a group enable carries an id, a name, or both`);
        if (typeof p.id === "string" && facts.known.has(p.name)) learn(facts.ids, p.name, p.id);
        break;
    }
  });
  return facts;
}

function checkGroupSnapshot(snap, where, facts) {
  if (!isPlainObject(snap.services)) {
    check(false, `${where}: a group snapshot has a services map`);
    return;
  }
  // §6A.4: the group's (state, code) pairs are §6.5's minus disabled/DISABLED.
  check(snap.state !== "disabled", `${where}: a group is never disabled as a whole (a dashboard disable is per service)`);
  // §6A.6 G5. Snapshots are not tied to event positions, so "the preceding registered" is approximated: the
  // fixture holds a register answer with no success and no SERVER_DISABLED (or a whole-frame refusal), and the
  // snapshot itself has every service rejected or locally disabled, at least one rejected.
  if (snap.state === "error" && snap.lastError === GROUP_FATAL_LAST_ERROR) {
    check(facts.fatalReply, `${where}: "${GROUP_FATAL_LAST_ERROR}" needs a registered with no services[] entry and no SERVER_DISABLED (or a whole-frame refusal) before it (G5)`);
    const svcs = Object.values(snap.services).filter(isPlainObject);
    check(svcs.some((s) => s.state === "rejected") && svcs.every((s) => s.state === "rejected" || (s.state === "disabled" && !("code" in s))), `${where}: G5 holds only while every service is rejected or locally disabled, at least one rejected`);
  }
  const SERVICE_KEYS = ["state", "id", "url", "code", "lastError"];
  for (const [name, s] of Object.entries(snap.services)) {
    const at = `${where}.services["${name}"]`;
    check(facts.known.has(name), `${at}: not a configured service`);
    if (!isPlainObject(s) || !Object.keys(s).every((k) => SERVICE_KEYS.includes(k)) || !GROUP_SERVICE_STATES.includes(s.state)) {
      check(false, `${at}: must be {state, id?, url?, code?, lastError?} with state one of ${GROUP_SERVICE_STATES.join(", ")}`);
      continue;
    }
    if ("id" in s) check(facts.ids.get(name)?.has(s.id) === true, `${at}: id ${JSON.stringify(s.id)} was never learned for this name (registered, SERVER_DISABLED, enable)`);
    check(("url" in s) === (s.state === "active"), `${at}: url is present exactly while active`);
    if ("url" in s) check(facts.urls.get(name)?.has(s.url) === true, `${at}: url ${JSON.stringify(s.url)} was never registered for this name`);
    check(("code" in s) === ("lastError" in s), `${at}: code and lastError are set together`);
    const dashboard = s.state === "disabled" && "code" in s;
    let want = null; // [code, lastError] the §6A.4 table requires
    if (dashboard) want = ["DISABLED", TUNNEL_LAST_ERRORS.DISABLED[0]];
    if (s.state === "taken_over") want = ["TAKEN_OVER", TUNNEL_LAST_ERRORS.TAKEN_OVER[0]];
    if (s.state === "rejected") want = [s.code, typeof s.code === "string" ? rejectedLastError(s.code) : undefined];
    if (want === null) {
      check(!("code" in s), `${at}: ${s.state} carries no code`);
    } else {
      check(s.code === want[0] && s.lastError === want[1], `${at}: ${s.state} needs code ${JSON.stringify(want[0])} with lastError ${JSON.stringify(want[1])}, got ${JSON.stringify(s.code)} / ${JSON.stringify(s.lastError)}`);
    }
    const ids = [...(facts.ids.get(name) ?? [])];
    const backed = {
      pending: true,
      active: facts.active.has(name),
      rejected: facts.rejected.has(`${name} ${s.code}`),
      disabled: dashboard ? facts.disabledDash.has(name) || ids.some((id) => facts.disabledIds.has(id)) : facts.disabledLocal.has(name),
      taken_over: facts.conflicts.has(name),
      unregistered: facts.removed.has(name),
    }[s.state];
    check(backed, `${at}: state ${s.state}${s.code ? ` ${s.code}` : ""} has no event behind it in this fixture`);
  }
}

const tunnelDir = path.join(mcpwarpDir, "tunnel");
let mcpwarpTunnelCount = 0;

if (!fs.existsSync(tunnelDir)) {
  mcpwarpNotes.push("fixtures/mcpwarp/tunnel/ missing, skipped");
} else {
  const files = listJsonFiles(tunnelDir);
  if (files.length === 0) mcpwarpNotes.push("fixtures/mcpwarp/tunnel/ has no *.json yet, skipped");
  const readmePath = path.join(tunnelDir, "README.md");
  const tunnelVocab = fs.existsSync(readmePath) ? parseTunnelVocabulary(fs.readFileSync(readmePath, "utf8")) : {};
  check(Object.keys(tunnelVocab).length > 0, "fixtures/mcpwarp/tunnel/README.md: missing, or its Events table lists no events");
  for (const file of files) {
    const rel = path.relative(SPEC_DIR, file);
    const fx = readJsonChecked(file);
    if (fx === undefined) continue;
    mcpwarpTunnelCount++;

    check(typeof fx.description === "string" && fx.description.length > 0, `${rel}: description must be a non-empty string`);
    check(isPlainObject(fx.config), `${rel}: config must be an object`);
    check(Array.isArray(fx.events), `${rel}: events must be an array`);
    check(Array.isArray(fx.expect) && fx.expect.length > 0, `${rel}: expect must be a non-empty array of TunnelStatus snapshots`);
    if (!isPlainObject(fx.config) || !Array.isArray(fx.events) || !Array.isArray(fx.expect)) continue;
    if ("expectDials" in fx) check(Number.isInteger(fx.expectDials) && fx.expectDials >= 0, `${rel}: expectDials must be a non-negative integer`);
    for (const k of Object.keys(fx)) check(TUNNEL_FIXTURE_KEYS.has(k), `${rel}: unknown top-level key "${k}" (tunnel/README.md: ${[...TUNNEL_FIXTURE_KEYS].join(", ")})`);
    if ("runnerRequirement" in fx) {
      check(TUNNEL_RUNNER_REQUIREMENTS.includes(fx.runnerRequirement), `${rel}: runnerRequirement must be one of ${TUNNEL_RUNNER_REQUIREMENTS.join(", ")}, got ${JSON.stringify(fx.runnerRequirement)}`);
    }
    const flushes = fx.events.some((ev) => isPlainObject(ev) && isPlainObject(ev.disconnect) && "flush_after" in ev.disconnect);
    check(flushes === (fx.runnerRequirement === "post-disconnect-delivery"), `${rel}: a fixture with disconnect.flush_after carries runnerRequirement "post-disconnect-delivery", and only such a fixture does`);

    fx.events.forEach((ev, i) => checkTunnelEvent(ev, `${rel}#events[${i}]`, tunnelVocab, fx.expectDials, null));
    checkDrainRetryAfter(fx.events, rel);
    const group = "services" in fx.config;
    check(group === path.basename(file).startsWith("ms_"), `${rel}: a group fixture (config.services) is named ms_NN_*.json, and only it`);
    const groupFacts = group ? checkGroupEvents(fx, rel) : null;
    if (!group) {
      fx.events.forEach((ev, i) => {
        const name = isPlainObject(ev) ? Object.keys(ev)[0] : undefined;
        check(!GROUP_ONLY_EVENTS.includes(name), `${rel}#events[${i}]: "${name}" is only for group fixtures`);
        if (name === "registered" && isPlainObject(ev.registered)) {
          const p = ev.registered;
          check(!("errors" in p) && (!Array.isArray(p.services) || p.services.every((s) => !isPlainObject(s) || !("name" in s))), `${rel}#events[${i}].registered: a single-service registered has no errors and no names (register_error instead)`);
        }
      });
    }
    const subscribes = fx.events.filter((ev) => isPlainObject(ev) && "subscribe" in ev).length;
    let repeats = 0;
    fx.expect.forEach((snap, i) => {
      if (i > 0 && isPlainObject(snap) && isPlainObject(fx.expect[i - 1]) && sameJson(snap, fx.expect[i - 1])) repeats++;
    });
    check(repeats <= subscribes, `${rel}: ${repeats} snapshot(s) repeat the one before, but only a subscribe event (${subscribes} here) delivers an unchanged status`);

    const configs = collectConfigs(fx.events, [fx.config]);
    const tokens = [];
    for (const c of configs) {
      if (typeof c.token === "string" && c.token.trim() !== "") tokens.push(c.token.trim());
    }

    fx.expect.forEach((snap, i) => {
      const where = `${rel}#expect[${i}]`;
      if (!isPlainObject(snap)) {
        check(false, `${where}: a snapshot must be an object`);
        return;
      }
      for (const k of Object.keys(snap)) check(TUNNEL_SNAPSHOT_KEYS.has(k) || (group && k === "services"), `${where}: unknown TunnelStatus key "${k}"`);
      if (group) checkGroupSnapshot(snap, where, groupFacts);
      if (!TUNNEL_STATES.includes(snap.state)) {
        check(false, `${where}: state must be one of ${TUNNEL_STATES.join(", ")}, got ${JSON.stringify(snap.state)}`);
        return;
      }
      const code = tunnelCode(snap);
      check(!("code" in snap) || TUNNEL_CODES.has(snap.code), `${where}: ${JSON.stringify(snap.code)} is not a TunnelStatusCode (leave "code" out for no code)`);
      check(TUNNEL_PAIRS[snap.state].includes(code), `${where}: (${snap.state}, ${code ?? "none"}) is not a legal pairing; ${snap.state} allows ${TUNNEL_PAIRS[snap.state].map((c) => c ?? "none").join(" | ")}`);
      const hasLastError = "lastError" in snap;
      check(hasLastError === (code !== null), `${where}: lastError must be present exactly when a code is (code ${code ?? "none"}, lastError ${hasLastError ? "present" : "absent"})`);
      if (hasLastError) {
        const le = snap.lastError;
        check(typeof le === "string" && le.length > 0, `${where}: lastError must be a non-empty string`);
        if (typeof le === "string") {
          // A group's fatal first reply is error/INTERNAL with its own constant (tunnel/README.md Group fixtures).
          const canonical = [...(TUNNEL_LAST_ERRORS[code] ?? []), ...(group && snap.state === "error" && code === "INTERNAL" ? [GROUP_FATAL_LAST_ERROR] : [])];
          check(canonical.includes(le), `${where}: lastError for ${code} must be a MCPWARP.md §6.6 constant (${canonical.map((s) => JSON.stringify(s)).join(" | ")}), got ${JSON.stringify(le)}`);
          // a short redacted token ("t") is checked by equality only
          for (const t of tokens) check(t.length < 4 ? le !== t : !le.includes(t), `${where}: lastError contains the token`);
        }
      }
      if (group) {
        check(!("url" in snap), `${where}: a group snapshot has no connection-level url`);
      } else if (snap.state === "online") {
        check(typeof snap.url === "string" && snap.url.length > 0, `${where}: online carries a non-empty url`);
      } else {
        check(!("url" in snap), `${where}: url is only set while online`);
      }
    });
  }
}

// --- 4.4 status text tables --------------------------------------------------

const statusTextDir = path.join(mcpwarpDir, "status-text");
let mcpwarpStatusTextCount = 0;

function checkStatusString(s, where) {
  check(typeof s === "string" && s.length > 0, `${where}: must be a non-empty string`);
  if (typeof s !== "string") return;
  check(!s.includes("..."), `${where}: ASCII "..." instead of U+2026`);
  check(!/[.。]$/.test(s), `${where}: trailing "." or "。"`);
  check(s === s.trim(), `${where}: leading or trailing whitespace`);
}

if (!fs.existsSync(statusTextDir)) {
  mcpwarpNotes.push("fixtures/mcpwarp/status-text/ missing, skipped");
} else {
  // MCPWARP.md §6A.9: the seven multi-service keys sit beside the <state>.<code> pairings.
  const SERVICE_TEXT_KEYS = GROUP_SERVICE_STATES.map((s) => `service.${s}`);
  const NO_SERVICE_KEY = "lastError.NO_SERVICE_REGISTERED";
  const MULTI_TEXT_KEYS = [...SERVICE_TEXT_KEYS, NO_SERVICE_KEY];
  const legalKeys = [...Object.entries(TUNNEL_PAIRS).flatMap(([state, codes]) => codes.map((c) => `${state}.${c ?? "none"}`)), ...MULTI_TEXT_KEYS];
  const isSpecialKey = (k) => k.startsWith("_") || k.startsWith("$");
  for (const required of ["rules.json", "cases.json", "en.json"]) {
    check(fs.existsSync(path.join(statusTextDir, required)), `fixtures/mcpwarp/status-text/${required} is missing`);
  }
  const tables = {};
  for (const file of listJsonFiles(statusTextDir)) {
    const data = readJsonChecked(file);
    mcpwarpStatusTextCount++;
    if (data === undefined) continue;
    const base = path.basename(file, ".json");
    if (base === "rules" || base === "cases") continue;
    tables[base] = { data, rel: path.relative(SPEC_DIR, file) };
  }

  if (tables.en) {
    const enKeys = Object.keys(tables.en.data).sort();
    for (const k of enKeys) {
      check(isSpecialKey(k) || legalKeys.includes(k), `${tables.en.rel}: key "${k}" is not a legal <state>.<code|none> pairing or a multi-service key`);
    }
    for (const k of legalKeys) check(enKeys.includes(k), `${tables.en.rel}: legal pairing "${k}" has no entry`);
    for (const { data, rel } of Object.values(tables)) {
      const keys = Object.keys(data).sort();
      check(JSON.stringify(keys) === JSON.stringify(enKeys), `${rel}: key set differs from en.json (missing: ${enKeys.filter((k) => !keys.includes(k)).join(", ") || "-"}; extra: ${keys.filter((k) => !enKeys.includes(k)).join(", ") || "-"})`);
      for (const [k, v] of Object.entries(data)) {
        if (isSpecialKey(k)) continue;
        if (!isPlainObject(v) || Object.keys(v).some((x) => x !== "label" && x !== "detail")) {
          check(false, `${rel}: "${k}" must be {label, detail?}`);
          continue;
        }
        checkStatusString(v.label, `${rel}: "${k}".label`);
        if (SERVICE_TEXT_KEYS.includes(k)) check(!("detail" in v), `${rel}: "${k}" is {label} only`);
        if (k === NO_SERVICE_KEY) check("detail" in v, `${rel}: "${k}" needs a detail`);
        if ("detail" in v) {
          checkStatusString(v.detail, `${rel}: "${k}".detail`);
          check(v.detail !== v.label, `${rel}: "${k}".detail equals its label and must be left out`);
        }
      }
    }
  }

  // Per locale: states[state] = the one label of every <state>.* key; codes[code] =
  // the one sentence (detail ?? label) of every *.<code> key (connecting.INTERNAL
  // and error.INTERNAL share it).
  const derived = {};
  for (const [locale, { data, rel }] of Object.entries(tables)) {
    const states = {};
    const codes = {};
    for (const [k, v] of Object.entries(data)) {
      if (isSpecialKey(k) || !isPlainObject(v) || MULTI_TEXT_KEYS.includes(k)) continue;
      const [state, code] = k.split(".");
      if (state in states) check(states[state] === v.label, `${rel}: every ${state}.* key needs the same label; "${k}" has ${JSON.stringify(v.label)}, another has ${JSON.stringify(states[state])}`);
      else states[state] = v.label;
      if (code === "none") continue;
      const sentence = v.detail ?? v.label;
      if (code in codes) check(codes[code] === sentence, `${rel}: every *.${code} key needs the same sentence; "${k}" has ${JSON.stringify(sentence)}, another has ${JSON.stringify(codes[code])}`);
      else codes[code] = sentence;
    }
    derived[locale] = { states, codes, data };
  }

  const rulesPath = path.join(statusTextDir, "rules.json");
  const rules = fs.existsSync(rulesPath) ? readJsonChecked(rulesPath) : undefined;
  if (rules !== undefined && Array.isArray(rules.locales)) {
    const files = Object.keys(tables).sort();
    check(JSON.stringify([...rules.locales].sort()) === JSON.stringify(files), `fixtures/mcpwarp/status-text/rules.json: locales ${JSON.stringify(rules.locales)} do not match the table files ${JSON.stringify(files)}`);
  }

  // describeStatus per rules.json (localeResolution, composition, invalidConfigComposition).
  function resolveLocale(locale) {
    const order = rules.localeResolution.order;
    if (typeof locale !== "string") return "en";
    const tag = locale.trim().replace(/_/g, "-").toLowerCase();
    const exact = order.find((l) => l.toLowerCase() === tag);
    if (exact) return exact;
    const lang = tag.split("-")[0];
    return order.find((l) => l.toLowerCase().split("-")[0] === lang) ?? "en";
  }
  function describeStatus(status, locale) {
    const l = resolveLocale(locale);
    const { states, codes } = derived[l];
    const label = typeof status.state === "string" && Object.hasOwn(states, status.state) ? states[status.state] : String(status.state);
    const sentence = typeof status.code === "string" && Object.hasOwn(codes, status.code) ? codes[status.code] : undefined;
    const le = status.lastError;
    let detail;
    if (sentence === undefined) detail = le;
    else if (status.code === "INVALID_CONFIG" && typeof le === "string" && le !== "") {
      const rw = rules.lastErrorColonRewrite;
      if (le === rules.invalidConfigComposition.genericLastError) detail = sentence;
      else if (le.startsWith(sentence)) detail = le;
      else detail = sentence + rules.colonJoiner[l] + le.replace(new RegExp(rw.pattern, rw.flags), rw.replacement);
    } else if (status.state === "error" && status.code === "INTERNAL" && le === GROUP_FATAL_LAST_ERROR && isPlainObject(derived[l].data[NO_SERVICE_KEY])) {
      detail = derived[l].data[NO_SERVICE_KEY].detail; // §6A.9
    } else detail = sentence;
    return typeof detail === "string" && detail !== "" && detail !== label ? { label, detail } : { label };
  }
  // §6A.9 describeServiceStatus: the service.<state> label; a detail only for rejected (by code) and a
  // dashboard-disabled service; lastError never shown.
  function describeServiceStatus(service, locale) {
    const { data } = derived[resolveLocale(locale)];
    const text = (k, field) => (isPlainObject(data[k]) ? data[k][field] : undefined);
    const label = typeof service.state === "string" && isPlainObject(data[`service.${service.state}`]) ? data[`service.${service.state}`].label : String(service.state);
    let detail;
    if (service.state === "rejected") {
      if (["INVALID_NAME", "USERNAME_REQUIRED", "QUOTA_EXCEEDED", "CONFLICT"].includes(service.code)) detail = text(`error.${service.code}`, "detail");
      else if (service.code === "UNAUTHENTICATED" || service.code === "FORBIDDEN") detail = text("error.UNAUTHORIZED", "detail");
      else detail = text("error.INTERNAL", "detail");
    } else if (service.state === "disabled" && service.code === "DISABLED") detail = text("disabled.DISABLED", "label");
    return typeof detail === "string" && detail !== "" && detail !== label ? { label, detail } : { label };
  }
  const canDescribe =
    isPlainObject(rules) &&
    isPlainObject(rules.localeResolution) &&
    Array.isArray(rules.localeResolution.order) &&
    rules.localeResolution.order.every((l) => l in derived) &&
    isPlainObject(rules.colonJoiner) &&
    isPlainObject(rules.lastErrorColonRewrite) &&
    isPlainObject(rules.invalidConfigComposition);
  check(canDescribe, "fixtures/mcpwarp/status-text/rules.json: needs localeResolution.order (every locale with a table), colonJoiner, lastErrorColonRewrite and invalidConfigComposition");
  if (canDescribe) {
    for (const [locale, { data, rel }] of Object.entries(tables)) {
      for (const [k, v] of Object.entries(data)) {
        if (isSpecialKey(k)) continue;
        if (SERVICE_TEXT_KEYS.includes(k)) {
          const got = describeServiceStatus({ state: k.slice("service.".length) }, locale);
          check(got.label === v.label, `${rel}: "${k}".label is ${JSON.stringify(v.label)} but describeServiceStatus({state}) labels it ${JSON.stringify(got.label)}`);
          continue;
        }
        if (k === NO_SERVICE_KEY) {
          check(v.label === derived[locale].states.error, `${rel}: "${k}".label must be the locale's error label ${JSON.stringify(derived[locale].states.error)}`);
          continue;
        }
        const [state, code] = k.split(".");
        const got = describeStatus(code === "none" ? { state } : { state, code }, locale);
        check(sameJson(got, v), `${rel}: "${k}" is ${JSON.stringify(v)} but describeStatus({state, code}) gives ${JSON.stringify(got)}`);
      }
    }
  }

  const casesPath = path.join(statusTextDir, "cases.json");
  if (fs.existsSync(casesPath)) {
    const cases = readJsonChecked(casesPath);
    const relCases = path.relative(SPEC_DIR, casesPath);
    if (cases !== undefined) {
      check(isPlainObject(cases) && Array.isArray(cases.cases) && cases.cases.length > 0, `${relCases}: needs a non-empty "cases" array`);
      (Array.isArray(cases.cases) ? cases.cases : []).forEach((c, i) => {
        // input.status -> describeStatus; input.service (a ServiceStatus) -> describeServiceStatus (§6A.9).
        const ok =
          isPlainObject(c) &&
          typeof c.description === "string" &&
          isPlainObject(c.input) &&
          isPlainObject(c.input.status) !== isPlainObject(c.input.service) &&
          isPlainObject(c.expect) &&
          typeof c.expect.label === "string" &&
          (c.expect.detail === undefined || typeof c.expect.detail === "string");
        check(ok, `${relCases}#cases[${i}]: needs description, one of input.status / input.service (object) and expect {label, detail?}`);
        if (ok && canDescribe) {
          const locale = c.input.locale ?? undefined;
          const got = c.input.service ? describeServiceStatus(c.input.service, locale) : describeStatus(c.input.status, locale);
          check(sameJson(got, c.expect), `${relCases}#cases[${i}]: expect ${JSON.stringify(c.expect)} but rules.json + the locale tables give ${JSON.stringify(got)}`);
        }
      });
    }
  }
}

const mcpwarpTotal = countJsonFilesRecursive(mcpwarpDir);

// ---------------------------------------------------------------------------
// 5. Checked-in total-count floor (finding 12).
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
// A separate floor so an added control fixture cannot mask a deleted mcpwarp one.
if (counts.minimum_mcpwarp_fixtures !== undefined) {
  check(mcpwarpTotal >= counts.minimum_mcpwarp_fixtures, `mcpwarp fixture count dropped: ${mcpwarpTotal} < checked-in floor ${counts.minimum_mcpwarp_fixtures} (spec/fixtures/COUNTS.json)`);
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

console.log(`control fixtures: ${controlSchemaValidCount} schema_valid, ${controlSchemaInvalidCount} schema_invalid (${controlWireValidCount} wire_valid), across ${controlTypes.length} types`);
console.log(`frame fixtures:   ${frameValidCount} valid, ${frameInvalidCount} invalid`);
console.log(`sequence fixtures: ${sequenceCount}`);
console.log(`mcpwarp envelope: ${mcpwarpEnvelopeValid} schema_valid, ${mcpwarpEnvelopeInvalid} schema_invalid (${mcpwarpEnvelopeWireValid} wire_valid), across ${MCPWARP_OPS.length} ops`);
console.log(`mcpwarp bridging: ${mcpwarpBridgingCount}, tunnel: ${mcpwarpTunnelCount}, status-text files: ${mcpwarpStatusTextCount}`);
for (const note of mcpwarpNotes) console.log(`  note: ${note}`);
console.log(`mcpwarp fixture files: ${mcpwarpTotal} (floor ${counts.minimum_mcpwarp_fixtures ?? "-"})`);
console.log(`total fixture files: ${actualTotal} (floor ${counts.minimum_total_fixtures})`);
console.log(`\n${pass}/${pass + fail} checks passed`);

if (failures.length) {
  console.log("\nFAILURES:");
  for (const f of failures) console.log(`  - ${f}`);
  process.exit(1);
}
