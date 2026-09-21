package edge

import "net/http"

// shopper_page.go -- WHAT A SHOPPER SEES WHEN THE EDGE REFUSES (epic
// memql#5532, issue memql#5551).
//
// # Why the engine renders anything at all here
//
// It renders as LITTLE as it can get away with, and where it can do better
// it does not render at all. A form declares RedirectError, so the bff --
// which has resolved the declaration -- answers 303 to the merchant's own
// error page with a reason, and the shopper sees the merchant's design
// saying what happened. That is the good path and it is the common one.
//
// The edge cannot take it. It refuses BEFORE the declaration is resolved,
// on purpose: a rate limit that forwarded first would not be a rate limit,
// and a size cap that forwarded first would not be a cap. So for these few
// refusals there is no declared page to send anyone to, and the choice is
// between an engine-rendered page and http.Error's bare string.
//
// # The design, and why it is this one
//
// This page is an INTERLOPER. It appears on a merchant's own domain, in the
// middle of a storefront somebody designed carefully, to a member of the
// public who has never heard of MemQL. Anything with personality here is us
// putting our aesthetic on somebody else's site, so the design is
// deliberate self-effacement:
//
//   - NO PALETTE OF OUR OWN. `color-scheme: light dark` plus the Canvas and
//     CanvasText system colors means the page adopts the visitor's own
//     operating system in both themes, and MemQL chooses no colour at all.
//   - ANCHORED HIGH, not centred in the viewport. A short message centred
//     vertically is the error-page takeover; a note at the top of a
//     comfortable measure reads as a note.
//   - TWO TYPE SIZES, one hairline, no card, no accent, no shadow.
//   - NO BRAND, NO PRODUCT NAME, NO STORE NAME. The shopper is owed an
//     answer, not an introduction.
//
// # The reason code is a HEADER, not page furniture
//
// The shopper cannot act on "rate_limited" and the merchant cannot act
// without it. So the code goes on X-MemQL-Shopper-Refusal, where an
// operator reading a response or a log finds it, and the page stays a
// sentence and a link. Splitting them is what lets both be right.
//
// # There is no dynamic content on this page, and that is a security
// property rather than an economy
//
// Every string below is a constant. Nothing from the request, the site, the
// store or the form reaches the markup, so there is nothing to escape and
// no way to get the escaping wrong -- which is the strongest available
// answer on a page served on somebody else's origin.
// TestTheRefusalPageCarriesNothingFromTheRequest asserts it.

// shopperRefusal is one refusal the edge can answer.
type shopperRefusal struct {
	// code is for the operator, on a header. Never rendered.
	code string
	// status is what the browser is told.
	status int
	// message says what happened, and remedy says what to do about it.
	// Both in the interface's voice: no apology, nothing vague.
	message string
	remedy  string
}

var (
	shopperRefusalTooLarge = shopperRefusal{
		code:    "too_large",
		status:  http.StatusRequestEntityTooLarge,
		message: "That submission was too long to accept.",
		remedy:  "Shorten it and send it again.",
	}
	shopperRefusalRateLimited = shopperRefusal{
		code:    "rate_limited",
		status:  http.StatusTooManyRequests,
		message: "Too many submissions have come from this connection.",
		remedy:  "Wait a minute, then send it again.",
	}
	// ONE MESSAGE FOR SEVERAL CAUSES, deliberately. The surface being off,
	// the pack being disabled, the site having no owner to borrow and the
	// method being wrong are four different facts, and distinguishing them
	// here would tell somebody probing the origin which of them to work on.
	// The header carries the same single code for the same reason.
	shopperRefusalNotAvailable = shopperRefusal{
		code:    "not_available",
		status:  http.StatusNotFound,
		message: "This form is not accepting submissions.",
		remedy:  "Go back to the page you came from and try again later.",
	}
)

// shopperRefusalHeader names the refusal for whoever operates the site.
const shopperRefusalHeader = "X-Memql-Shopper-Refusal"

// writeShopperRefusal renders one refusal.
func writeShopperRefusal(w http.ResponseWriter, refusal shopperRefusal) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set(shopperRefusalHeader, refusal.code)
	// NOT INDEXABLE. This page is served on a merchant's own domain, and a
	// crawler that reached one must not put "not accepting submissions" into
	// a search result for their shop.
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Cache-Control", "no-store")
	// The page has no script, no image, no font and no form, so it declares
	// exactly that. style-src 'unsafe-inline' is the one allowance and it
	// carries no risk here: there is no dynamic content for it to reflect.
	h.Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	h.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(refusal.status)
	_, _ = w.Write([]byte(shopperRefusalDocument(refusal)))
}

func shopperRefusalDocument(refusal shopperRefusal) string {
	return `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex, nofollow">
<title>` + refusal.message + `</title>
<style>
  :root { color-scheme: light dark; }
  html { -webkit-text-size-adjust: 100%; }
  body {
    margin: 0;
    background: Canvas;
    color: CanvasText;
    font-family: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
    font-size: 100%;
  }
  main {
    /* Anchored high at a comfortable measure, not centred in the viewport:
       a short notice centred vertically reads as a page takeover. */
    max-width: 34rem;
    margin: 0 auto;
    padding: 4.5rem 1.5rem 3rem;
  }
  h1 {
    font-size: 1.3125rem;
    line-height: 1.4;
    font-weight: 600;
    letter-spacing: -0.006em;
    margin: 0 0 0.75rem;
  }
  p {
    font-size: 1rem;
    line-height: 1.6;
    margin: 0;
    /* The remedy is quieter than the fact, without becoming grey-on-grey:
       a relative alpha keeps the ratio in both themes. */
    color: color-mix(in srgb, CanvasText 78%, Canvas);
  }
  hr {
    border: 0;
    border-top: 1px solid color-mix(in srgb, CanvasText 18%, Canvas);
    margin: 2rem 0 1.25rem;
  }
  a {
    color: LinkText;
    text-decoration-thickness: 1px;
    text-underline-offset: 2px;
  }
  a:focus-visible {
    outline: 2px solid CanvasText;
    outline-offset: 3px;
    border-radius: 2px;
  }
  @media (max-width: 30rem) {
    main { padding-top: 2.5rem; }
  }
</style>
</head>
<body>
<main>
  <h1>` + refusal.message + `</h1>
  <p>` + refusal.remedy + `</p>
  <hr>
  <p><a href="/">Go to the home page</a></p>
</main>
</body>
</html>
`
}
