package privacy

import "testing"

func TestAllowsFollowsKindAndVersion(t *testing.T) {
	mem, _ := Get(Memory)
	if !mem.Allows(State{}) || mem.Allows(State{Status: Withdrawn}) {
		t.Fatal("memory is allowed until withdrawn")
	}
	f, _ := Get(Fortune)
	if f.Allows(State{}) || f.Allows(State{Status: Withdrawn, Version: f.Version}) {
		t.Fatal("fortune needs a grant")
	}
	if !f.Allows(State{Status: Granted, Version: f.Version}) {
		t.Fatal("a current grant allows")
	}
	old := State{Status: Granted, Version: "2020-01-01.v1"}
	if f.Allows(old) || !f.Stale(old) {
		t.Fatal("a grant of an older policy version is stale")
	}
}

func TestNormalizeAliases(t *testing.T) {
	words := map[string]string{"운세": Fortune, "학교공지": SchoolNotice, "편입": TransferNotice, "기억": Memory}
	for in, want := range map[string]string{"운세": Fortune, " 학교 공지 ": SchoolNotice, "편입": TransferNotice, "기억": Memory, "MEMORY": Memory, "school-notice": SchoolNotice} {
		if got, ok := Normalize(in, words); !ok || got != want {
			t.Fatalf("%q -> %q %v", in, got, ok)
		}
	}
	if _, ok := Normalize("주민번호", words); ok {
		t.Fatal("unknown scopes are refused")
	}
}
