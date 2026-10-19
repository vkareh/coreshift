package certgen

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// GenerateSessionSecrets creates the oauth-server session secrets file: one
// authentication secret for signing session cookies and one encryption secret
// for their contents, in the SessionSecrets format the oauth server reads.
// Following Hypershift's control-plane operator, the secrets are random
// URL-safe strings of 64 and 32 characters.
func GenerateSessionSecrets() ([]byte, error) {
	return []byte(fmt.Sprintf(`apiVersion: osin.config.openshift.io/v1
kind: SessionSecrets
secrets:
- authentication: %s
  encryption: %s
`, randomString(64), randomString(32))), nil
}

// randomString returns size random URL-safe characters, avoiding path
// separators so the result is safe to embed in the secrets file.
func randomString(size int) string {
	b64size := base64.RawURLEncoding.DecodedLen(size) + 1
	b := make([]byte, b64size)
	if _, err := rand.Read(b); err != nil {
		panic(err) // rand should never fail
	}
	return base64.RawURLEncoding.EncodeToString(b)[:size]
}
