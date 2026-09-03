package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"
)

// jwt.go -- the app JWT, signed here rather than through a JWT library.
//
// About twenty-five lines against a new DIRECT dependency for the root module,
// and the twenty-five lines are the ones a reader can check: one header, three
// claims, RS256 over the two base64url segments. github.com/golang-jwt/jwt/v5
// is present in go.mod as an INDIRECT requirement, and promoting it to a direct
// one for this would be a module-graph change to save a function.

const (
	// jwtLifetime is eight minutes, and the number is chosen against the
	// BACKDATE below rather than on its own. GitHub refuses an assertion
	// whose exp is more than ten minutes past its iat, and the backdate is
	// part of that span -- so nine minutes plus a minute of backdating is
	// exactly 600 seconds, sitting on the boundary GitHub rejects at. Eight
	// leaves 540, which is still far longer than the one round trip the
	// assertion is minted for.
	jwtLifetime = 8 * time.Minute
	// jwtBackdate is the other half of the same problem. A node whose clock
	// runs a few seconds fast issues a JWT GitHub reads as being from the
	// future and rejects; sixty seconds of backdating is what GitHub's own
	// documentation recommends.
	jwtBackdate = 60 * time.Second
)

// appJWT signs one short-lived assertion that this cluster IS the app.
//
// It is the credential for exactly two endpoints -- the installation lookup
// and the installation-token mint -- and for nothing else. It is never cached:
// signing is cheap next to a round trip, and a cached assertion is one more
// secret with a lifetime to reason about.
func (c *Client) appJWT(now time.Time) (string, error) {
	if !c.cfg.Configured() {
		return "", ErrNotConfigured
	}
	key, err := c.privateKey()
	if err != nil {
		return "", err
	}
	claims, merr := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": c.cfg.AppId,
	})
	if merr != nil {
		return "", merr
	}
	signing := base64url([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64url(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, serr := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if serr != nil {
		return "", serr
	}
	return signing + "." + base64url(sig), nil
}

func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// privateKey decodes and parses the app's key ONCE per client.
//
// Every error here names the SHAPE of the failure and never the material: a
// base64 error carries a byte offset, pem.Decode carries nothing at all, and
// x509's errors describe structure. That is deliberate -- this is the one
// function in the package holding key bytes, and its errors are the only route
// they could take to a log line.
//
// Both PKCS#1 and PKCS#8 are accepted because GitHub's download button hands
// out the first and every conversion tool in the world produces the second;
// refusing one would be refusing a key that is correct.
func (c *Client) privateKey() (*rsa.PrivateKey, error) {
	c.keyOnce.Do(func() {
		raw, err := base64.StdEncoding.DecodeString(c.cfg.PrivateKeyB64)
		if err != nil {
			c.keyErr = fmt.Errorf("%s is not base64: %v", EnvPrivateKeyB64, err)
			return
		}
		block, _ := pem.Decode(raw)
		if block == nil {
			c.keyErr = fmt.Errorf("%s does not decode to a PEM block -- it is the app's private key file, base64-encoded whole", EnvPrivateKeyB64)
			return
		}
		if key, perr := x509.ParsePKCS1PrivateKey(block.Bytes); perr == nil {
			c.key = key
			return
		}
		parsed, perr := x509.ParsePKCS8PrivateKey(block.Bytes)
		if perr != nil {
			c.keyErr = fmt.Errorf("%s is neither a PKCS#1 nor a PKCS#8 private key: %v", EnvPrivateKeyB64, perr)
			return
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			c.keyErr = fmt.Errorf("%s is a %T; a GitHub App signs with RSA", EnvPrivateKeyB64, parsed)
			return
		}
		c.key = key
	})
	return c.key, c.keyErr
}
