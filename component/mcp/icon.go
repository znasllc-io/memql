package mcp

import (
	_ "embed"
	"encoding/base64"
)

// serverIconSVG is the memQL square mark (the node-graph glyph from
// assets/logo.svg on a brand-blue tile), embedded so the MCP server can
// advertise a branded connector icon without a separate static endpoint.
//
//go:embed icon.svg
var serverIconSVG []byte

// serverIconDataURI is serverIconSVG as an RFC 2397 data: URI, advertised in
// the initialize serverInfo.icons (MCP Implementation.icons) so clients such
// as Claude render a proper icon for the memQL connector instead of a
// placeholder. The MCP schema permits a data: URI for an icon src.
var serverIconDataURI = "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(serverIconSVG)
