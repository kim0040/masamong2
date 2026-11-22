package application

import "masamong/internal/action"

// A model turn in a server room (a chat turn, or a scheduled task's turn) answers in that room, so it is
// given, and may change, only the reminders the person made in that room: not the ones made in other
// rooms or in DMs. A DM is the person's alone, and so is the card of their own command or button: those
// reach every reminder of theirs.

// reminderScope is the SQL condition (with its leading AND) that keeps inv to the reminders it may see,
// or "" when it may see them all.
func reminderScope(inv *action.Invoker) (string, []any) {
	if inv == nil || (inv.Via != viaChat && inv.Via != viaTask) || inv.Private || inv.GuildID == "" {
		return "", nil
	}
	return ` AND channel_id=?`, []any{inv.ChannelID}
}

// inReminderScope is reminderScope for one reminder found by its id.
func inReminderScope(inv *action.Invoker, r reminder) bool {
	where, args := reminderScope(inv)
	return where == "" || r.ChannelID == args[0]
}
