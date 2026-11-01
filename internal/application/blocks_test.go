package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
)

func practiceSvc(t *testing.T) *application.Service {
	t.Helper()
	p, err := application.OpenPractice(context.Background(), t.TempDir(), "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func writeKakao(t *testing.T, dir, name string, rows int) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("\ufeffDate,User,Message\n")
	start := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	who := []string{"민지", "철수", "영희"}
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "%s,%s,\"동아리 회의 %d번째 이야기 \"\"따옴표\"\", 쉼표 포함\"\n", start.Add(time.Duration(i)*7*time.Minute).Format("2006-01-02 15:04:05"), who[i%3], i)
	}
	p := filepath.Join(dir, "imports", name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func findBlock(t *testing.T, svc *application.Service, room string) map[string]any {
	t.Helper()
	list := exec(svc, "memory.blocks.list", nil)
	if !list.OK {
		t.Fatal(list.Error)
	}
	raw, _ := json.Marshal(list.Output["blocks"])
	var blocks []map[string]any
	_ = json.Unmarshal(raw, &blocks)
	for _, b := range blocks {
		if b["source_room"] == room {
			return b
		}
	}
	return nil
}

func waitJob(t *testing.T, svc *application.Service, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second) // -race on a slow runner takes a while for the 3000-row prepare
	for time.Now().Before(deadline) {
		r := exec(svc, "jobs.get", map[string]any{"id": id})
		if r.OK {
			switch r.Output["status"] {
			case "succeeded", "failed", "cancelled":
				return r.Output
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

// The whole block flow an operator follows in the console: import a CSV → it is a block → estimate
// (cost and time) → prepare in the background with progress → rename and annotate → connect to a
// channel (the channel's memory scope grows) → delete (detached everywhere, source file untouched).
func TestBlockLifecycle(t *testing.T) {
	svc := practiceSvc(t)
	csv := writeKakao(t, svc.DataRoot, "club.csv", 120)
	if r := exec(svc, "imports.start", map[string]any{"source": "kakao_csv", "path": csv, "room": "kakao:club", "label": "동아리"}); !r.OK {
		t.Fatalf("import %+v", r.Error)
	}
	b := findBlock(t, svc, "kakao:club")
	if b == nil || b["status"] != "imported" || b["message_count"].(float64) != 120 || b["speakers"].(float64) != 3 {
		t.Fatalf("imported block %+v", b)
	}
	if ch, ok := b["channels"].([]any); !ok || len(ch) != 0 {
		t.Fatalf("an unconnected block lists channels as [] (never null): %#v", b["channels"])
	}
	if !strings.HasPrefix(fmt.Sprint(b["first"]), "2025-03-01") {
		t.Fatalf("span %v", b["first"])
	}
	est := exec(svc, "memory.blocks.prepare", map[string]any{"source_room": "kakao:club", "dry_run": true})
	if !est.OK {
		t.Fatal(est.Error)
	}
	rep := jsonShape(est.Output["report"])
	e := jsonShape(rep["estimate"])
	if e["leaf_embeds"].(float64) == 0 || e["seconds"] == nil || e["embed_tokens"].(float64) == 0 {
		t.Fatalf("estimate must count work, tokens and time: %+v", e)
	}
	started := exec(svc, "memory.blocks.prepare", map[string]any{"source_room": "kakao:club", "background": true, "budget_usd": "1"})
	if !started.OK {
		t.Fatal(started.Error)
	}
	jobID := started.JobID
	again := exec(svc, "memory.blocks.prepare", map[string]any{"source_room": "kakao:club", "background": true})
	if again.OK && again.JobID != jobID && again.Output["already_running"] != true {
		// the first job may already be done; a second run is then a new job, which is fine
		waitJob(t, svc, again.JobID)
	}
	done := waitJob(t, svc, jobID)
	if done["status"] != "succeeded" {
		t.Fatalf("job %+v", done)
	}
	b = findBlock(t, svc, "kakao:club")
	if b["status"] != "ready" || b["pending_leaves"].(float64) != 0 || b["job"] == nil {
		t.Fatalf("prepared block %+v", b)
	}

	// rename and annotate; bad input is refused
	for _, bad := range []map[string]any{{"label": ""}, {"label": strings.Repeat("가", 81)}, {"label": "두\n줄"}, {"note": strings.Repeat("x", 501)}, {"budget_usd": "-1"}, {}} {
		bad["source_room"] = "kakao:club"
		if r := exec(svc, "memory.blocks.update", bad); r.OK {
			t.Fatalf("update %v must be refused", bad)
		}
	}
	if r := exec(svc, "memory.blocks.update", map[string]any{"source_room": "kakao:club", "label": "동아리 <b>2025</b>", "note": "봄 학기", "auto_update": false}); !r.OK {
		t.Fatal(r.Error)
	}
	b = findBlock(t, svc, "kakao:club")
	if b["label"] != "동아리 <b>2025</b>" || b["note"] != "봄 학기" || b["auto_update"] != false {
		t.Fatalf("updated %+v", b)
	}

	// connect to the practice channel: the channel now searches the block's room too
	blockID := b["block_id"].(string)
	g, c := application.PracticeGuild, application.PracticeChannel
	if r := exec(svc, "channels.view.attach", map[string]any{"guild_id": g, "channel_id": c, "block_id": blockID}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := exec(svc, "channels.view.attach", map[string]any{"guild_id": g, "channel_id": c, "block_id": blockID}); r.OK {
		t.Fatal("attaching twice must be refused")
	}
	own := findBlock(t, svc, "discord:"+g+":"+c)
	if own != nil {
		if r := exec(svc, "channels.view.attach", map[string]any{"guild_id": g, "channel_id": c, "block_id": own["block_id"]}); r.OK {
			t.Fatal("a channel's own room is always searched; attaching it must be refused")
		}
	}
	view := exec(svc, "channels.view.show", map[string]any{"guild_id": g, "channel_id": c})
	if !strings.Contains(fmt.Sprint(view.Output["rooms"]), "kakao:club") {
		t.Fatalf("scope %v", view.Output["rooms"])
	}
	b = findBlock(t, svc, "kakao:club")
	if len(b["channels"].([]any)) != 1 {
		t.Fatalf("block must list the channel using it: %+v", b["channels"])
	}
	hits := exec(svc, "memory.search", map[string]any{"query": "동아리 회의", "guild_id": g, "channel_id": c, "ranking": "hybrid"})
	if !hits.OK || !strings.Contains(fmt.Sprint(hits.Output["hits"]), "동아리") {
		t.Fatalf("search through the connected block: %+v", hits)
	}

	// delete needs the name typed back; it detaches everywhere and keeps the file
	if r := exec(svc, "memory.blocks.delete", map[string]any{"source_room": "kakao:club", "confirm": "wrong"}); r.OK {
		t.Fatal("wrong confirmation must be refused")
	}
	del := exec(svc, "memory.blocks.delete", map[string]any{"source_room": "kakao:club", "confirm": "동아리 <b>2025</b>"})
	if out := jsonShape(del.Output); !del.OK || out["messages_removed"].(float64) != 120 || out["channels_detached"].(float64) != 1 {
		t.Fatalf("delete %+v", del)
	}
	if findBlock(t, svc, "kakao:club") != nil {
		t.Fatal("deleted block still listed")
	}
	if _, err := os.Stat(csv); err != nil {
		t.Fatal("the source file must stay")
	}
	view = exec(svc, "channels.view.show", map[string]any{"guild_id": g, "channel_id": c})
	if strings.Contains(fmt.Sprint(view.Output["rooms"]), "kakao:club") {
		t.Fatal("deleted block still in the channel scope")
	}
}

// A job cancelled from the console stops and stays cancelled; retry runs it again to success.
func TestPrepareJobCancelAndRetry(t *testing.T) {
	svc := practiceSvc(t)
	csv := writeKakao(t, svc.DataRoot, "big.csv", 3000)
	if r := exec(svc, "imports.start", map[string]any{"source": "kakao_csv", "path": csv, "room": "kakao:big"}); !r.OK {
		t.Fatal(r.Error)
	}
	started := exec(svc, "memory.blocks.prepare", map[string]any{"source_room": "kakao:big", "background": true})
	if !started.OK {
		t.Fatal(started.Error)
	}
	cancel := exec(svc, "jobs.cancel", map[string]any{"id": started.JobID})
	if !cancel.OK {
		t.Fatal(cancel.Error)
	}
	got := waitJob(t, svc, started.JobID)
	if got["status"] != "cancelled" {
		t.Fatalf("cancelled job must stay cancelled: %+v", got)
	}
	time.Sleep(1500 * time.Millisecond) // the runner notices within a second
	if r := exec(svc, "jobs.get", map[string]any{"id": started.JobID}); r.Output["status"] != "cancelled" {
		t.Fatalf("runner overwrote the cancel: %v", r.Output["status"])
	}
	retry := exec(svc, "jobs.retry", map[string]any{"id": started.JobID})
	if !retry.OK {
		t.Fatal(retry.Error)
	}
	if got := waitJob(t, svc, started.JobID); got["status"] != "succeeded" {
		t.Fatalf("retried job %+v", got)
	}
	if b := findBlock(t, svc, "kakao:big"); b["status"] != "ready" {
		t.Fatalf("block after retry %+v", b)
	}
}

// Odd inputs fail with a clear error instead of a partial block.
func TestBlockPrepareRefusesBadInput(t *testing.T) {
	svc := practiceSvc(t)
	for _, in := range []map[string]any{
		{"source_room": "kakao:nothere"},
		{"block_id": "0000000000000000"},
		{},
		{"source_room": "kakao:practice", "budget_usd": "5000"},
		{"source_room": "kakao:practice", "limit": -3},
	} {
		if r := exec(svc, "memory.blocks.prepare", in); r.OK {
			t.Fatalf("prepare %v must be refused", in)
		}
	}
	if r := exec(svc, "memory.blocks.update", map[string]any{"source_room": "kakao:nothere", "label": "x"}); r.OK {
		t.Fatal("update of an unknown block must be refused")
	}
	if r := exec(svc, "memory.blocks.delete", map[string]any{"source_room": "kakao:nothere", "confirm": "kakao:nothere"}); r.OK {
		t.Fatal("delete of an unknown block must be refused")
	}
}

// jsonShape turns an Action output value into its JSON shape, as the console receives it.
func jsonShape(v any) map[string]any {
	raw, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

var _ = action.Request{}

// A service stop while a job runs records "interrupted" (never a closed-database error), and the next
// start offers the job for retry.
func TestPrepareJobInterruptedByShutdown(t *testing.T) {
	svc := practiceSvc(t)
	csv := writeKakao(t, svc.DataRoot, "stop.csv", 3000)
	if r := exec(svc, "imports.start", map[string]any{"source": "kakao_csv", "path": csv, "room": "kakao:stop"}); !r.OK {
		t.Fatal(r.Error)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.ResumeJobs(ctx)
	started := exec(svc, "memory.blocks.prepare", map[string]any{"source_room": "kakao:stop", "background": true})
	if !started.OK {
		t.Fatal(started.Error)
	}
	cancel()
	svc.WaitJobs(10 * time.Second)
	got := jsonShape(exec(svc, "jobs.get", map[string]any{"id": started.JobID}).Output)
	if got["status"] == "succeeded" {
		t.Skip("the offline model finished before the stop landed")
	}
	if got["status"] != "failed" || got["error_code"] != "INTERRUPTED" || !strings.Contains(fmt.Sprint(got["result"]), `"stopped_by":"interrupted"`) {
		t.Fatalf("interrupted job %+v", got)
	}
	if b := findBlock(t, svc, "kakao:stop"); strings.Contains(fmt.Sprint(b["error"]), "closed") {
		t.Fatalf("block error must be a code, not a database error: %v", b["error"])
	}
}
