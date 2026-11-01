package corpus

import (
	"errors"
)

// KakaoRowIdentity uses the time and text as storage sees them, independent of file bytes.
func KakaoRowIdentity(room, at, speaker, text string, occurrence int) string {
	return "kakao-row-v1:" + sha256Hex(canonicalJSON(room, at, speaker, text, occurrence))
}

type kakaoGroup struct {
	at     string
	counts map[string]int
}

// KakaoOccurrences counts interleaved identical rows in file order. Each room keeps only its current
// timestamp group. Reject an exceptional group rather than silently losing rows or using unbounded RAM.
type KakaoOccurrences struct {
	rooms   map[string]*kakaoGroup
	entries int
}

var ErrKakaoGroupTooLarge = errors.New("kakao timestamp groups exceed bounded occurrence state")

func NewKakaoOccurrences() *KakaoOccurrences {
	return &KakaoOccurrences{rooms: map[string]*kakaoGroup{}}
}

func (c *KakaoOccurrences) Next(room, at, speaker, text string) (int, error) {
	g := c.rooms[room]
	if g == nil || g.at != at {
		if g == nil && len(c.rooms) >= 10000 {
			return 0, ErrKakaoGroupTooLarge
		}
		if g != nil {
			c.entries -= len(g.counts)
		}
		g = &kakaoGroup{at: at, counts: map[string]int{}}
		c.rooms[room] = g
	}
	k := sha256Hex(canonicalJSON(speaker, text))
	if g.counts[k] == 0 {
		if c.entries >= 100000 {
			return 0, ErrKakaoGroupTooLarge
		}
		c.entries++
	}
	n := g.counts[k]
	g.counts[k] = n + 1
	return n, nil
}
