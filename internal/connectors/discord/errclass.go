package discord

import (
	"errors"
	"strings"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/errclass"
)

// Discord JSON error codes the gateway reacts to (https://discord.com/developers/docs/topics/opcodes-and-status-codes).
const (
	codeMissingAccess      = 50001
	codeMissingPermissions = 50013
	codeUnknownChannel     = 10003
	codeUnknownMessage     = 10008
	codeInvalidFormBody    = 50035
)

// classifyREST puts a Discord REST failure into the shared failure vocabulary. Discord's own JSON code
// wins over the HTTP status where it says more (50013 is a 403, but 50035 is a 400 that can mean
// "the message you reply to is gone"). Errors that are not Discord answers (context, network) go to
// errclass.Of.
func classifyREST(err error) errclass.Class {
	if err == nil {
		return errclass.None
	}
	var rl *discordgo.RateLimitError
	if errors.As(err, &rl) {
		return errclass.RateLimited
	}
	var re *discordgo.RESTError
	if errors.As(err, &re) && re != nil {
		code := 0
		if re.Message != nil {
			code = re.Message.Code
		}
		switch code {
		case codeMissingPermissions, codeMissingAccess:
			return errclass.Permission
		case codeUnknownChannel, codeUnknownMessage:
			return errclass.NotFound
		case codeInvalidFormBody:
			return errclass.BadRequest
		}
		if re.Response != nil {
			if c := errclass.FromStatus(re.Response.StatusCode); c != errclass.None {
				return c
			}
		}
		return errclass.Unknown
	}
	if c := errclass.Of(err); c != errclass.Unknown {
		return c
	}
	// discordgo gives up on repeated 502s with a plain error instead of a RESTError
	if strings.Contains(err.Error(), "Exceeded Max retries HTTP 5") {
		return errclass.Overloaded
	}
	return errclass.Unknown
}

// replyTargetGone reports whether Discord refused a reply because the message it replies to no longer
// exists (400 with code 50035 whose body names message_reference). Sending the same text without the
// reference works.
func replyTargetGone(err error) bool {
	var re *discordgo.RESTError
	if !errors.As(err, &re) || re == nil || re.Message == nil || re.Message.Code != codeInvalidFormBody {
		return false
	}
	return strings.Contains(string(re.ResponseBody), "message_reference") || strings.Contains(re.Message.Message, "message_reference")
}
