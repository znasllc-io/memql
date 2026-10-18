// Keep download failures outside the extension-test process. test-electron can
// reject a second stream promise after ECONNRESET and abort Node before its own
// retry completes. Only download transport failures are retried, never tests.
const { fork } = require("node:child_process");
const path = require("node:path");
const { setTimeout: delay } = require("node:timers/promises");

const TRANSIENT = /\b(?:ECONNRESET|ETIMEDOUT|ECONNREFUSED|EAI_AGAIN|ENETUNREACH|EHOSTUNREACH)\b/;

function attempt(options, worker, timeoutMs) {
  return new Promise((resolve, reject) => {
    const child = fork(worker, [JSON.stringify(options)], {
      stdio: ["ignore", "inherit", "pipe", "ipc"],
    });
    let receipt;
    let tail = "";
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill("SIGKILL");
    }, timeoutMs);
    child.stderr.on("data", chunk => {
      process.stderr.write(chunk);
      tail = (tail + chunk.toString()).slice(-8192);
    });
    child.on("message", message => { receipt = message; });
    child.once("error", error => { clearTimeout(timer); reject(error); });
    child.once("close", (code, signal) => {
      clearTimeout(timer);
      if (code === 0 && typeof receipt?.executable === "string" && receipt.executable) {
        resolve(receipt.executable);
        return;
      }
      const error = new Error(timedOut ? "VS Code download timed out" :
        `VS Code download failed (${signal || code}): ${receipt?.error || tail || "no executable receipt"}`);
      error.retryable = timedOut || TRANSIENT.test(tail + (receipt?.error || ""));
      reject(error);
    });
  });
}

async function downloadVSCode(options, settings = {}) {
  const worker = settings.worker || __filename;
  const timeoutMs = settings.timeoutMs ?? 180000;
  for (let number = 1; number <= 3; number++) {
    try {
      return await attempt(options, worker, timeoutMs);
    } catch (error) {
      if (!error.retryable || number === 3) throw error;
      console.error(`VS Code download interrupted; retrying download (${number + 1}/3).`);
      await delay(settings.retryDelayMs ?? number * 1000);
    }
  }
}

if (require.main === module) {
  const { downloadAndUnzipVSCode } = require(path.resolve(__dirname,
    "../../editors/vscode/node_modules/@vscode/test-electron"));
  downloadAndUnzipVSCode(JSON.parse(process.argv[2])).then(executable => {
    process.send({ executable }, () => process.disconnect());
  }).catch(error => {
    console.error(error);
    process.send({ error: `${error.code || ""} ${error.message}` }, () => process.exit(1));
  });
}

module.exports = { downloadVSCode };
