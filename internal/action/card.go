package action

// Card is how a personal or subscription feature (consent, fortune, notices) wants its result shown in
// chat: already localized by the Application, so the Discord connector renders it without knowing the
// feature. It travels in Result.Output["card"] and is plain JSON for the console and CLI too.
type Card struct {
	Title     string       `json:"title,omitempty"`
	Text      string       `json:"text,omitempty"`
	Tone      string       `json:"tone,omitempty"` // ok (default) | warn | error | neutral | fortune | school | transfer | reminder | quake
	Fields    []CardField  `json:"fields,omitempty"`
	Buttons   []CardButton `json:"buttons,omitempty"`
	Footer    string       `json:"footer,omitempty"`
	URL       string       `json:"url,omitempty"`
	ImageURL  string       `json:"image_url,omitempty"`
	Timestamp string       `json:"timestamp,omitempty"` // RFC 3339; shown by the footer in the reader's time zone
	Ephemeral bool         `json:"ephemeral,omitempty"` // only the person who asked sees it (slash commands)
}

// CardField is a labelled value.
type CardField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// CardButton is a button whose press comes back as a component interaction with ID as its custom ID
// ("<feature>:<verb>:<args>"); the connector maps it to an Action. A URL button just opens the link.
type CardButton struct {
	ID    string `json:"id,omitempty"`
	Label string `json:"label"`
	Style string `json:"style,omitempty"` // primary | secondary | success | danger | link
	URL   string `json:"url,omitempty"`
}

// WithCard attaches a card to an OK result's output.
func WithCard(res Result, c Card) Result {
	if res.Output == nil {
		res.Output = map[string]any{}
	}
	res.Output["card"] = c
	return res
}
