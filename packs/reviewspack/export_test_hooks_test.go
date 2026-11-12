package reviewspack

// TokenHashForTest exposes the stored form of a link token to the external
// live suite, which writes request rows the way the pack does.
func TokenHashForTest(token string) string { return tokenHash(token) }

// ReviewIDForRequestForTest is the one review row a request's link writes.
func ReviewIDForRequestForTest(requestID string) string { return reviewIDForRequest(requestID) }

// RegisterShopperSurfaceForTest declares the pack's shopper routes, as
// Register does at boot, so the external live suite can post the real form
// through the real handler. The caller resets the surface around it.
func RegisterShopperSurfaceForTest() { registerShopperSurface() }
