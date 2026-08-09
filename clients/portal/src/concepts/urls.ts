// The concept browser's addresses.
//
// EVERY destination in the browser is a URL, not a piece of component state:
// which concept you are looking at, which row is open, what you searched for,
// which domain you narrowed to. That is a hard requirement of memql#3316 --
// "a concept, and a row within it, should each have an address someone can
// paste into chat" -- and it is also the cheaper design, because a selection
// held in state has to be lifted, serialized and parsed later anyway.
//
// ===========================================================================
// THE ENCODING DECISION. Read this before "simplifying" encodeSegment.
// ===========================================================================
//
// memQL ids are colon-delimited -- `<version>:<domain>:<entity>` names a
// concept, and a row id may be equally punctuated. A colon is a perfectly
// legal `pchar` in a URL path segment (RFC 3986 §3.3), so it does NOT have to
// be escaped -- and escaping it turns every id in the address bar into a
// `%3A` thicket, which is the kind of link someone pastes into chat and
// someone else fails to recognise as a concept.
//
// So encodeSegment runs encodeURIComponent (which escapes everything a path
// segment cannot carry -- `/`, `?`, `#`, `%`, spaces) and then puts the
// colons BACK. The round trip is still exact, and that is the property that
// matters:
//
//   "q1:a:b"   -> "q1:a:b"     -> decodeURIComponent -> "q1:a:b"    (readable)
//   "a/b"      -> "a%2Fb"      -> decodeURIComponent -> "a/b"       (one segment)
//   "a%3Ab"    -> "a%253Ab"    -> decodeURIComponent -> "a%3Ab"     (not confused
//                                                                    with a colon)
//   "a b"      -> "a%20b"      -> decodeURIComponent -> "a b"
//
// The third line is why the restoration is a replace of the ESCAPE SEQUENCE
// rather than a character-class exclusion: an id that genuinely contains the
// three characters `%3A` must survive, and it does, because encodeURIComponent
// has already turned its `%` into `%25` before we look for `%3A`.
//
// The router decodes params for us (react-router runs decodeURIComponent on
// every path parameter), so nothing downstream has to know this happened.
// test/conceptUrls.test.ts pins the whole table, including the paste-back
// round trip through a real router.

// encodeSegment escapes a memQL id for use as ONE path segment. See above.
export function encodeSegment(value: string): string {
  return encodeURIComponent(value).replace(/%3A/g, ":");
}

// decodeSegment is the inverse. Callers reading a param out of react-router do
// NOT need it (the router already decoded), but a test proving the round trip
// does, and so would any code parsing a location by hand.
export function decodeSegment(value: string): string {
  try {
    return decodeURIComponent(value);
  } catch {
    // A malformed escape ("%zz") throws. A pasted-and-mangled link is a
    // realistic way to get here, and showing the raw text beats a blank
    // screen from an uncaught URIError.
    return value;
  }
}

export const CONCEPTS_ROOT = "/concepts";

// The registry's own filter state lives in the query string, so a filtered
// registry is itself a link ("here are the identity concepts, filtered to
// 'session'"). Named constants because three modules read them.
export const SEARCH_PARAM = "q";
export const DOMAIN_PARAM = "domain";

// withSearch appends a preserved query string. Navigation WITHIN the browser
// keeps the operator's filter -- clicking a concept out of a filtered registry
// and then clicking back must not silently drop the filter they typed.
function withSearch(path: string, search: string): string {
  if (!search) return path;
  return path + (search.startsWith("?") ? search : "?" + search);
}

export function conceptsPath(search = ""): string {
  return withSearch(CONCEPTS_ROOT, search);
}

export function conceptPath(conceptId: string, search = ""): string {
  return withSearch(`${CONCEPTS_ROOT}/${encodeSegment(conceptId)}`, search);
}

export function conceptSchemaPath(conceptId: string, search = ""): string {
  return withSearch(`${CONCEPTS_ROOT}/${encodeSegment(conceptId)}/schema`, search);
}

export function conceptRowPath(conceptId: string, rowId: string, search = ""): string {
  return withSearch(
    `${CONCEPTS_ROOT}/${encodeSegment(conceptId)}/rows/${encodeSegment(rowId)}`,
    search,
  );
}

// The route PATTERNS, so the route table and the builders above cannot drift
// apart. routes.tsx declares the routes from these; ConceptPage asks
// useMatch about SCHEMA_ROUTE_PATTERN to decide which tab is lit. A literal
// in any of those three places is a literal that can be edited alone.
export const CONCEPTS_ROUTE_PATTERN = "concepts";
export const CONCEPT_ROUTE_PATTERN = "concepts/:conceptId";
export const CONCEPT_ROW_CHILD_PATTERN = "rows/:rowId";
export const CONCEPT_SCHEMA_CHILD_PATTERN = "schema";
// Absolute form, for useMatch (which matches against the location, not
// against a child route's relative path).
export const SCHEMA_ROUTE_PATTERN = `/${CONCEPT_ROUTE_PATTERN}/${CONCEPT_SCHEMA_CHILD_PATTERN}`;
