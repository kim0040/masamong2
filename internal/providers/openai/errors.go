package openai

import (
	"strconv"
	"strings"

	"masamong/internal/errclass"
)

// classedError is a fixed error that knows its class. It is a comparable value, so errors.Is works.
type classedError struct {
	msg   string
	class errclass.Class
}

func (e classedError) Error() string            { return e.msg }
func (e classedError) ErrClass() errclass.Class { return e.class }

// authError is a refused key with the host's own words. It is ErrAuth for errors.Is (callers branch on
// that) and class Auth for the ledgers; the APIError is kept as text only, so errors.As(*APIError)
// still does not match a refused key.
type authError struct{ detail string }

func (e *authError) Error() string            { return ErrAuth.Error() + ": " + e.detail }
func (e *authError) Unwrap() error            { return ErrAuth }
func (e *authError) ErrClass() errclass.Class { return errclass.Auth }

// byokError is the operator's own provider key (OpenRouter BYOK) refused upstream. The OpenRouter key
// itself is fine, so it is not ErrAuth (the provider is not marked rejected); class Permission rests
// the model for a while and lets its fallback answer.
type byokError struct{ detail string }

func (e *byokError) Error() string {
	return "openrouter: your own provider key (BYOK) was refused: " + e.detail
}
func (e *byokError) ErrClass() errclass.Class { return errclass.Permission }

// ErrClass classifies the error (errclass.Classed): the HTTP status is the base and the body's type,
// code and message refine it, because hosts disagree on statuses (Gemini answers a bad key with 400,
// OpenAI answers an empty account with 429, OpenRouter wraps a provider's 5xx in a 200 envelope).
func (e *APIError) ErrClass() errclass.Class { return classify(e) }

var (
	contextMarks = []string{"context_length_exceeded", "maximum context length", "context length", "context window",
		"exceeds the maximum number of tokens", "input token count", "prompt is too long", "reduce the length of the messages"}
	authMarks = []string{"invalid api key", "incorrect api key", "api key not valid", "invalid_api_key", "api_key_invalid", "no auth credentials", "authentication fail"}
	// a used-up balance or spending cap: these reset in hours to months, so they win over the 429 they
	// come with (OpenAI credit_balance_exhausted and the spend limits, OpenGateway budget_exceeded, NanoGPT
	// api_key_usd_limit, api_key_weekly_usd_limit, api_account_monthly_usd_limit and the
	// *_subscription_token_limit codes)
	quotaStrong = []string{"insufficient_quota", "exceeded your current quota", "credit_balance_exhausted", "organization_spend_limit_exceeded",
		"project_spend_limit_exceeded", "organization_usage_limit_exceeded", "budget_exceeded", "_usd_limit", "_subscription_token_limit"}
	// a request the host's content filter stopped: about this request, not the key or the model
	policyMarks = []string{"content_policy_violation"}
	rateMarks   = []string{"rate limit", "rate_limit", "too many requests", "tokens per min", "requests per min", "resource_exhausted", "resource has been exhausted"}
	quotaWeak   = []string{"insufficient credits", "insufficient balance", "out of credits", "billing", "payment required", "credit balance"}
	overMarks   = []string{"overloaded", "server is busy", "temporarily unavailable"}
	notFound    = []string{"model_not_found", "model not found", "no endpoints found", "unknown model", "is not a valid model", "is not found for api version"}
	toolTerms   = []string{"tool_choice", "tools", "tool use", "tool calling", "tool_calls", "function calling", "function call", "functions"}
	// a tools refusal says the feature is missing; a schema complaint about one tool says the request is wrong
	toolNotTaken  = []string{"support", "no endpoints found", "not enabled", "not allowed", "not available"}
	toolSchemaish = []string{"schema", "tools[", "function_declarations", "parameters."}
)

// policyRefusal reports a content filter's refusal: OpenRouter's 403 content_policy_violation, or its
// error.metadata.error_type "content_policy_violation" / "refusal".
func policyRefusal(e *APIError, m string) bool {
	return hasAny(m, policyMarks) || e.MetaType == "content_policy_violation" || e.MetaType == "refusal"
}

func hasAny(m string, marks []string) bool {
	for _, k := range marks {
		if strings.Contains(m, k) {
			return true
		}
	}
	return false
}

// toolsRefused reports a message saying the model or route takes no tools at all.
func toolsRefused(m string) bool {
	// parallel_tool_calls alone is an optional field (shape.go drops it); it must not make tools unsupported
	m = strings.ReplaceAll(m, "parallel_tool_calls", "")
	return hasAny(m, toolTerms) && hasAny(m, toolNotTaken) && !hasAny(m, toolSchemaish)
}

func classify(e *APIError) errclass.Class {
	status := e.Status
	if status == 0 { // a 200 envelope: some hosts put the real status in the numeric code
		if n, err := strconv.Atoi(e.Code); err == nil {
			status = n
		}
	}
	base := errclass.FromStatus(status)
	m := strings.ToLower(e.Type + " " + e.Code + " " + e.Message)
	reqStatus := status == 0 || status == 400 || status == 404 || status == 422
	switch {
	case e.LimitSource != "":
		// OpenRouter's 403 for a spent key, workspace or guardrail budget (error.metadata.limit_source)
		return errclass.Quota
	case policyRefusal(e, m):
		// a content filter's 403: the fallback model may answer it; neither key nor model is at fault
		return errclass.Empty
	case strings.Contains(m, "authentication_rate_limited"):
		// NanoGPT's 429 after failed key attempts: the same key waits it out, like a refused key
		return errclass.Auth
	case status == 401:
		return errclass.Auth
	case (status == 0 || status == 400 || status == 403) && hasAny(m, authMarks):
		return errclass.Auth
	case hasAny(m, contextMarks):
		return errclass.ContextLength
	case strings.Contains(m, "model_not_found"):
		// at any status: CometAPI answers a model it cannot route with 503 model_not_found
		return errclass.NotFound
	case hasAny(m, quotaStrong):
		return errclass.Quota
	case status == 429 || hasAny(m, rateMarks):
		return errclass.RateLimited
	case hasAny(m, quotaWeak):
		return errclass.Quota
	case status == 529 || hasAny(m, overMarks):
		return errclass.Overloaded
	case reqStatus && toolsRefused(m):
		return errclass.Unsupported
	case reqStatus && hasAny(m, notFound):
		return errclass.NotFound
	case reqStatus && strings.Contains(m, "does not exist") && strings.Contains(m, "model"):
		return errclass.NotFound
	}
	if base == errclass.None {
		return errclass.Unknown
	}
	return base
}
