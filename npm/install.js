// Fetch the prebuilt binary for this machine.
//
// sieve is a Go program and its users are Claude Code users, who have node and
// mostly do not have a Go toolchain. Asking them to install one to try a plugin
// is asking most of them not to try it. So npm carries the wrapper and this
// pulls the binary that matches the platform.
//
// Everything here fails loudly. A postinstall that swallows an error leaves a
// package that looks installed and does nothing, which is the worst of the
// available outcomes: the plugin will appear to be broken rather than absent.

const fs = require("fs");
const os = require("os");
const path = require("path");
const https = require("https");
const { execFileSync } = require("child_process");

const pkg = require("./package.json");
const VERSION = pkg.version;
const REPO = "qcoderx/sieve";

// These names are a contract with .github/workflows/release.yml. Changing one
// without the other breaks installation on the platform that was renamed.
const TARGETS = {
  "darwin-arm64": "darwin_arm64",
  "darwin-x64": "darwin_amd64",
  "linux-x64": "linux_amd64",
  "linux-arm64": "linux_arm64",
  "win32-x64": "windows_amd64",
  "win32-arm64": "windows_arm64",
};

function fail(message, hint) {
  console.error("\nsieve: " + message);
  if (hint) console.error(hint);
  console.error(
    "\nYou can also install it directly:\n" +
      "  go install github.com/qcoderx/sieve/cmd/sieve@latest\n" +
      "or download a binary from https://github.com/" + REPO + "/releases\n"
  );
  process.exit(1);
}

function get(url, redirectsLeft = 5) {
  return new Promise((resolve, reject) => {
    https
      .get(url, { headers: { "User-Agent": "sieve-npm-installer" } }, (res) => {
        // GitHub release assets redirect to a storage host.
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          if (redirectsLeft === 0) return reject(new Error("too many redirects"));
          res.resume();
          return resolve(get(res.headers.location, redirectsLeft - 1));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error("HTTP " + res.statusCode + " for " + url));
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
      })
      .on("error", reject);
  });
}

async function main() {
  const key = process.platform + "-" + process.arch;
  const target = TARGETS[key];
  if (!target) {
    fail(
      "no prebuilt binary for " + key + ".",
      "sieve builds for macOS, Linux and Windows on x64 and arm64."
    );
  }

  const isWindows = process.platform === "win32";
  const archive =
    "sieve_" + VERSION + "_" + target + (isWindows ? ".zip" : ".tar.gz");
  const base =
    "https://github.com/" + REPO + "/releases/download/v" + VERSION + "/";

  const binDir = path.join(__dirname, "bin");
  fs.mkdirSync(binDir, { recursive: true });

  let blob, sums;
  try {
    console.log("sieve: downloading " + archive);
    blob = await get(base + archive);
    sums = (await get(base + "checksums.txt")).toString("utf8");
  } catch (err) {
    fail("could not download the binary: " + err.message,
      "Check the network, or that release v" + VERSION + " exists.");
  }

  // Verify before unpacking. An unverified binary that runs is worse than one
  // that does not install.
  const crypto = require("crypto");
  const got = crypto.createHash("sha256").update(blob).digest("hex");
  const line = sums.split("\n").find((l) => l.trim().endsWith(archive));
  if (!line) fail("no checksum published for " + archive + ".");
  const want = line.trim().split(/\s+/)[0];
  if (got !== want) {
    fail("checksum mismatch for " + archive + ".",
      "  expected " + want + "\n  got      " + got);
  }

  const archivePath = path.join(binDir, archive);
  fs.writeFileSync(archivePath, blob);
  try {
    if (isWindows) {
      execFileSync("powershell", [
        "-NoProfile", "-NonInteractive", "-Command",
        "Expand-Archive -Force -LiteralPath '" + archivePath + "' -DestinationPath '" + binDir + "'",
      ], { stdio: "inherit" });
    } else {
      execFileSync("tar", ["xzf", archivePath, "-C", binDir], { stdio: "inherit" });
    }
  } catch (err) {
    fail("could not unpack " + archive + ": " + err.message);
  }
  fs.unlinkSync(archivePath);

  const bin = path.join(binDir, isWindows ? "sieve.exe" : "sieve");
  if (!fs.existsSync(bin)) fail("the archive did not contain a sieve binary.");
  if (!isWindows) fs.chmodSync(bin, 0o755);

  // Prove it runs now, rather than the first time a hook fires mid-session.
  try {
    const out = execFileSync(bin, ["version"], { encoding: "utf8" }).trim();
    console.log("sieve: installed " + out);
  } catch (err) {
    fail("the downloaded binary would not run: " + err.message);
  }
}

main().catch((err) => fail(String(err && err.message ? err.message : err)));
