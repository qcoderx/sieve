#!/usr/bin/env node
// Hand every argument to the real binary and return its exit code unchanged.
//
// The exit code matters more than usual here: sieve uses distinct codes for a
// refusal by policy, an unreachable host and a hang, and a wrapper that
// flattened them would take that away from every caller installed through npm.

const path = require("path");
const fs = require("fs");
const { spawnSync } = require("child_process");

const bin = path.join(
  __dirname,
  process.platform === "win32" ? "sieve.exe" : "sieve"
);
if (!fs.existsSync(bin)) {
  console.error(
    "sieve: the binary is missing. Reinstall with `npm install -g @qcoderx/sieve`,\n" +
      "or download it from https://github.com/qcoderx/sieve/releases"
  );
  process.exit(1);
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error("sieve: " + res.error.message);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
