package launcher

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// A console proves it belongs to this installation before a client sends it the sign-in token: the
// client sends a random challenge with GET /api/health, and the console answers with an HMAC of the
// challenge and the address it listens on, keyed with the token. A program that took the port of a
// stopped service does not know the token and cannot answer; a console of another installation (or
// one relaying to this one from another address) answers for a different token or address.

// ChallengeParam is the /api/health query parameter that carries the client's challenge.
const ChallengeParam = "challenge"

// EndpointProof is the answer to challenge for a console listening on bind with token.
func EndpointProof(token, bind, challenge string) string {
	m := hmac.New(sha256.New, []byte(token))
	_, _ = io.WriteString(m, "masamong-endpoint\x00"+bind+"\x00"+challenge)
	return hex.EncodeToString(m.Sum(nil))
}

// ValidChallenge reports whether a challenge has the form clients send (32 hex digits), so a console
// answers only that and never signs arbitrary text.
func ValidChallenge(c string) bool {
	return len(c) == 32 && strings.Trim(c, "0123456789abcdef") == ""
}

// errNotLoopback and errUnproven explain why a token was not sent. Neither names the token.
var (
	errNotLoopback = errors.New("the recorded console address is not on this computer; the sign-in token was not sent")
	errUnproven    = errors.New("the program at the recorded console address did not prove it is this installation's service; the sign-in token was not sent (if an older masamongd is running, stop it from its own menu or restart the computer)")
)

// loopbackAddr reports whether host:port names this computer by address (127.0.0.0/8, ::1) or as
// localhost.
func loopbackAddr(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// noRedirect keeps a client from following a redirect (to another host or port) with a token.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// verifyEndpoint checks, before the token leaves this process, that bind is a loopback address and
// that the console there knows token. dataRoot is used only for a console of an older version, which
// cannot answer the challenge: it is accepted while this data folder's daemon lock is held, since
// then that daemon is alive and still owns its port (no other program can have taken it).
func verifyEndpoint(ctx context.Context, bind, token, dataRoot string) error {
	addr := hostPort(bind)
	if !loopbackAddr(addr) {
		return errNotLoopback
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	challenge := hex.EncodeToString(b[:])
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/api/health?"+ChallengeParam+"="+challenge, nil)
	res, err := (&http.Client{CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var out struct {
		Proof *string `json:"proof"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&out) != nil {
		return errUnproven
	}
	if out.Proof == nil {
		if dataRoot != "" && lockHeld(dataRoot, "daemon.lock") {
			return nil
		}
		return errUnproven
	}
	if !hmac.Equal([]byte(*out.Proof), []byte(EndpointProof(token, addr, challenge))) {
		return errUnproven
	}
	return nil
}
