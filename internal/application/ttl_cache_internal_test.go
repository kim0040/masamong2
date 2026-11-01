package application

import (
	"fmt"
	"testing"
	"time"

	"masamong/internal/providers/openmeteo"
)

func TestTTLCacheExpiresAndStaysBounded(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	c := newTTLCache[int](3, time.Hour)
	c.put("a", 1, now)
	c.put("b", 2, now.Add(time.Minute))
	c.put("c", 3, now.Add(2*time.Minute))
	if v, at, ok := c.get("a", now.Add(30*time.Minute)); !ok || v != 1 || !at.Equal(now) {
		t.Fatalf("fresh entry: %v %v %v", v, at, ok)
	}
	if _, _, ok := c.get("a", now.Add(time.Hour)); ok {
		t.Fatal("an entry past its ttl was returned")
	}
	// full: the expired entries go first
	c.put("d", 4, now.Add(time.Hour+30*time.Second))
	if c.len() != 3 {
		t.Fatalf("len after the expired went: %d", c.len())
	}
	if _, _, ok := c.get("a", now.Add(time.Hour+31*time.Second)); ok {
		t.Fatal("expired entry survived the sweep")
	}
	// full of live entries: the oldest goes
	c.put("e", 5, now.Add(time.Hour+40*time.Second))
	c.put("f", 6, now.Add(time.Hour+50*time.Second))
	if c.len() != 3 {
		t.Fatalf("len = %d, want 3", c.len())
	}
	if _, _, ok := c.get("b", now.Add(time.Hour+51*time.Second)); ok {
		t.Fatal("the oldest live entry should have gone")
	}
	if _, _, ok := c.get("c", now.Add(time.Hour+51*time.Second)); ok {
		t.Fatal("the next oldest should have gone for the second newcomer")
	}
	// replacing a key never evicts another
	c.put("f", 7, now.Add(time.Hour+52*time.Second))
	if v, _, ok := c.get("e", now.Add(time.Hour+52*time.Second)); !ok || v != 5 {
		t.Fatal("replacing a key evicted a neighbour")
	}
}

// The two process-wide caches that used to grow with every place and every person: a flood of distinct keys
// leaves them at their caps.
func TestGeocodeAndCooldownCachesAreCapped(t *testing.T) {
	now := time.Now()
	for i := 0; i < geocodeCacheMax+500; i++ {
		geocodeCache.put(fmt.Sprintf("en|capped-test-%d", i), openmeteo.Place{Name: "x"}, now)
	}
	if n := geocodeCache.len(); n > geocodeCacheMax {
		t.Fatalf("geocode cache holds %d places, cap %d", n, geocodeCacheMax)
	}
	for i := 0; i < zodiacCooldownMax+500; i++ {
		zodiacCooldown.put(fmt.Sprintf("capped-test-user-%d", i), struct{}{}, now)
	}
	if n := zodiacCooldown.len(); n > zodiacCooldownMax {
		t.Fatalf("cooldown table holds %d people, cap %d", n, zodiacCooldownMax)
	}
}
