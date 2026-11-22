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
// that the console there knows token. A console that does not answer the challenge is refused, even
// while this data folder's daemon lock is held: a daemon that is still starting holds the lock while
// daemon.json may still name the port of the copy before it, which another program may have taken.
func verifyEndpoint(ctx context.Context, bind, token string) error {
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
	if out.Proof == nil || !hmac.Equal([]byte(*out.Proof), []byte(EndpointProof(token, addr, challenge))) {
		return errUnproven
	}
	return nil
}

// recordWait bounds how long a client waits for a starting daemon to replace a stale address record.
var recordWait = 15 * time.Second

// provenBind returns the address the token may be sent to: bind, once its console proved itself. With a
// data folder whose daemon lock is held, a console that does not prove itself may be a stale record of a
// daemon that is still starting (it writes a fresh record once its console listens), so the record is
// read again until the address in it proves itself or recordWait passes. Nothing is sent to an address
// that did not prove itself.
func provenBind(ctx context.Context, bind, token, dataRoot string) (string, error) {
	err := verifyEndpoint(ctx, bind, token)
	if err == nil || dataRoot == "" || !errors.Is(err, errUnproven) {
		return bind, err
	}
	deadline := time.Now().Add(recordWait)
	for time.Now().Before(deadline) && lockHeld(dataRoot, "daemon.lock") {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		info, ok := ReadInfo(dataRoot)
		if !ok {
			continue // the starting daemon removed the stale record and has not written its own yet
		}
		if verifyEndpoint(ctx, info.Bind, token) == nil {
			return info.Bind, nil
		}
	}
	return "", err
}
