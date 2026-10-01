package edge

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

// Compression belongs only to public bundle files. Runtime configuration,
// authenticated proxies, and private previews never pass this decision.
func gzipFile(w http.ResponseWriter, r *http.Request, name string) bool {
	if alreadyPrivate(w) || w.Header().Get("Content-Encoding") != "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm", ".js", ".mjs", ".css", ".json", ".map", ".xml", ".txt", ".svg", ".webmanifest":
	default:
		return false
	}
	// Identity responses vary too: otherwise a shared cache can reuse them
	// for a gzip client, or reuse compressed bytes for one refusing gzip.
	w.Header().Add("Vary", "Accept-Encoding")
	// Byte ranges retain the identity representation and its strong ETag.
	// If-Range with a gzip ETag cannot match it and gets a full response.
	return r.Header.Get("Range") == "" && acceptsGzip(r.Header.Get("Accept-Encoding"))
}

func acceptsGzip(header string) bool {
	explicit, wildcard := -1.0, 0.0
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(value, ";")
		coding := strings.ToLower(strings.TrimSpace(parts[0]))
		if coding != "gzip" && coding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(key, "q") {
				quality = 0
				break
			}
			q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || !(q >= 0 && q <= 1) {
				quality = 0
				break
			}
			quality = q
		}
		if coding == "gzip" {
			explicit = quality
		} else {
			wildcard = quality
		}
	}
	if explicit >= 0 {
		return explicit > 0
	}
	return wildcard > 0
}

func serveGzipFile(w http.ResponseWriter, r *http.Request, name string, data []byte) {
	var encoded bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&encoded, gzip.BestSpeed)
	_, _ = writer.Write(data) // bytes.Buffer cannot fail.
	_ = writer.Close()
	w.Header().Set("Content-Type", bundleFileContentType(name, data))
	http.ServeContent(gzipLengthWriter{ResponseWriter: w, size: encoded.Len()}, r, path.Base(name), time.Time{}, bytes.NewReader(encoded.Bytes()))
}

// ServeContent leaves Content-Length to a caller setting Content-Encoding.
// Set it only once preconditions succeed; a 412 must not advertise a gzip
// body that was never written, or clients wait for bytes that will not come.
type gzipLengthWriter struct {
	http.ResponseWriter
	size int
}

func (w gzipLengthWriter) WriteHeader(status int) {
	if status == http.StatusOK {
		w.Header().Set("Content-Length", strconv.Itoa(w.size))
	} else {
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
	}
	w.ResponseWriter.WriteHeader(status)
}
