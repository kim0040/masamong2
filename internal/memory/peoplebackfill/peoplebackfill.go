// Package peoplebackfill builds the work units of the paid people backfill (person memory phase 4,
// docs/plan-memory-conversation.md): per room, a room's observations are cut by the episode
// segmenter (internal/memory/episodes.Split), same-speaker runs merge into lines, and whole
// segments join into windows of at most 120 merged lines — the unit one model call (or one
// Message Batches item) extracts person claims from. It also selects the pilot's deterministic
// spread of windows and prices the run: a low/high estimate per model and mode, and the real
// settle from reported usage.
//
// Everything here is pure: no database, no network, no clock reading beyond what the caller
// passes in, so the same input always builds the same windows, ids, picks and prices.
package peoplebackfill
