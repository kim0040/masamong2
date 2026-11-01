// Package errclass is the one vocabulary for "what kind of failure was this" across model providers,
// tool services and the Discord REST API. Ledgers (llm_calls, tool_calls, delivery) record the class, and
// retry decisions read Retryable instead of every caller guessing from status codes again.
package errclass

import (
	"context"
	"errors"
	"net"
)

// Class is a stable machine value (stored in ledgers, never translated).
type Class string

const (
	None          Class = ""               // no error
	Timeout       Class = "timeout"        // the deadline passed (ours or the server's 408/504)
	Network       Class = "network"        // connection refused, reset, DNS
	RateLimited   Class = "rate_limited"   // 429 or a provider's "slow down"
	Overloaded    Class = "overloaded"     // 5xx, 529, "overloaded"
	Auth          Class = "auth"           // 401, a rejected or missing key
	Permission    Class = "permission"     // 403: the key or bot lacks a right (Discord: missing access)
	Quota         Class = "quota"          // out of credits, monthly cap, 402
	BadRequest    Class = "bad_request"    // 400/422 the request itself is wrong (fix the code, not retry)
	Unsupported   Class = "unsupported"    // the model or endpoint does not take a feature (tools, a parameter)
	ContextLength Class = "context_length" // the prompt is longer than the model takes
	NotFound      Class = "not_found"      // 404: unknown model, deleted message or channel
	Format        Class = "format"         // the answer did not parse
	Empty         Class = "empty"          // the call succeeded but returned nothing usable
	Canceled      Class = "canceled"       // our caller gave up (shutdown, a newer message)
	Unknown       Class = "unknown"
)

// Retryable reports whether trying the same request again later can succeed.
func (c Class) Retryable() bool {
	switch c {
	case Timeout, Network, RateLimited, Overloaded, Empty:
		return true
	}
	return false
}

// Classed is implemented by errors that know their class (providers.APIError, Discord REST errors).
type Classed interface{ ErrClass() Class }

// Of classifies err. Errors that carry their own class win; then context and network errors; anything
// else is Unknown.
func Of(err error) Class {
	if err == nil {
		return None
	}
	var c Classed
	if errors.As(err, &c) {
		if k := c.ErrClass(); k != "" {
			return k
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return Timeout
		}
		return Network
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return Network
	}
	return Unknown
}

// FromStatus maps an HTTP status to a class (0 or 2xx = None).
func FromStatus(status int) Class {
	switch {
	case status == 0 || (status >= 200 && status < 300):
		return None
	case status == 401:
		return Auth
	case status == 402:
		return Quota
	case status == 403:
		return Permission
	case status == 404:
		return NotFound
	case status == 408 || status == 504:
		return Timeout
	case status == 413:
		return ContextLength
	case status == 429:
		return RateLimited
	case status == 400 || status == 422:
		return BadRequest
	case status >= 500:
		return Overloaded
	}
	return Unknown
}
