package reviewspack

// TokenHashForTest exposes the stored form of a link token to the external
// live suite, which writes request rows the way the pack does.
func TokenHashForTest(token string) string { return tokenHash(token) }
