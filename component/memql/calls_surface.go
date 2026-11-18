package memql

// calls_surface.go -- THE WIRE NAMES OF THE CALLS SURFACE, shared by the edge, which
// answers a deployable's own origin at CallsInboundPath, and the bff, which serves the
// routes under CallsSurfacePathPrefix (component/server calls_handler.go). The owner
// approved this HTTP route family on 2026-11-17.
//
// THEY LIVE HERE for the reason the access surface's do (site_access_code.go): this is the
// one package component/edge and component/server can both import, and a literal repeated
// on each side is wrong before it drifts. If the two ever disagree the symptom is silent: the
// edge stamps a header the bff does not read, or forwards to a path the bff does not
// answer, and every call request reaches the bff as the 404 a stranger gets, with nothing
// naming why.
//
// THEY ARE NOT THE ACCESS SURFACE'S NAMES, and must not become them. The edge strips each
// surface's stamps and cookie from every request it forwards anywhere else, so a shared
// name would carry one surface's stamp onto the other's routes.

// The two headers the edge stamps on every calls request it forwards, having stripped any
// copy the client sent.
const (
	// CallsStampSiteHeader names the v1:platform:site row the request arrived on. It is a
	// POINTER, never an assertion: the bff reads that row again under the stamped owner and
	// hands the calls service the row's own site, never the header's.
	CallsStampSiteHeader = "X-Memql-Calls-Site"

	// CallsStampOwnerHeader names the user who owns the site, as whom every calls row of it
	// is read and written. The bff reads the site under this user and requires the row's own
	// ownerUserId to equal it: a forged owner reads no row, and one who reads it as a member
	// of the site's organization is refused by the comparison.
	CallsStampOwnerHeader = "X-Memql-Calls-Owner"
)

// The routes of the calls surface on the bff: the base its handler mounts under and the ten
// paths the edge forwards to. Each is reached on a deployable's own origin at the same name
// under CallsInboundPath, and the edge maps one to the other.
const (
	// CallsSurfacePathPrefix is the base the bff mounts the surface under. It is not a
	// visitor's path: on a deployable's origin the surface is CallsInboundPath.
	CallsSurfacePathPrefix = "/calls/"

	// CallsUnlockPath trades a key for a host session (POST).
	CallsUnlockPath = CallsSurfacePathPrefix + "unlock"
	// CallsSessionPath says what the cookie presented holds (GET).
	CallsSessionPath = CallsSurfacePathPrefix + "session"
	// CallsStartPath opens a call on a key's session and seats its holder as host (POST).
	CallsStartPath = CallsSurfacePathPrefix + "start"
	// CallsSchedulePath books a call for later (POST).
	CallsSchedulePath = CallsSurfacePathPrefix + "schedule"
	// CallsReschedulePath moves a booked call and replaces its links (POST).
	CallsReschedulePath = CallsSurfacePathPrefix + "reschedule"
	// CallsCancelPath cancels a booked call (POST).
	CallsCancelPath = CallsSurfacePathPrefix + "cancel"
	// CallsJoinPath seats a person in a call by its link or by the session they hold (POST).
	CallsJoinPath = CallsSurfacePathPrefix + "join"
	// CallsQueuePath is a waiting seat's poll (GET).
	CallsQueuePath = CallsSurfacePathPrefix + "queue"
	// CallsHostPath makes one host control (POST).
	CallsHostPath = CallsSurfacePathPrefix + "host"
	// CallsLeavePath takes the session's seat out of its call: the hang-up (POST).
	CallsLeavePath = CallsSurfacePathPrefix + "leave"
)

// What a browser sees of the surface, and the two caps on what it may send.
//
// THE CAPS ARE READ FROM BOTH SIDES: the edge caps a body so a refusal comes before the
// hop, and the bff caps it again while reading, because the edge's cap is the edge's.
const (
	// CallsInboundPath is the surface on a deployable's own origin, under the /_memql/
	// marker with every other reserved path, so a bundle may not claim it. Each route is
	// this path, a slash and the route's name.
	CallsInboundPath = "/_memql/calls"

	// CallsSmallBodyBytes caps every route but schedule and reschedule. The largest of
	// them is a join: two link tokens of 32 symbols, a name and a terms version of at most
	// 64 characters each and three confirmations, which stays under 1 KiB even with every
	// character escaped.
	CallsSmallBodyBytes int64 = 1 << 10

	// CallsScheduleBodyBytes caps schedule and reschedule, which carry a booking's title.
	CallsScheduleBodyBytes int64 = 4 << 10
)

// CallsCookieName is the cookie a calls session's token travels in: a key holder's after
// unlock, and a person's seat session after their first join. The edge reads the name and
// never the value: it keeps the cookie to the calls routes, and the bff sets it and reads it.
//
// A __Host- PREFIX, for AccessCookieName's reason: a browser refuses a __Host- cookie unless
// it is Secure, has Path=/ and names no Domain, so a sibling deployable on the same parent
// domain cannot plant one to catch somebody's seat.
const CallsCookieName = "__Host-memql_calls"
