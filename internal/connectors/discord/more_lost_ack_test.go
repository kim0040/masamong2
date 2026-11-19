package discord

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// lostAckTransport posts a message once per nonce and can drop the answer to the first post.
type lostAckTransport struct {
	next         *fakeDiscord
	visible      map[string]string
	loseFirstAck bool
}

func (f *lostAckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/channels/c1/messages") {
		return f.next.RoundTrip(req)
	}
	var body struct {
		Nonce        string `json:"nonce"`
		EnforceNonce bool   `json:"enforce_nonce"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Nonce == "" || !body.EnforceNonce {
		return nil, fmt.Errorf("expected the production send's enforced nonce")
	}
	id, duplicate := f.visible[body.Nonce]
	if !duplicate {
		id = fmt.Sprint(200 + len(f.visible))
		f.visible[body.Nonce] = id
	}
	if f.loseFirstAck {
		f.loseFirstAck = false
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"id":%q}`, id))), Request: req}, nil
}

// A More press whose post reached Discord but whose answer was lost does not leave the rest to be posted
// again by the next press (the next press has a new nonce, so Discord would show a second copy).
func TestMoreRetryDoesNotRepostAfterALostAcknowledgement(t *testing.T) {
	g, fake := gatewayWithFakeDiscord(t)
	g.ActionTimeout = 40 * time.Millisecond
	transport := &lostAckTransport{next: fake, visible: map[string]string{}, loseFirstAck: true}
	g.sess.Client = &http.Client{Transport: transport}
	g.svc.NoteReplyMessages("original", "g1", "c1", []string{"head"})
	g.more.put("head", "c1", "synthetic continuation", nil)
	press := func(id string) {
		g.handleMore(g.sess, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: id, Token: id, GuildID: "g1", ChannelID: "c1",
			Message: &discordgo.Message{ID: "head", Content: "synthetic reply"}}}, "head")
		g.inflight.Wait()
	}
	press("one")
	if len(transport.visible) != 1 {
		t.Fatalf("first POST should have published before the lost acknowledgement: %d", len(transport.visible))
	}
	press("two")
	if len(transport.visible) != 1 {
		t.Fatalf("two copies of the continuation are visible after retry: %d POSTs with new nonces", len(transport.visible))
	}
}
