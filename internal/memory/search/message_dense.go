package search

import "sort"

// messageDense scores every message vector of the allowed leaves against the question and returns the
// leaves in descending order of their best message, at most limit of them, with that best cosine as the
// leaf's score. It is the walk "sort the messages by cosine, take each leaf the first time it appears":
// a leaf's rank is fixed by its best message, and ties go to the message that comes first in the canonical
// row order (ProjectMessages: leaf order, then message order inside the leaf). A message in several
// leaves counts for each of them. scored is how many (message, leaf) rows were compared.
//
// q is the question vector of the leaf space; the message matrix may be narrower (message vectors use the
// width selected by the engine), so the question is cut to it and normalised again. No extra embedding
// call is made.
func (ix *Index) messageDense(q []float32, allowed Set, limit int) (leafIDs []string, scores map[string]float64, scored int) {
	m := ix.MsgVecs
	if m.Rows() == 0 || len(q) < m.Dims || limit <= 0 {
		return nil, nil, 0
	}
	mq := FitQuery(q, m.Dims)
	// pairs are (message, leaf) in the canonical order; idx[k] is the matrix row of the k-th scored pair and
	// leafAt[k] its leaf, so a message in several leaves is scored once per leaf it belongs to
	var ok []bool
	if allowed != nil {
		ok = make([]bool, len(ix.Leaves))
		for i, l := range ix.Leaves {
			ok[i] = allowed.Has(l.DocID)
		}
	}
	idx := make([]int, 0, len(ix.msgLeaf))
	leafAt := make([]int32, 0, len(ix.msgLeaf))
	for k, lf := range ix.msgLeaf {
		if ok == nil || ok[lf] {
			idx = append(idx, int(ix.msgRow[k]))
			leafAt = append(leafAt, lf)
		}
	}
	if len(idx) == 0 {
		return nil, nil, 0
	}
	sims := m.scores(mq, idx)
	bestK := make([]int, len(ix.Leaves)) // position in sims of the leaf's best message, -1 = none
	for i := range bestK {
		bestK[i] = -1
	}
	var order []int32
	for k, s := range sims {
		lf := leafAt[k]
		if bestK[lf] < 0 {
			bestK[lf] = k
			order = append(order, lf)
		} else if s > sims[bestK[lf]] {
			bestK[lf] = k
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := sims[bestK[order[a]]], sims[bestK[order[b]]]
		if sa != sb {
			return sa > sb
		}
		return bestK[order[a]] < bestK[order[b]]
	})
	if len(order) > limit {
		order = order[:limit]
	}
	leafIDs = make([]string, len(order))
	scores = make(map[string]float64, len(order))
	for i, lf := range order {
		id := ix.Leaves[lf].DocID
		leafIDs[i] = id
		scores[id] = sims[bestK[lf]]
	}
	return leafIDs, scores, len(sims)
}

// ProjectMessages is the canonical message→leaf mapping both the engine and the sandbox use. leafObs lists,
// for each leaf in leaf order, the observation IDs it holds in message order; has reports whether a
// message has a vector. It returns the pairs (message, leaf position) in that order — the leaf order, then
// the message order inside the leaf — so a message that sits in several leaves (overlapping recipes) is
// projected onto every one of them, and a leaf's score is the best cosine of its messages. The row order
// is also the tie-break of the run: among equal cosines the pair that comes first wins. It does not depend
// on observation IDs or times, only on the leaves.
func ProjectMessages(leafObs [][]string, has func(id string) bool) (ids []string, leafOf []int32) {
	for li, obs := range leafObs {
		for _, id := range obs {
			if has(id) {
				ids = append(ids, id)
				leafOf = append(leafOf, int32(li))
			}
		}
	}
	return ids, leafOf
}
