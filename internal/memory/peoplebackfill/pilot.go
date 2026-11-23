package peoplebackfill

import (
	"sort"
)

// PilotSize is the number of windows a pilot sends when the caller names none (the contract's 60).
const PilotSize = 60

// SelectPilot picks n windows (skipped ones are never picked) as a deterministic spread across
// rooms and time: every room with windows gives at least one while n allows, the rest go to the
// rooms in proportion to how many windows they have (largest remainder), and a room's picks are
// spaced evenly across its timeline. The same input always picks the same windows; the result is
// ordered by room and time.
func SelectPilot(windows []Window, n int) []Window {
	if n <= 0 {
		n = PilotSize
	}
	byRoom := map[string][]Window{}
	var rooms []string
	for _, w := range windows {
		if w.Skipped {
			continue
		}
		if _, ok := byRoom[w.Room]; !ok {
			rooms = append(rooms, w.Room)
		}
		byRoom[w.Room] = append(byRoom[w.Room], w)
	}
	sort.Strings(rooms)
	total := 0
	sizes := make([]int, len(rooms))
	for i, r := range rooms {
		sort.SliceStable(byRoom[r], func(a, b int) bool {
			x, y := byRoom[r][a], byRoom[r][b]
			if !x.First.Equal(y.First) {
				return x.First.Before(y.First)
			}
			return x.ID < y.ID
		})
		sizes[i] = len(byRoom[r])
		total += sizes[i]
	}
	if total <= n {
		out := make([]Window, 0, total)
		for _, r := range rooms {
			out = append(out, byRoom[r]...)
		}
		return out
	}
	quota := make([]int, len(rooms))
	if n < len(rooms) {
		// fewer picks than rooms: the rooms themselves are spread evenly, one window each
		for i := 0; i < n; i++ {
			quota[i*len(rooms)/n] = 1
		}
	} else {
		apportion(n, sizes, quota)
	}
	var out []Window
	for i, r := range rooms {
		ws := byRoom[r]
		for k := 0; k < quota[i]; k++ {
			out = append(out, ws[(2*k+1)*len(ws)/(2*quota[i])])
		}
	}
	return out
}

// apportion splits n picks over sizes (one per room first, the rest by proportional largest
// remainder, rooms in index order on a tie) into quota. Every room gets at least one while n
// allows, and never more than it has. Callers pass n > len(sizes) and n < sum(sizes).
func apportion(n int, sizes, quota []int) {
	left := n
	for i, s := range sizes {
		if s > 0 && left > 0 {
			quota[i] = 1
			left--
		}
	}
	for left > 0 {
		headroom := 0
		for i, s := range sizes {
			headroom += s - quota[i]
		}
		if headroom == 0 {
			return // every window is already picked
		}
		if left >= headroom {
			for i, s := range sizes {
				left -= s - quota[i]
				quota[i] = s
			}
			return
		}
		assigned := 0
		remainder := make([]int, len(sizes))
		for i, s := range sizes {
			h := s - quota[i]
			share := left * h / headroom
			remainder[i] = left * h % headroom
			quota[i] += share
			assigned += share
		}
		left -= assigned
		// the unassigned picks go by largest remainder, one per room (each fits: share < headroom)
		order := make([]int, len(sizes))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool {
			if remainder[order[a]] != remainder[order[b]] {
				return remainder[order[a]] > remainder[order[b]]
			}
			return order[a] < order[b]
		})
		for _, i := range order {
			if left == 0 {
				return
			}
			if quota[i] < sizes[i] {
				quota[i]++
				left--
			}
		}
	}
}
