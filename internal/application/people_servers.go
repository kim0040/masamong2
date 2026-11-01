package application

import (
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/privacy"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

const peopleServersLimit = 200

type serverPersonName struct {
	label string
	at    time.Time
}

type serverPerson struct {
	userID     string
	nameTimes  map[string]time.Time
	messages   int64
	firstSeen  time.Time
	lastSeen   time.Time
	cardItems  int
	hasUserRow bool
}

type serverPersonRow struct {
	UserID     string   `json:"user_id"`
	Name       string   `json:"name"`
	OtherNames []string `json:"other_names"`
	Messages   int64    `json:"messages"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
	CardItems  int      `json:"card_items"`
	HasUserRow bool     `json:"has_user"`
}

type serverPeopleGroup struct {
	GuildID string            `json:"guild_id"`
	Name    string            `json:"name"`
	Total   int               `json:"total_count"`
	HasMore bool              `json:"has_more"`
	People  []serverPersonRow `json:"people"`
}

// people.servers lists Discord accounts seen in captured server messages. It returns only labels,
// counts and timestamps; source text and person-card text never leave the database through this Action.
func (s *Service) peopleServers(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	in := inputMap(req)
	guildFilter := strings.TrimSpace(strVal(in, "guild_id", ""))
	query := strings.ToLower(strings.TrimSpace(strVal(in, "q", "")))

	prefix := "discord:"
	if guildFilter != "" {
		prefix = "discord:" + guildFilter + ":"
	}
	scanned, err := s.peopleScan(db, prefix)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	// withdrawals are read on every call (the scan may be up to a minute old): a person who withdrew
	// never shows, even from a cached scan
	withdrawn, err := peopleWithdrawn(db)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	groups := map[string]map[string]*serverPerson{}
	peopleByRef := map[string]*serverPerson{}
	for _, sc := range scanned {
		room, id, label, firstRaw, lastRaw, count := sc.room, sc.id, sc.label, sc.first, sc.last, sc.count
		if withdrawn[id] {
			continue
		}
		guildID := discordGuildFromRoom(room)
		if guildID == "" || (guildFilter != "" && guildID != guildFilter) {
			continue
		}
		first, firstOK := parsePeopleTime(firstRaw)
		last, lastOK := parsePeopleTime(lastRaw)
		if !firstOK || !lastOK {
			continue
		}
		byID := groups[guildID]
		if byID == nil {
			byID = map[string]*serverPerson{}
			groups[guildID] = byID
		}
		person := byID[id]
		if person == nil {
			person = &serverPerson{userID: id, nameTimes: map[string]time.Time{}}
			byID[id] = person
			peopleByRef[guildID+"\x00"+store.PersonRef(id)] = person
		}
		person.messages += count
		if person.firstSeen.IsZero() || first.Before(person.firstSeen) {
			person.firstSeen = first
		}
		if person.lastSeen.IsZero() || last.After(person.lastSeen) {
			person.lastSeen = last
		}
		if label != "" {
			if at, ok := person.nameTimes[label]; !ok || last.After(at) {
				person.nameTimes[label] = last
			}
		}
	}
	if db.IsSQLite() && tableReady(db, "person_card_item") {
		cardRows, err := db.Reader().Query(`SELECT guild_id, person_ref, COUNT(*) FROM person_card_item WHERE state='active' GROUP BY guild_id, person_ref`)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		for cardRows.Next() {
			var guildID, ref string
			var count int
			if err := cardRows.Scan(&guildID, &ref, &count); err != nil {
				cardRows.Close()
				return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
			}
			if person := peopleByRef[guildID+"\x00"+ref]; person != nil {
				person.cardItems = count
			}
		}
		if err := cardRows.Err(); err != nil {
			cardRows.Close()
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
		if err := cardRows.Close(); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
		}
	}

	userIDs, err := serverPeopleUserIDs(db)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", s.msg(req, "generic.error"), nil)
	}
	for _, id := range s.Doc().Access.Operators {
		userIDs[id] = true
	}
	for _, id := range s.Doc().Access.BlockedUsers {
		userIDs[id] = true
	}

	names := s.peopleServerNames()
	serverIDs := map[string]bool{}
	for guildID := range groups {
		serverIDs[guildID] = true
	}
	for guildID := range names {
		if guildFilter == "" || guildFilter == guildID {
			serverIDs[guildID] = true
		}
	}
	if guildFilter != "" {
		serverIDs[guildFilter] = true
		if names[guildFilter] == "" {
			names[guildFilter] = guildFilter
		}
	}
	for guildID := range serverIDs {
		if names[guildID] == "" {
			names[guildID] = guildID
		}
	}
	serverIDsSorted := make([]string, 0, len(serverIDs))
	for guildID := range serverIDs {
		serverIDsSorted = append(serverIDsSorted, guildID)
	}
	sort.Slice(serverIDsSorted, func(i, j int) bool {
		ni, nj := strings.ToLower(names[serverIDsSorted[i]]), strings.ToLower(names[serverIDsSorted[j]])
		if ni != nj {
			return ni < nj
		}
		return serverIDsSorted[i] < serverIDsSorted[j]
	})

	servers := make([]serverPeopleGroup, 0, len(serverIDsSorted))
	total := 0
	for _, guildID := range serverIDsSorted {
		people := make([]*serverPerson, 0, len(groups[guildID]))
		for _, person := range groups[guildID] {
			person.hasUserRow = userIDs[person.userID]
			if query != "" && !personMatchesQuery(person, query) {
				continue
			}
			people = append(people, person)
		}
		sort.Slice(people, func(i, j int) bool {
			if !people[i].lastSeen.Equal(people[j].lastSeen) {
				return people[i].lastSeen.After(people[j].lastSeen)
			}
			return people[i].userID < people[j].userID
		})
		group := serverPeopleGroup{GuildID: guildID, Name: names[guildID], Total: len(people), HasMore: len(people) > peopleServersLimit, People: make([]serverPersonRow, 0, min(len(people), peopleServersLimit))}
		total += group.Total
		for _, person := range people[:min(len(people), peopleServersLimit)] {
			name, other := personNames(person)
			group.People = append(group.People, serverPersonRow{
				UserID: person.userID, Name: name, OtherNames: other, Messages: person.messages,
				FirstSeen: person.firstSeen.UTC().Format(time.RFC3339), LastSeen: person.lastSeen.UTC().Format(time.RFC3339),
				CardItems: person.cardItems, HasUserRow: person.hasUserRow,
			})
		}
		servers = append(servers, group)
	}
	return action.OK(req, map[string]any{"servers": servers, "total_count": total, "limit": peopleServersLimit, "query": query, "guild_id": guildFilter})
}

func discordGuildFromRoom(room string) string {
	parts := strings.SplitN(room, ":", 3)
	if len(parts) != 3 || parts[0] != "discord" || parts[1] == "" || parts[1] == "0" {
		return ""
	}
	return parts[1]
}

func parsePeopleTime(raw string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

func personMatchesQuery(person *serverPerson, query string) bool {
	if strings.Contains(strings.ToLower(person.userID), query) {
		return true
	}
	for label := range person.nameTimes {
		if strings.Contains(strings.ToLower(label), query) {
			return true
		}
	}
	return false
}

func personNames(person *serverPerson) (string, []string) {
	versions := make([]serverPersonName, 0, len(person.nameTimes))
	for label, at := range person.nameTimes {
		versions = append(versions, serverPersonName{label: label, at: at})
	}
	sort.Slice(versions, func(i, j int) bool {
		if !versions[i].at.Equal(versions[j].at) {
			return versions[i].at.After(versions[j].at)
		}
		return versions[i].label < versions[j].label
	})
	if len(versions) == 0 {
		return person.userID, []string{}
	}
	other := make([]string, 0, min(len(versions)-1, 5))
	for _, version := range versions[1:min(len(versions), 6)] {
		other = append(other, version.label)
	}
	return versions[0].label, other
}

func (s *Service) peopleServerNames() map[string]string {
	names := map[string]string{}
	if s.Discord != nil && s.Discord.Connected() {
		for _, guild := range s.Discord.Guilds() {
			if guild.ID != "" && guild.Name != "" {
				names[guild.ID] = guild.Name
			}
		}
	}
	for guildID := range s.Doc().Guilds {
		if names[guildID] == "" {
			names[guildID] = guildID
		}
	}
	return names
}

func serverPeopleUserIDs(db *storage.Store) (map[string]bool, error) {
	out := map[string]bool{}
	if !db.IsSQLite() {
		return out, nil // the users.list feature tables are SQLite-only too
	}
	queries := []string{
		`SELECT user_id FROM user_prefs`,
		`SELECT user_id FROM fortune_profiles`,
		`SELECT user_id FROM notice_subs`,
		`SELECT owner_id FROM reminders WHERE ` + reminderLive,
	}
	queries = append([]string{`SELECT user_id FROM consent_state`}, queries...)
	for _, query := range queries {
		rows, err := db.Reader().Query(query)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if id != "" {
				out[id] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// peopleScanRow is one (room, account, name) group of live Discord messages: counts and times only.
type peopleScanRow struct {
	room, id, label, first, last string
	count                        int64
}

type peopleScanCache struct {
	mu   sync.Mutex
	at   map[string]time.Time
	rows map[string][]peopleScanRow
}

const peopleScanFresh = time.Minute

// peopleScan groups the live Discord messages under prefix. One pass reads every stored Discord
// message's metadata (no text), so a result is reused for a minute: the page searches as people type.
func (s *Service) peopleScan(db *storage.Store, prefix string) ([]peopleScanRow, error) {
	s.peopleScans.mu.Lock()
	if at, ok := s.peopleScans.at[prefix]; ok && time.Since(at) < peopleScanFresh {
		out := s.peopleScans.rows[prefix]
		s.peopleScans.mu.Unlock()
		return out, nil
	}
	s.peopleScans.mu.Unlock()
	// a key range instead of LIKE: LIKE is case-insensitive in SQLite and would scan every stored
	// message (Kakao included); the range walks the room index. ';' sorts right after ':'.
	rows, err := db.Reader().Query(`SELECT o.room, o.actor_account, o.actor_label, COUNT(*), MIN(o.parsed_time), MAX(o.parsed_time)
		FROM memory_observations o
		WHERE o.room >= ? AND o.room < ? AND o.platform='discord' AND o.record_kind='human_text' AND o.deleted_at=''
			AND o.actor_account<>''
		GROUP BY o.room, o.actor_account, o.actor_label`, prefix, prefix[:len(prefix)-1]+";")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []peopleScanRow
	for rows.Next() {
		var r peopleScanRow
		if err := rows.Scan(&r.room, &r.id, &r.label, &r.count, &r.first, &r.last); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.peopleScans.mu.Lock()
	if s.peopleScans.at == nil || len(s.peopleScans.at) > 64 {
		s.peopleScans.at, s.peopleScans.rows = map[string]time.Time{}, map[string][]peopleScanRow{}
	}
	s.peopleScans.at[prefix], s.peopleScans.rows[prefix] = time.Now(), out
	s.peopleScans.mu.Unlock()
	return out, nil
}

// peopleWithdrawn is everyone who withdrew the memory consent (SQLite only, like the consent table).
func peopleWithdrawn(db *storage.Store) (map[string]bool, error) {
	out := map[string]bool{}
	if !db.IsSQLite() {
		return out, nil
	}
	rows, err := db.Reader().Query(`SELECT user_id FROM consent_state WHERE scope=? AND status=?`, privacy.Memory, privacy.Withdrawn)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
