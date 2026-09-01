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
  var data = event.data || {};
  if (data.type !== "memql-download-open" || !event.ports || !event.ports[0]) return;
  var port = event.ports[0];
  counter += 1;
  var id = Date.now().toString(36) + "-" + counter;
  var url = new URL("__memql-dl/" + id, self.registration.scope).href;

  var stream = new ReadableStream({
    start: function (controller) {
      port.onmessage = function (msg) {
        var m = msg.data || {};
        if (m.type === "chunk" && m.chunk) {
          controller.enqueue(m.chunk);
        } else if (m.type === "done") {
          controller.close();
          downloads.delete(id);
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
    name: typeof data.name === "string" && data.name !== "" ? data.name : "download",
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
  var id = path.slice(marker + "__memql-dl/".length);
  var entry = downloads.get(id);
  if (!entry) return;
  entry.claimed = true;

  var headers = {
    "Content-Type": "application/octet-stream",
    "Content-Disposition":
      "attachment; filename*=UTF-8''" + encodeURIComponent(entry.name).replace(/['()]/g, escape),
  };
  if (entry.size > 0) headers["Content-Length"] = String(entry.size);
  event.respondWith(new Response(entry.stream, { headers: headers }));
});
