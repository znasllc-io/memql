/*
 * The Files download worker (design D13, epic memql#4721).
 *
 * One job: serve a synthesized in-scope URL (`__memql-dl/<id>`) as a
 * navigation response whose body is fed, chunk by chunk, over a
 * MessageChannel by the page that fetched the bytes. The browser writes the
 * response to disk as it arrives, so a download's memory cost is one chunk,
 * not one file.
 *
 * THE WORKER NEVER SEES A CREDENTIAL. The page fetches the content route
 * with its own bearer and hands over bytes only; this file holds streams,
 * names and sizes, and nothing else. Registered with scope `__memql-dl/`, so
 * no other request on the site ever passes through here.
 *
 * AND IT CHECKS WHO IS TALKING TO IT. The message handler verifies the
 * sender's origin against this worker's own before it does anything. Service
 * workers are same-origin by construction, so this is defense in depth -- but
 * a handler that never looks is one nobody can audit by reading it.
 *
 * Plain JS on purpose: this file is copied verbatim into the bundle root
 * (vite `public/`), outside the module graph and the typechecker. Keep it
 * small enough to review by reading.
 */

"use strict";

var downloads = new Map();
var counter = 0;

self.addEventListener("install", function () {
  self.skipWaiting();
});

self.addEventListener("activate", function (event) {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("message", function (event) {
  // WHO SENT THIS. A service worker is same-origin by construction -- no
  // cross-origin page can obtain this registration, and none can reach this
  // handler -- so the check below is defense in depth rather than the thing
  // standing between a stranger and the worker. It is worth the two lines
  // anyway: a postMessage handler that never looks at its sender is one
  // nobody can audit by reading it, which is exactly what CodeQL's
  // js/missing-origin-check objects to (alert #1035).
  //
  // The origin comparison tolerates an EMPTY origin rather than refusing it.
  // The spec populates `origin` for a message from a client, but a download
  // that silently stopped working in some embedding would be a worse bug than
  // the one this guards against, and an empty origin cannot in any case name
  // a cross-origin sender that the service worker security model would have
  // let through.
  if (event.origin && event.origin !== self.location.origin) return;
  // The belt to that brace, and the stronger of the two: the sender is a
  // Client, and its url is the page it is running. A client from another
  // origin cannot be one of ours.
  var source = event.source;
  if (source && typeof source.url === "string" && source.url !== "") {
    var senderOrigin;
    try {
      senderOrigin = new URL(source.url).origin;
    } catch (err) {
      return;
    }
    if (senderOrigin !== self.location.origin) return;
  }

  var data = event.data || {};
  if (data.type !== "memql-download-open" || !event.ports || !event.ports[0]) return;
  var port = event.ports[0];
  counter += 1;
  var id = Date.now().toString(36) + "-" + counter;
  var name = (typeof data.name === "string" ? data.name : "").replace(/[\\/\u0000-\u001f\u007f]/g, "_").trim() || "download";
  // Give Safari a filename in the navigation URL as well as both disposition
  // parameters. An extensionless iframe navigation can otherwise become .html.
  var url = new URL(id + "/" + encodeURIComponent(name), self.registration.scope).href;

  var stream = new ReadableStream({
    start: function (controller) {
      port.onmessage = function (msg) {
        var m = msg.data || {};
        if (m.type === "chunk" && m.chunk) {
          controller.enqueue(m.chunk);
        } else if (m.type === "done") {
          controller.close();
          // A small response can finish before the iframe navigates. Keep
          // the closed stream until fetch claims it (or the expiry below).
        } else if (m.type === "abort") {
          controller.error(new Error("download aborted"));
          downloads.delete(id);
        }
      };
    },
    cancel: function () {
      downloads.delete(id);
    },
  });

  downloads.set(id, {
    stream: stream,
    name: name,
    mimeType: typeof data.mimeType === "string" && !/[\r\n]/.test(data.mimeType) ? data.mimeType : "application/octet-stream",
    size: typeof data.size === "number" && data.size > 0 ? data.size : 0,
  });

  // An entry nobody navigates to would hold its stream forever; give the
  // page a minute to open the URL it asked for.
  setTimeout(function () {
    var entry = downloads.get(id);
    if (entry && !entry.claimed) downloads.delete(id);
  }, 60000);

  port.postMessage({ type: "ready", url: url });
});

self.addEventListener("fetch", function (event) {
  var path = new URL(event.request.url).pathname;
  var marker = path.lastIndexOf("__memql-dl/");
  if (marker === -1) return;
  var id = path.slice(marker + "__memql-dl/".length).split("/")[0];
  var entry = downloads.get(id);
  if (!entry) return;
  // The Response keeps the stream alive. Removing only this lookup makes
  // the URL single-use without discarding bytes before the first request.
  downloads.delete(id);

  var headers = {
    "Content-Type": entry.mimeType || "application/octet-stream",
    "X-Content-Type-Options": "nosniff",
    "Content-Disposition":
      'attachment; filename="' + entry.name.replace(/[^\x20-\x7e]|["\\]/g, "_") +
      '"; filename*=UTF-8\'\'' + encodeURIComponent(entry.name).replace(/['()]/g, escape),
  };
  if (entry.size > 0) headers["Content-Length"] = String(entry.size);
  event.respondWith(new Response(entry.stream, { headers: headers }));
});
