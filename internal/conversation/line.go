// Package conversation decides which lines of a busy channel belong to the conversation the bot is
// answering (attribution) and lays them out for the prompts (the live transcript, L0).
//
// Why it exists: a Discord room carries several conversations at once. One friend talks to the bot while
// two others decide which game to play. When the answer model is shown every recent line and told to
// ignore the unrelated ones, it sometimes answers the game talk anyway. Code decides what the model sees
// instead (docs/plan-memory-conversation.md §5.1, principle 1): the links between messages — replies,
// mentions, the bot's replies, one person's bursts — are structural, cost nothing, behave the same in
// every language and can be tested. Meaning ("does 'that one' point at the game talk?") is left to a small
// classifier the caller runs on the Candidates this package returns.
package conversation

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Line is one message of a channel, as the live window or the store knows it.
type Line struct {
	ID       string // the platform message ID (Discord snowflake)
	AuthorID string // the account; "" when the platform has none (Kakao imports)
	Author   string // the name people see
	Bot      bool   // the bot itself
	ReplyTo  string // the message this one replies to ("" for none)
	// Mentions are the accounts the message mentions (people and the bot). MentionsOthers marks a role or
	// @everyone mention, which is directed at people in general.
	Mentions       []string
	MentionsOthers bool
	At             time.Time
	Text           string
}

func (l Line) mentions(id string) bool {
	if id == "" {
		return false
	}
	for _, m := range l.Mentions {
		if m == id {
			return true
		}
	}
	return false
}

// Names maps the names people are called by in a channel (lower-cased display names) to their account.
// It is built from who actually speaks in the channel, so no member list or language rules are needed.
type Names map[string]string

// NamesFrom collects the authors of lines (the bot excluded) as a Names table.
func NamesFrom(lines []Line) Names {
	n := Names{}
	for _, l := range lines {
		if l.Bot || l.AuthorID == "" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(l.Author))
		if utf8.RuneCountInString(name) >= 2 && nameLike(name) {
			n[name] = l.AuthorID
		}
	}
	return n
}

// nameLike: a display name that can be told from ordinary text — it has letters, and is not one
// character repeated ("ㅋㅋ", "..."), which would make every laugh a call.
func nameLike(name string) bool {
	var first rune
	letters, same := 0, true
	for i, r := range name {
		if unicode.IsLetter(r) {
			letters++
		}
		if i == 0 {
			first = r
		} else if r != first {
			same = false
		}
	}
	return letters >= 2 && !same
}

// Called returns the accounts whose name appears in text as a word (see CallsName).
func (n Names) Called(text string) []string {
	var out []string
	lower := strings.ToLower(text)
	for name, id := range n {
		if CallsName(lower, name) {
			out = append(out, id)
		}
	}
	return out
}

// CallsName reports whether name (lower-cased) occurs in text (lower-cased) as a name rather than inside
// another word. Scripts written without spaces between words (Hangul, Han, Kana, Thai) attach particles
// and endings to a name ("민수야", "みんすさん"), so only the left edge is checked there; in scripts that
// separate words with spaces both edges must be word boundaries ("Sam" does not match "Samsung").
func CallsName(text, name string) bool {
	if name == "" {
		return false
	}
	for start := 0; start <= len(text)-len(name); {
		i := strings.Index(text[start:], name)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(name)
		first, _ := utf8.DecodeRuneInString(name)
		last, _ := utf8.DecodeLastRuneInString(name)
		before, _ := utf8.DecodeLastRuneInString(text[:i])
		after, _ := utf8.DecodeRuneInString(text[end:])
		leftOK := i == 0 || !wordRune(before) || (spaceless(first) && !spaceless(before))
		rightOK := end == len(text) || !wordRune(after) || spaceless(last)
		if leftOK && rightOK {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		start = i + size
	}
	return false
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// spaceless reports scripts that do not separate words with spaces.
func spaceless(r rune) bool {
	return unicode.In(r, unicode.Hangul, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Thai)
}
