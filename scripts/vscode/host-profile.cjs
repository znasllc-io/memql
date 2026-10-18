const fs = require("node:fs");
const path = require("node:path");

// Cockpit's TMPDIR is deliberately nested in its owned checkout. Electron
// puts a Unix socket below user-data, so that scratch path can exceed macOS's
// 103-byte limit. Keep the private profile directly under the same checkout:
// normal cleanup removes it, and the runner still owns it after cancellation.
function createHostProfile(checkout) {
  const root = fs.mkdtempSync(path.join(checkout, ".host-"));
  const userData = path.join(root, "u");
  const extensions = path.join(root, "e");
  const dispose = () => fs.rmSync(root, { recursive: true, force: true });
  try {
    if (process.platform !== "win32" && Buffer.byteLength(path.join(userData, "1.999-main.sock")) > 103) {
      throw new Error("The editor test checkout path is too long for a Unix socket; use a shorter checkout directory.");
    }
    fs.mkdirSync(userData, { mode: 0o700 });
    fs.mkdirSync(extensions, { mode: 0o700 });
    return { root, userData, extensions, dispose };
  } catch (error) {
    dispose();
    throw error;
  }
}

module.exports = { createHostProfile };
