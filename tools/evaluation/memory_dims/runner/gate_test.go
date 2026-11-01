package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"masamong/internal/memory/search"
)

func TestRerankTapeExactRequestReplay(t *testing.T) {
	record := &rerankTape{Mode: "record", Entries: map[string]search.RerankResult{}, Provider: deterministicRerank{}}
	a, err := record.Rerank(context.Background(), "synthetic", []string{"one", "two"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	replay := &rerankTape{Mode: "replay", Entries: record.Entries}
	b, err := replay.Rerank(context.Background(), "synthetic", []string{"one", "two"}, 2)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("identical request did not replay identical response")
	}
	for _, change := range []struct {
		q    string
		docs []string
		n    int
	}{
		{"different", []string{"one", "two"}, 2},
		{"synthetic", []string{"two", "one"}, 2},
		{"synthetic", []string{"one", "two"}, 1},
	} {
		if _, err := replay.Rerank(context.Background(), change.q, change.docs, change.n); err == nil {
			t.Fatal("distinct rerank request reused response")
		}
	}
	if replay.Missing != 3 {
		t.Fatal("missing requests were hidden by fallback")
	}
}

func TestScratchPathUsesRepositoryRoot(t *testing.T) {
	root, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := scratchPath(filepath.Join(".masamong-scratch", "root-relative.json")); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "tools", "evaluation", "memory_dims", "runner", ".masamong-scratch", "nested.json")
	if err := scratchPath(nested); err == nil {
		t.Fatal("package-local scratch path was accepted")
	}
}

// A resident sequence writes <out>.stepN.jsonl: -out .masamong-scratch must not put the steps (which
// hold observation IDs) beside the folder, outside what Git ignores.
func TestResidentStepPathStaysInScratch(t *testing.T) {
	if p, err := residentStepPath(filepath.Join(".masamong-scratch", "gate", "seq.jsonl"), 0); err != nil || filepath.Base(p) != "seq.step1.jsonl" {
		t.Fatalf("step in scratch: %q %v", p, err)
	}
	if p, err := residentStepPath(".masamong-scratch", 1); err == nil {
		t.Fatalf("step beside the scratch folder accepted: %q", p)
	}
}

func TestScratchPathRejectsSymlinksAndDanglingLinks(t *testing.T) {
	root := t.TempDir()
	scratch := filepath.Join(root, ".masamong-scratch")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "outside.txt")
	if err := os.WriteFile(target, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(scratch, "escape")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := scratchPathAt(root, filepath.Join(link, "output.jsonl")); err == nil {
		t.Fatal("symlink output path was accepted")
	}
	broken := filepath.Join(scratch, "broken")
	if err := os.Symlink(filepath.Join(root, "missing-target"), broken); err != nil {
		t.Skipf("dangling symlink creation unavailable: %v", err)
	}
	if err := scratchPathAt(root, filepath.Join(broken, "output.jsonl")); err == nil {
		t.Fatal("dangling symlink output path was accepted")
	}
}

func TestScratchPathRejectsWindowsJunction(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows junction test")
	}
	root := t.TempDir()
	scratch := filepath.Join(root, ".masamong-scratch")
	target := filepath.Join(root, "outside")
	if err := os.MkdirAll(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(scratch, "junction")
	if output, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Skipf("junction creation unavailable: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() { _ = os.Remove(junction) })
	// This is the same output-path check dump-candidates performs before opening -out.
	if err := scratchPathAt(root, filepath.Join(junction, "dump.jsonl")); err == nil {
		t.Fatal("junction output path was accepted")
	}
}

func captureCompareOutput(t *testing.T, left, right string, failOnRegress bool) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	compareErr := compareCandidates(left, right, "", failOnRegress)
	_ = writer.Close()
	os.Stdout = previous
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(output), compareErr
}

func TestSyntheticCandidateGate(t *testing.T) {
	repoRoot, err := repositoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(repoRoot, ".masamong-scratch")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(base, "gate-test-")
	if err != nil {
		t.Fatal(err)
	}
	// root is allocated by MkdirTemp under the literal scratch directory and
	// validated before removal, so cleanup cannot target another workspace.
	t.Cleanup(func() {
		if scratchPath(root) == nil {
			_ = os.RemoveAll(root)
		}
	})
	data := filepath.Join(root, "data")
	if err := syntheticRoot(data); err != nil {
		t.Fatal(err)
	}
	gold, tape := filepath.Join(data, "gold.json"), filepath.Join(root, "tape.json")
	paths := map[string]string{}
	for _, name := range []string{"full", "replay", "all", "partial", "back"} {
		paths[name] = filepath.Join(root, name+".jsonl")
	}
	dump := func(name, resident, mode string) {
		t.Helper()
		if err := dumpCandidates(data, gold, paths[name], resident, tape, mode, true, []string{"hybrid", "rerank", "auto"}); err != nil {
			t.Fatal(err)
		}
	}
	dump("full", "", "record")
	dump("replay", "", "replay")
	dump("all", "discord:gate:A,discord:gate:B,discord:gate:C", "replay")
	full, err := readCandidateRows(paths["full"])
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range full {
		if !row.Correct {
			t.Fatalf("synthetic baseline failed: %s/%s", row.ID, row.Mode)
		}
	}
	for _, name := range []string{"replay", "all"} {
		rows, err := readCandidateRows(paths[name])
		if err != nil || !reflect.DeepEqual(full, rows) {
			t.Fatal("repeat or full-set filter changed candidates/hits")
		}
		if err := compareCandidates(paths["full"], paths[name], "", false); err != nil {
			t.Fatal(err)
		}
	}
	dump("partial", "discord:gate:A,discord:gate:B", "record")
	// Add C (all), then remove it again. Replay the union of requests collected
	// during the pair preparation; provider calls are fake throughout this test.
	dump("all", "discord:gate:A,discord:gate:B,discord:gate:C", "replay")
	dump("back", "discord:gate:A,discord:gate:B", "replay")
	part, err := readCandidateRows(paths["partial"])
	if err != nil {
		t.Fatal(err)
	}
	back, err := readCandidateRows(paths["back"])
	if err != nil || !reflect.DeepEqual(part, back) {
		t.Fatal("residency round trip changed candidates")
	}
	// The same round trip inside one process: each step rebuilds from the one before it (the product reuses
	// rows that way) and must equal the independent build of its set.
	setA, setAB, setABC := []string{"discord:gate:A", "discord:gate:B"}, []string{"discord:gate:A", "discord:gate:B"}, []string{"discord:gate:A", "discord:gate:B", "discord:gate:C"}
	residentSequence = [][]string{setAB, setABC, setA}
	t.Cleanup(func() { residentSequence = nil })
	if err := dumpCandidates(data, gold, filepath.Join(root, "seq.jsonl"), "", tape, "replay", true, []string{"hybrid", "rerank", "auto"}); err != nil {
		t.Fatal(err)
	}
	all3, err := readCandidateRows(paths["all"])
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []map[string]candidateRow{part, all3, part} {
		got, err := readCandidateRows(filepath.Join(root, fmt.Sprintf("seq.step%d.jsonl", i+1)))
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatalf("in-process sequence step %d differs from the independent build", i+1)
		}
	}
	residentSequence = nil
	for _, mode := range []string{"hybrid", "rerank", "auto"} {
		if !full["gate-partial/"+mode].Correct || part["gate-partial/"+mode].Correct {
			t.Fatal("missing resident C did not produce expected regression")
		}
		if !part["gate-alias/"+mode].Correct {
			t.Fatal("out-of-scope alias was lost")
		}
		if full["gate-empty-scope/"+mode].ScopeKind == full["gate-nonexistent-room/"+mode].ScopeKind {
			t.Fatal("empty scope conflated with missing room")
		}
	}
	if !full["gate-auto/auto"].Recovery || len(full["gate-auto/auto"].Stages) != 2 {
		t.Fatal("default auto confidence/recovery path not covered")
	}
	if err := compareCandidates(paths["full"], paths["partial"], filepath.Join(root, "compare.json"), false); err != nil {
		t.Fatal(err)
	}
	if err := compareCandidates(paths["full"], paths["partial"], "", true); err == nil {
		t.Fatal("fail-on-regress accepted a regression")
	}
	if err := compareCandidates(paths["partial"], paths["full"], "", true); err != nil {
		t.Fatalf("fail-on-regress rejected improvements: %v", err)
	}
	if output, err := captureCompareOutput(t, paths["partial"], paths["full"], false); err != nil || !strings.Contains(output, "improved=3") {
		t.Fatalf("swapped comparison did not report improved=3: %v (%s)", err, output)
	}
	unknownRooms := "missing-room-one,missing-room-two"
	err = dumpCandidates(data, gold, filepath.Join(root, "unknown.jsonl"), unknownRooms, "", "off", true, []string{"hybrid"})
	if err == nil || !strings.Contains(err.Error(), "2 resident room(s) not found") || strings.Contains(err.Error(), "missing-room-one") || strings.Contains(err.Error(), "missing-room-two") {
		t.Fatalf("unknown resident rooms were not rejected safely: %v", err)
	}
	if err := scratchPath(filepath.Join("..", "outside.json")); err == nil {
		t.Fatal("private output accepted outside scratch")
	}
	if err := dumpCandidates(data, gold, filepath.Join(root, "off.jsonl"), "none", "", "off", true, []string{"hybrid", "rerank", "auto"}); err != nil {
		t.Fatal("empty residency or rerank off failed", err)
	}
}

type costlyRerank struct{ deterministicRerank }

func (costlyRerank) Rerank(ctx context.Context, q string, docs []string, n int) (search.RerankResult, error) {
	r, err := deterministicRerank{}.Rerank(ctx, q, docs, n)
	r.CostUSD = 0.03
	return r, err
}

func TestRerankTapeSpendCap(t *testing.T) {
	tape := &rerankTape{Mode: "record", Entries: map[string]search.RerankResult{}, Provider: costlyRerank{}, SpendCap: 0.05}
	for i := 0; i < 2; i++ {
		if _, err := tape.Rerank(context.Background(), "q", []string{"a", fmt.Sprint(i)}, 2); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tape.Rerank(context.Background(), "q", []string{"a", "c"}, 2); err == nil || tape.Failures != 1 {
		t.Fatal("recording continued past the spend cap")
	}
	if tape.Spent < 0.0599 || tape.Spent > 0.0601 {
		t.Fatalf("spent %.4f, want 0.06", tape.Spent)
	}
	for _, e := range tape.Entries {
		if e.CostUSD != 0 {
			t.Fatal("tape kept a cost")
		}
	}
}
