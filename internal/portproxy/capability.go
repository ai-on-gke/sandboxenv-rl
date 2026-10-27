// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portproxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// A port capability lets a WebSocket client that cannot set headers through
// the port proxy when the API requires its bearer token. The OpenHands SDK is
// one: it opens its event stream with the workspace URL and no extra headers,
// and it drops the URL's query string. It keeps the URL's user info, which the
// WebSocket client sends as "Authorization: Basic". Thus the caller puts
// CapabilityUser and a capability, not the token, in the user info of the URL.
//
// The capability is "v1.<expiry>.<mac>": expiry in Unix seconds, and mac the
// lowercase hex HMAC-SHA256, keyed with the API's bearer token, of
//
//	"ate-env port capability v1\n" + atespace + "\n" + id + "\n" + port + "\n" + expiry
//
// It names one environment port and expires, thus a URL that lands in a log
// exposes that port until the expiry, and never the token. The proxy accepts
// it only on WebSocket upgrade requests, for the target it names, and never
// forwards it. A token rotation makes the capabilities of the old token fail.
const CapabilityUser = "ate-env-capability"

// MaxCapabilityTTL bounds how far in the future a capability can expire.
const MaxCapabilityTTL = 7 * 24 * time.Hour

const capabilityVersion = "v1"

func capabilityMAC(token, atespace, id string, port int, expiry int64) []byte {
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte("ate-env port capability " + capabilityVersion + "\n" +
		atespace + "\n" + id + "\n" + strconv.Itoa(port) + "\n" + strconv.FormatInt(expiry, 10)))
	return m.Sum(nil)
}

// Capability returns the capability for one environment port, signed with the
// API's bearer token, that expires at expiry.
func Capability(token, atespace, id string, port int, expiry time.Time) string {
	e := expiry.Unix()
	return capabilityVersion + "." + strconv.FormatInt(e, 10) + "." +
		hex.EncodeToString(capabilityMAC(token, atespace, id, port, e))
}

var errCapability = errors.New("invalid port capability")

// VerifyCapability checks capability against token for the environment port
// atespace/id:port at now. All failures return the same error, which names no
// cause, because the caller is not authenticated yet.
func VerifyCapability(token, capability, atespace, id string, port int, now time.Time) error {
	parts := strings.Split(capability, ".")
	if len(parts) != 3 || parts[0] != capabilityVersion {
		return errCapability
	}
	// Only the canonical text verifies: digits without a sign or a leading
	// zero, and lowercase hex. Thus one capability has one text form.
	expiry, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || strconv.FormatInt(expiry, 10) != parts[1] {
		return errCapability
	}
	if left := time.Unix(expiry, 0).Sub(now); left <= 0 || left > MaxCapabilityTTL {
		return errCapability
	}
	got, err := hex.DecodeString(parts[2])
	if err != nil || hex.EncodeToString(got) != parts[2] ||
		!hmac.Equal(got, capabilityMAC(token, atespace, id, port, expiry)) {
		return errCapability
	}
	return nil
}

// capabilityCredential returns the capability that r carries as
// "Authorization: Basic" with the user CapabilityUser. It looks at each
// Authorization field and at each element of a comma-separated value, thus a
// capability cannot hide behind another credential. ambiguous reports a
// capability next to any other credential: Authorization is a single field,
// and a capability must be the request's only one.
func capabilityCredential(r *http.Request) (capability string, found, ambiguous bool) {
	values := r.Header.Values("Authorization")
	for _, v := range values {
		elements := strings.Split(v, ",")
		for _, e := range elements {
			c, ok := parseCapabilityCredential(e)
			if !ok {
				continue
			}
			if found || len(values) > 1 || len(elements) > 1 {
				ambiguous = true
			}
			capability, found = c, true
		}
	}
	return capability, found, ambiguous
}

// parseCapabilityCredential returns the capability in one credential of the
// form "Basic base64(CapabilityUser:capability)".
func parseCapabilityCredential(credential string) (string, bool) {
	scheme, payload, ok := strings.Cut(strings.TrimSpace(credential), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		return "", false
	}
	user, capability, ok := strings.Cut(string(raw), ":")
	if !ok || user != CapabilityUser {
		return "", false
	}
	return capability, true
}

// isWebSocketUpgrade reports whether r is a WebSocket handshake that
// httputil.ReverseProxy forwards as an upgrade: a GET whose Connection lists
// "upgrade" and whose Upgrade lists "websocket". ReverseProxy removes Upgrade
// from a request without "Connection: upgrade" and forwards a plain request,
// thus a capability must not authorize that request.
func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		httpguts.HeaderValuesContainsToken(r.Header["Connection"], "Upgrade") &&
		httpguts.HeaderValuesContainsToken(r.Header["Upgrade"], "websocket")
}
