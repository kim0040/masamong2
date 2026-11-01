// Package errcode holds stable machine codes. JSON enum is never translated (17장).
package errcode

const (
	ConfigConflict     = "CONFIG_CONFLICT"
	ConfigInvalid      = "CONFIG_INVALID"
	ActionUnavailable  = "ACTION_UNAVAILABLE"
	ActionUnknown      = "ACTION_UNKNOWN"
	StaleRevision      = "STALE_REVISION"
	ScopeMismatch      = "SCOPE_MISMATCH"
	LeaseMismatch      = "LEASE_MISMATCH"
	JobNotFound        = "JOB_NOT_FOUND"
	JobIdempotency     = "JOB_IDEMPOTENCY_CONFLICT"
	InvalidInput       = "INVALID_INPUT"
	NotSetup           = "NOT_SETUP"
	ProviderFakeOnly   = "PROVIDER_FAKE_ONLY"
	NoRecommendation   = "NO_RECOMMENDATION"
	BackupVerifyFailed = "BACKUP_VERIFY_FAILED"
	PlanRejected       = "PLAN_REJECTED"
	BudgetExceeded     = "BUDGET_EXCEEDED"
	PermissionDenied   = "PERMISSION_DENIED"
	FeatureDisabled    = "FEATURE_DISABLED"
	ToolLimited        = "TOOL_LIMITED" // a tools.<id>.limits cap or cooldown refused the call
)

// Provider outcome codes (05장 부분 실패 계약). Retryable is carried on the Error, not encoded in the code.
const (
	IO                   = "IO"
	ProviderUnconfigured = "PROVIDER_UNCONFIGURED"
	ProviderUnavailable  = "PROVIDER_UNAVAILABLE" // transport: timeout, connection, 5xx
	ProviderRateLimited  = "PROVIDER_RATE_LIMITED"
	ProviderRejected     = "PROVIDER_REJECTED" // 4xx / service error / auth
	ProviderFormat       = "PROVIDER_FORMAT"   // body did not match the expected wire format
	ProviderError        = "PROVIDER_ERROR"
	DiscordNotConnected  = "DISCORD_NOT_CONNECTED"
	Unauthorized         = "UNAUTHORIZED"
)
