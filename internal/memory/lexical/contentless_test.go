package lexical

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// The index keeps no copy of the text (contentless FTS5). Hits, bm25 scores and tie order must be
// exactly those of the earlier table that stored the text and a doc_id column.
func TestContentlessMatchesStoredTable(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	words := []string{"캠핑", "가평", "마라탕", "시험", "철수가", "영희랑", "토요일", "camping", "Seoul", "🎉", "ㅋㅋㅋ", "어디서", "하기로", "했지", "고기는", "텐트"}
	var docs []Doc
	for i := 0; i < 600; i++ {
		var b strings.Builder
		for w := 0; w < 3+r.IntN(40); w++ {
			b.WriteString(words[r.IntN(len(words))] + " ")
		}
		docs = append(docs, Doc{ID: fmt.Sprintf("leaf-%03d", i), Text: b.String()})
	}
	ix, err := NewIndex(2)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	// two batches: rowids continue across AddMany calls
	if err := ix.AddMany(docs[:250]); err != nil {
		t.Fatal(err)
	}
	if err := ix.AddMany(docs[250:]); err != nil {
		t.Fatal(err)
	}
	ref, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer ref.Close()
	ref.SetMaxOpenConns(1)
	if _, err := ref.Exec(`CREATE VIRTUAL TABLE docs USING fts5(doc_id UNINDEXED, body, tokenize='unicode61')`); err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		if _, err := ref.Exec(`INSERT INTO docs(doc_id, body) VALUES (?, ?)`, d.ID, ix.Tok.ToNgrams(d.Text)); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	queries := []string{"캠핑 어디서 하기로 했지", "가평", "마라탕 누가", "camping Seoul", "🎉 ㅋㅋㅋ", "철수가 텐트", "없는말", "시험 토요일 고기는"}
	for _, q := range queries {
		got := ix.Search(q, 0)
		rows, err := ref.Query(`SELECT doc_id, bm25(docs) FROM docs WHERE docs MATCH ? ORDER BY bm25(docs)`, ix.Tok.MatchExpression(q))
		if err != nil {
			t.Fatal(err)
		}
		var want []Hit
		for rows.Next() {
			var h Hit
			_ = rows.Scan(&h.DocID, &h.Score)
			want = append(want, h)
		}
		rows.Close()
		if len(got) != len(want) {
			t.Fatalf("%q: %d hits, want %d", q, len(got), len(want))
		}
		total += len(want)
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%q #%d: %+v, want %+v", q, i, got[i], want[i])
			}
		}
	}
	if total < 1000 {
		t.Fatalf("only %d hits compared", total)
	}
	if ix.Len() != len(docs) {
		t.Fatalf("len %d", ix.Len())
	}
}
