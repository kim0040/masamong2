package application_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/memory/store"
)

type peopleServersTestResponse struct {
	Total   int                     `json:"total_count"`
	Servers []peopleServerTestGroup `json:"servers"`
}

type peopleServerTestGroup struct {
	GuildID string                   `json:"guild_id"`
	Total   int                      `json:"total_count"`
	People  []peopleServerTestPerson `json:"people"`
}

type peopleServerTestPerson struct {
	UserID     string   `json:"user_id"`
	Name       string   `json:"name"`
	OtherNames []string `json:"other_names"`
	Messages   int64    `json:"messages"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
	CardItems  int      `json:"card_items"`
	HasUser    bool     `json:"has_user"`
}

func TestPeopleServersReturnsPerGuildCountsNamesAndCardTotals(t *testing.T) {
	svc, _, _ := memorySvc(t)
	seedServerPerson(t, svc, "p1", "discord:g:c", "u1", "Old Name", "2026-10-10T12:00:00+00:00")
	seedServerPerson(t, svc, "p2", "discord:g:c", "u1", "Mid Name", "2026-10-11T12:00:00+00:00")
	seedServerPerson(t, svc, "p3", "discord:g:c", "u1", "Current Name", "2026-10-12T12:00:00+00:00")
	seedServerPerson(t, svc, "p4", "discord:g:c2", "u1", "Current Name", "2026-10-13T12:00:00+00:00")
	seedServerPerson(t, svc, "p5", "discord:g2:c2", "u2", "Second Server", "2026-10-13T11:00:00+00:00")
	seedServerPerson(t, svc, "p6", "discord:g:c", "withdrawn", "Withdrawn Name", "2026-10-13T10:00:00+00:00")
	seedServerPerson(t, svc, "p7", "discord:g:c", "deleted", "Deleted Name", "2026-10-13T09:00:00+00:00")
	seedServerPerson(t, svc, "p8", "discord:0:dm", "dm-user", "DM Name", "2026-10-13T08:00:00+00:00")
	if _, err := svc.Store.DB.Exec(`INSERT INTO consent_state(user_id,scope,status,updated_at) VALUES('withdrawn','memory','withdrawn','2026-10-13T10:00:00Z')`); err != nil {
		t.Fatalf("seed memory withdrawal: %v", err)
	}
	if _, err := svc.Store.DB.Exec(`UPDATE memory_observations SET deleted_at='2026-10-13T09:30:00Z' WHERE observation_id='p7'`); err != nil {
		t.Fatalf("mark deleted observation: %v", err)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO user_prefs(user_id,updated_at) VALUES('u1','2026-10-13T12:00:00Z')`); err != nil {
		t.Fatalf("seed users.list row: %v", err)
	}
	ref := store.PersonRef("u1")
	for _, card := range []struct{ id, state string }{{"c1", "active"}, {"c2", "active"}, {"c3", "superseded"}} {
		if _, err := svc.Store.DB.Exec(`INSERT INTO person_card_item(id,guild_id,person_ref,key,text,evidence,episodes,origin,state,created_at,updated_at)
			VALUES(?, 'g', ?, ?, 'private card canary', '[]','[]','test',?,?,?)`, card.id, ref, card.id, card.state, "2026-10-13T12:00:00Z", "2026-10-13T12:00:00Z"); err != nil {
			t.Fatalf("seed card item: %v", err)
		}
	}

	r := exec(svc, "people.servers", nil)
	if !r.OK {
		t.Fatalf("people.servers: %+v", r)
	}
	raw, err := json.Marshal(r.Output)
	if err != nil {
		t.Fatal(err)
	}
	var out peopleServersTestResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 2 || len(out.Servers) != 2 {
		t.Fatalf("two server people should remain after privacy and DM filtering: %s", raw)
	}
	serverByGuild := map[string]*peopleServerTestGroup{}
	for i := range out.Servers {
		serverByGuild[out.Servers[i].GuildID] = &out.Servers[i]
	}
	serverG, serverG2 := serverByGuild["g"], serverByGuild["g2"]
	if serverG == nil || serverG2 == nil || serverG.Total != 1 || serverG2.Total != 1 || len(serverG.People) != 1 {
		t.Fatalf("people should be grouped by Discord server: %s", raw)
	}
	p := serverG.People[0]
	if p.UserID != "u1" || p.Name != "Current Name" || p.Messages != 4 || p.CardItems != 2 || !p.HasUser {
		t.Fatalf("current person summary: %+v", p)
	}
	if len(p.OtherNames) != 2 || p.OtherNames[0] != "Mid Name" || p.OtherNames[1] != "Old Name" {
		t.Fatalf("recent names: %+v", p.OtherNames)
	}
	if p.FirstSeen != "2026-10-10T12:00:00Z" || p.LastSeen != "2026-10-13T12:00:00Z" {
		t.Fatalf("UTC RFC3339 timestamps: first=%q last=%q", p.FirstSeen, p.LastSeen)
	}
	if strings.Contains(string(raw), "private message canary") || strings.Contains(string(raw), "private card canary") || strings.Contains(string(raw), `"content"`) {
		t.Fatalf("people.servers must never return message or card text: %s", raw)
	}
	for _, person := range serverG.People {
		if person.UserID == "withdrawn" || person.UserID == "deleted" || person.UserID == "dm-user" {
			t.Fatalf("withdrawn, deleted, and DM people must not appear: %s", raw)
		}
	}

	filtered := exec(svc, "people.servers", map[string]any{"guild_id": "g", "q": "mid name"})
	if !filtered.OK {
		t.Fatalf("filtered people.servers: %+v", filtered)
	}
	raw, err = json.Marshal(filtered.Output)
	if err != nil {
		t.Fatal(err)
	}
	var filteredOut peopleServersTestResponse
	if err := json.Unmarshal(raw, &filteredOut); err != nil {
		t.Fatal(err)
	}
	if len(filteredOut.Servers) != 1 || filteredOut.Servers[0].GuildID != "g" || filteredOut.Servers[0].Total != 1 || len(filteredOut.Servers[0].People) != 1 || filteredOut.Servers[0].People[0].UserID != "u1" || filteredOut.Servers[0].People[0].Messages != 4 {
		t.Fatalf("guild and name-history query should retain whole person counts: %s", raw)
	}
}

func seedServerPerson(t *testing.T, svc *application.Service, observationID, room, account, label, at string) {
	t.Helper()
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("invalid test time: %v", err)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
		VALUES(?,'discord','discord',?,?,?, ?,?,'private message canary',?,'human_text',?)`,
		observationID, room, room, observationID, label, account, at, at); err != nil {
		t.Fatalf("seed person observation: %v", err)
	}
}
