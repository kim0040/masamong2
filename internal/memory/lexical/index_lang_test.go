package lexical

import "testing"

func TestTokenizerScriptsAndLatinWords(t *testing.T) {
	zh := NewTokenizer(2, []string{"han"}, true, []string{"what"})
	if got := zh.ToNgrams("我们去露营 camping2 trip!"); got != "我们 们去 去露 露营 camping2 trip !" {
		t.Fatal(got)
	}
	if got := zh.QueryWords("What about camping"); len(got) != 2 {
		t.Fatalf("%v", got)
	}
	ix, err := NewIndexWith(zh)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	_ = ix.AddMany([]Doc{{"a", "we went camping at the lake"}, {"b", "decamping is a word"}, {"c", "周末去露营吧"}})
	hits := ix.Search("camping", 10)
	if len(hits) != 1 || hits[0].DocID != "a" {
		t.Fatalf("latin words must match whole words: %+v", hits)
	}
	if h := ix.Search("露营", 10); len(h) != 1 || h[0].DocID != "c" {
		t.Fatalf("han bigram: %+v", h)
	}
	if DefaultTokenizer().ToNgrams("camping") != "c a m p i n g" {
		t.Fatal("default tokenizer must keep sandbox behaviour")
	}
}
