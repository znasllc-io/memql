// Keep transport retries outside the extension-test process. A slow download
// must keep its progress; only a stalled transfer or transport failure retries.
const { fork } = require("node:child_process");
const path = require("node:path");
const { setTimeout: delay } = require("node:timers/promises");

// test-electron's request timer can reject without a Node transport error code,
// including through an unhandled rejection. Its timeout still retries only the
// downloader process, under the same attempt count and shared time budget.
const TRANSIENT = /\b(?:ECONNRESET|ETIMEDOUT|ECONNREFUSED|EAI_AGAIN|ENETUNREACH|EHOSTUNREACH)\b|@vscode\/test-electron request timeout/;
const TIME_LIMIT = "VS Code download exceeded its total time limit";

// The pipeline reads exit 75 (EX_TEMPFAIL) as a request to retry the step,
// within the step's retry limits. Only a failure carrying this mark, set as it
// leaves downloadVSCode, can make that request.
const RETRY_STEP = 75;
const DOWNLOAD_FAILURE = Symbol("VS Code download failure");

function attempt(options, worker, idleMs, remainingMs) {
  return new Promise((resolve, reject) => {
    const child = fork(worker, [JSON.stringify(options)], {
      stdio: ["ignore", "inherit", "pipe", "ipc"],
    });
    let receipt;
    let tail = "";
    let timeoutReason = "";
    let bytes = 0;
    let internalRetries = 0;
    const stages = new Set();
    const stop = reason => { timeoutReason = reason; child.kill("SIGKILL"); };
    const idle = setTimeout(() => stop("VS Code download stalled"), idleMs);
    const deadline = setTimeout(() => stop(TIME_LIMIT), remainingMs);
    const clearTimers = () => { clearTimeout(idle); clearTimeout(deadline); };
    child.stderr.on("data", chunk => {
      process.stderr.write(chunk);
      tail = (tail + chunk.toString()).slice(-8192);
    });
    child.on("message", message => {
      if (message?.progress) {
        const progress = message.progress;
        // A heartbeat or repeated byte count is not forward progress. Each
        // non-transfer stage can extend the idle watchdog only once.
        if (progress.stage === "retrying" && internalRetries < 2) {
          // The pinned upstream downloader has three internal attempts. A new
          // transfer starts its counter over, under the same absolute budget.
          internalRetries++;
          bytes = 0;
          idle.refresh();
        } else if (Number.isSafeInteger(progress.bytes) && progress.bytes > bytes) {
          bytes = progress.bytes;
          idle.refresh();
        } else if (["resolvedVersion", "extractingSynchonrously", "newInstallComplete", "foundMatchingInstall"].includes(progress.stage) && !stages.has(progress.stage)) {
          stages.add(progress.stage);
          idle.refresh();
        }
      } else if (typeof message?.executable === "string") {
        receipt = message;
      }
    });
    child.once("error", error => { clearTimers(); reject(error); });
    child.once("close", (code, signal) => {
      clearTimers();
      if (!timeoutReason && code === 0 && typeof receipt?.executable === "string" && receipt.executable) {
        resolve(receipt.executable);
        return;
      }
      const error = new Error(timeoutReason ||
        `VS Code download failed (${signal || code}): ${tail || "no executable receipt"}`);
      error.retryable = timeoutReason === "VS Code download stalled" || TRANSIENT.test(tail);
      reject(error);
    });
  });
}

async function download(options, settings) {
  const worker = settings.worker || __filename;
  // The 302 MB macOS archive can make progress for over ten minutes. Give all
  // attempts one shared 20-minute budget, leaving time for the real host test.
  const idleMs = settings.timeoutMs ?? 120000;
  const deadline = performance.now() + (settings.totalTimeoutMs ?? 1200000);
  for (let number = 1; number <= 3; number++) {
    const remainingMs = deadline - performance.now();
    if (remainingMs <= 0) throw new Error(TIME_LIMIT);
    try {
      return await attempt(options, worker, idleMs, remainingMs);
    } catch (error) {
      const retryDelayMs = settings.retryDelayMs ?? number * 1000;
      if (!error.retryable || number === 3 || performance.now() + retryDelayMs >= deadline) throw error;
      console.error(`VS Code download interrupted; retrying download (${number + 1}/3).`);
      await delay(retryDelayMs);
    }
  }
}

async function downloadVSCode(options, settings = {}) {
  try {
    return await download(options, settings);
  } catch (error) {
    if (error !== null && typeof error === "object") error[DOWNLOAD_FAILURE] = true;
    throw error;
  }
}

// exitCodeFor is the status a runner exits with after a failure. A download
// that failed for a passing reason (a stalled transfer, the shared time limit
// or a transport failure) answers 75, so another attempt can succeed. A bad
// version, a missing receipt, a failed host test and anything thrown outside
// the download answer 1.
function exitCodeFor(error) {
  if (!error?.[DOWNLOAD_FAILURE]) return 1;
  return error.retryable === true || error.message === TIME_LIMIT ? RETRY_STEP : 1;
}

function safeError(error) {
  return `${error?.code || ""} ${error?.message || String(error)}`.trim().replace(/https?:\/\/\S+/g, "[download URL]");
}

if (require.main === module) {
  const { downloadAndUnzipVSCode } = require(path.resolve(__dirname,
    "../../editors/vscode/node_modules/@vscode/test-electron"));
  let lastSent = 0;
  let lastReported = 0;
  const reporter = {
    report(progress) {
      const now = performance.now();
      if (progress.stage !== "downloading" || now - lastSent >= 1000 || progress.bytesSoFar === progress.totalBytes) {
        lastSent = now;
        if (process.connected) process.send({ progress: { stage: progress.stage, bytes: progress.bytesSoFar } });
      }
      if (progress.stage === "downloading" && (now - lastReported >= 30000 || progress.bytesSoFar === progress.totalBytes)) {
        lastReported = now;
        console.error(`Downloading VS Code: ${(progress.bytesSoFar / 1048576).toFixed(1)} of ${(progress.totalBytes / 1048576).toFixed(1)} MiB`);
      }
    },
    error(error) { console.error(safeError(error)); },
  };
  downloadAndUnzipVSCode({ ...JSON.parse(process.argv[2]), reporter }).then(executable => {
    process.send({ executable }, () => process.disconnect());
  }).catch(error => {
    console.error(safeError(error));
    process.exit(1);
  });
}

module.exports = { downloadVSCode, exitCodeFor };
