// Package blocks implements the memory block/view identity contract in
// contracts/memory/blocks.json. Retrieval is not implemented here.
package blocks

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const UnpreparedSegmentation = "unprepared"

type Block struct {
	ID                  string         `json:"block_id"`
	Platform            string         `json:"platform"`
	Collection          string         `json:"collection"`
	SourceRoom          string         `json:"source_room"`
	Label               string         `json:"label"`
	SegmentationVersion string         `json:"segmentation_version"`
	EmbeddingSpace      string         `json:"embedding_space"`
	MessageCount        int            `json:"message_count,omitempty"`
	LeafCount           int            `json:"leaf_count,omitempty"`
	Status              string         `json:"status"`
	PreparedAt          string         `json:"prepared_at,omitempty"`
	CostUSD             float64        `json:"cost_usd,omitempty"`
	Error               string         `json:"error,omitempty"`
	Hints               map[string]any `json:"hints,omitempty"`
	TimeSemantics       string         `json:"time_semantics"`
}

type View struct {
	ChannelKey       string   `json:"channel_key"`
	Blocks           []string `json:"blocks"`
	Revision         int      `json:"revision"`
	ScopeFingerprint string   `json:"scope_fingerprint"`
	UpdatedAt        string   `json:"updated_at,omitempty"`
	UpdatedBy        string   `json:"updated_by,omitempty"`
}

func EmbeddingSpace(provider, model string, dimension int, normalization string) string {
	if normalization == "" {
		normalization = "l2"
	}
	return strings.Join([]string{provider, model, strconv.Itoa(dimension), normalization}, "|")
}

func BlockID(collection, sourceRoom, segmentationVersion, embeddingSpace string) string {
	payload := strings.Join([]string{collection, sourceRoom, segmentationVersion, embeddingSpace}, "\n")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])[:16]
}

func ScopeFingerprint(blockIDs []string, revision int) string {
	ids := uniqueSorted(blockIDs)
	parts := append(append([]string{}, ids...), strconv.Itoa(revision))
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func PlatformOfRoom(sourceRoom string) string {
	if strings.HasPrefix(sourceRoom, "kakao:") {
		return "kakao"
	}
	return "discord"
}

func TimeSemantics(platform string) string {
	if platform == "kakao" {
		return "naive"
	}
	return "offset"
}

func LabelForRoom(sourceRoom string) string {
	parts := strings.Split(sourceRoom, ":")
	if len(parts) >= 3 && parts[0] == "discord" {
		id := parts[len(parts)-1]
		if len(id) > 6 {
			id = id[len(id)-6:]
		}
		return "#" + id
	}
	if strings.HasPrefix(sourceRoom, "kakao:") {
		return "kakao-room"
	}
	if len(sourceRoom) > 12 {
		return sourceRoom[len(sourceRoom)-12:]
	}
	return sourceRoom
}

func MakeView(channelKey string, blockIDs []string, revision int) View {
	ids := uniqueSorted(blockIDs)
	return View{
		ChannelKey:       channelKey,
		Blocks:           ids,
		Revision:         revision,
		ScopeFingerprint: ScopeFingerprint(ids, revision),
	}
}

func AssertFingerprint(v View) error {
	if ScopeFingerprint(v.Blocks, v.Revision) != v.ScopeFingerprint {
		return fmt.Errorf("stale_scope")
	}
	return nil
}

func AssertSameEmbeddingSpace(items []Block) error {
	seen := map[string]struct{}{}
	for _, b := range items {
		if b.EmbeddingSpace == "" {
			continue
		}
		seen[b.EmbeddingSpace] = struct{}{}
	}
	if len(seen) > 1 {
		return fmt.Errorf("view members must share embedding_space")
	}
	return nil
}

func Attach(v View, add Block, bound []Block) (View, error) {
	catalog := make([]Block, 0, len(bound)+1)
	replaced := false
	for _, b := range bound {
		if b.ID == add.ID {
			catalog = append(catalog, add)
			replaced = true
			continue
		}
		catalog = append(catalog, b)
	}
	if !replaced {
		catalog = append(catalog, add)
	}
	if err := AssertSameEmbeddingSpace(catalog); err != nil {
		return View{}, err
	}
	ids := append(append([]string{}, v.Blocks...), add.ID)
	out := MakeView(v.ChannelKey, ids, v.Revision+1)
	out.UpdatedAt = v.UpdatedAt
	out.UpdatedBy = v.UpdatedBy
	return out, nil
}

func Detach(v View, blockID string) View {
	next := make([]string, 0, len(v.Blocks))
	for _, id := range v.Blocks {
		if id != blockID {
			next = append(next, id)
		}
	}
	out := MakeView(v.ChannelKey, next, v.Revision+1)
	out.UpdatedAt = v.UpdatedAt
	out.UpdatedBy = v.UpdatedBy
	return out
}

func ChannelKey(guildID, channelID string) string {
	return "discord:" + guildID + ":" + channelID
}

func uniqueSorted(ids []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
