package calls

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// SimulationOperation identifies one caller-supplied operation within its
// authenticated site and key. The separate payload fingerprint detects a nonce
// reused for a different request. Both survive process and token-pepper changes;
// neither is an authorization secret or includes credential material.
func SimulationOperation(siteID, keyID string, request AccountRequest) (operationID, payloadDigest string) {
	operation := sha256.Sum256([]byte("calls.simulation\x00" + siteID + "\x00" + keyID + "\x00" + request.IdempotencyKey))
	operationID = hex.EncodeToString(operation[:])
	payload, _ := json.Marshal(struct {
		Action string `json:"action"`
		Amount int64  `json:"amount"`
		Plan   string `json:"plan"`
	}{request.Action, request.AmountCents, request.Plan})
	digest := sha256.Sum256(payload)
	payloadDigest = hex.EncodeToString(digest[:])
	return operationID, payloadDigest
}
