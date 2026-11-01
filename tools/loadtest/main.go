// Command loadtest runs the Masamong service under sustained, mixed load and reports whether it can run
// for a long time: throughput and latency, errors, and the resources that must stay flat (goroutines,
// open files) or grow only with stored data (heap, database, logs).
//
// Everything runs in one process with a fake OpenAI-compatible model server, so a run costs nothing and
// never leaves the machine (MASAMONG_OFFLINE=1). The service code is the real one: chat turns, memory
// capture and indexing, the scheduler, poll buttons, console reads and config changes, concurrently.
//
//	go run ./tools/loadtest -duration 2m                  # quick look
//	go run ./tools/loadtest -duration 30m -rate 8 -strict # soak; exit 1 on a leak or errors
//	go run ./tools/loadtest -rate 60 -concurrency 8       # saturate: find the throughput ceiling
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	_ "net/http/pprof" // -pprof
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/logfile"
	"masamong/internal/memory/engine"
	"masamong/internal/providers"
	"masamong/internal/tools"
)

type options struct {
	duration, sampleEvery, configEvery, drain time.Duration
	rate, reply, cmdRate, consoleRate         float64
	channels, users, concurrency, dims        int
	latency, jitter                           time.Duration
	errRate                                   float64
	memory, strict, keep, messageVectors      bool
	seed                                      int
	dataRoot, report                          string
	heapProfile, pprofAddr                    string
	tidbDSN                                   string
	dbLatency                                 time.Duration
	features                                  bool
	featureRate                               float64
}

func main() {
	var o options
	flag.DurationVar(&o.duration, "duration", 2*time.Minute, "how long to send traffic")
	flag.Float64Var(&o.rate, "rate", 10, "Discord messages per second (Poisson)")
	flag.Float64Var(&o.reply, "reply", 0.6, "share of messages the bot answers (the rest are only captured)")
	flag.Float64Var(&o.cmdRate, "cmd-rate", 1, "commands and poll button presses per second")
	flag.Float64Var(&o.consoleRate, "console-rate", 2, "console reads per second")
	flag.BoolVar(&o.features, "features", true, "personal features too: reminders with delivery, participation (odd channels), consent, settings")
	flag.Float64Var(&o.featureRate, "feature-rate", 1, "personal feature uses per second (with -features)")
	flag.DurationVar(&o.configEvery, "config-every", 30*time.Second, "operator config change interval (0 = none)")
	flag.IntVar(&o.channels, "channels", 12, "Discord channels")
	flag.IntVar(&o.users, "users", 60, "Discord users")
	flag.IntVar(&o.concurrency, "concurrency", 8, "limits.max_concurrent_turns")
	flag.DurationVar(&o.latency, "llm-latency", 400*time.Millisecond, "model call latency")
	flag.DurationVar(&o.jitter, "llm-jitter", 400*time.Millisecond, "extra random model latency (0..jitter)")
	flag.Float64Var(&o.errRate, "llm-error-rate", 0.01, "share of model calls that fail with 502")
	flag.IntVar(&o.dims, "dims", 256, "embedding dimensions")
	flag.BoolVar(&o.memory, "memory", true, "capture messages and use memory (capture, index, retrieval)")
	flag.BoolVar(&o.messageVectors, "message-vectors", true, "memory.message_vectors (on by default like the product; off to compare memory use)")
	flag.IntVar(&o.seed, "seed", 0, "messages of existing history to store and index before the load (a long-running instance)")
	flag.DurationVar(&o.sampleEvery, "sample", 5*time.Second, "resource sample interval")
	flag.DurationVar(&o.drain, "drain", 150*time.Second, "how long to wait for in-flight work after traffic stops")
	flag.StringVar(&o.dataRoot, "data-root", "", "instance folder (default: a new temporary one)")
	flag.StringVar(&o.report, "report", "", "write the report here (.md; a .json next to it)")
	flag.BoolVar(&o.strict, "strict", false, "exit 1 when a long-run check fails")
	flag.BoolVar(&o.keep, "keep", false, "keep the temporary instance folder")
	flag.StringVar(&o.heapProfile, "heap-profile", "", "write a heap profile after the drain (go tool pprof)")
	flag.StringVar(&o.pprofAddr, "pprof", "", "serve net/http/pprof here during the run, e.g. 127.0.0.1:6060 (goroutine dumps of a stall)")
	flag.StringVar(&o.tidbDSN, "tidb-dsn", "", "run on TiDB/MySQL instead of SQLite (disabled with the TiDB backend, see storage.TiDBEnabled)")
	flag.DurationVar(&o.dbLatency, "db-latency", 0, "with -tidb-dsn: extra delay each way to the database (0 = direct)")
	flag.Parse()
	if err := run(o); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

func run(o options) error {
	_ = os.Setenv("MASAMONG_OFFLINE", "1") // only the loopback fake model is reachable
	_ = os.Setenv("LOADTEST_MODEL_KEY", "fake")
	if o.dataRoot == "" {
		dir, err := os.MkdirTemp("", "masamong-load-")
		if err != nil {
			return err
		}
		o.dataRoot = dir
		if !o.keep {
			defer os.RemoveAll(dir)
		}
	}
	if o.tidbDSN != "" {
		cleanup, err := useTiDB(o)
		if err != nil {
			return fmt.Errorf("tidb: %w", err)
		}
		defer cleanup()
	}
	if o.pprofAddr != "" {
		go func() { _ = http.ListenAndServe(o.pprofAddr, nil) }()
	}
	llm := &fakeLLM{latency: o.latency, jitter: o.jitter, errRate: o.errRate, dims: o.dims}
	srv := llm.start()
	defer srv.Close()

	svc, err := application.Open(o.dataRoot, nil, false)
	if err != nil {
		return err
	}
	defer svc.Close()
	// the daemon's log setup: JSON lines to a file under logs/, plus the console's in-memory ring
	logDir := filepath.Join(o.dataRoot, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	lf, err := logfile.Open(filepath.Join(logDir, "daemon.log"), 10<<20, 5) // as masamongd does
	if err != nil {
		return err
	}
	defer lf.Close()
	svc.Daemon = true
	svc.Log = svc.CaptureLogs(slog.New(slog.NewJSONHandler(lf, nil)))
	slog.SetDefault(svc.Log)

	if err := prepare(svc, o, srv.URL); err != nil {
		return err
	}
	h := &harness{svc: svc, opt: o, rec: newRecorder(), errors: map[string]int{}}
	gw := &featureGateway{delivered: map[string]int{}}
	if o.features {
		svc.Discord = gw // reminders are delivered, participation posts, through a counting stand-in
	}
	var seeded *seedResult
	if o.seed > 0 && o.memory {
		if seeded, err = seedHistory(svc, o); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
		fmt.Fprintf(os.Stderr, "loadtest: seeded %d messages in %s, indexed in %s (%d leaves), index rebuild %dms (reading %dms), heap %.1fMB, rss %.1fMB\n",
			o.seed, seeded.Capture.Round(time.Millisecond), seeded.Index.Round(time.Millisecond), seeded.Leaves, seeded.ReloadMS, seeded.ReadMS, seeded.HeapMB, seeded.RSSMB)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.ResumeJobs(ctx)
	var bg sync.WaitGroup
	for _, fn := range []func(context.Context){svc.RunMemoryWorker, svc.RunScheduler} {
		bg.Add(1)
		go func(fn func(context.Context)) { defer bg.Done(); fn(ctx) }(fn)
	}

	time.Sleep(time.Second)
	settle()
	start := time.Now()
	baseline := takeSample(start, svc, o.dataRoot, 0, 0)
	samples := []sample{baseline}
	fmt.Fprintf(os.Stderr, "loadtest: %s at %.1f msg/s over %d channels (concurrency %d, model %v±%v, errors %.0f%%) in %s\n",
		o.duration, o.rate, o.channels, o.concurrency, o.latency, o.jitter, o.errRate*100, o.dataRoot)

	load, stopLoad := context.WithTimeout(ctx, o.duration)
	defer stopLoad()
	var gens sync.WaitGroup
	for _, fn := range []func(){
		func() { h.arrivals(load) },
		func() { h.every(load, o.cmdRate, h.command) },
		func() { h.every(load, o.consoleRate, h.console) },
		func() { h.configChurn(load) },
		func() {
			if o.features {
				h.every(load, o.featureRate, h.feature)
			}
		},
		func() {
			if o.features {
				h.every(load, o.consoleRate/2, h.featureConsole)
			}
		},
	} {
		gens.Add(1)
		go func(fn func()) { defer gens.Done(); fn() }(fn)
	}
	var smu sync.Mutex
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		t := time.NewTicker(o.sampleEvery)
		defer t.Stop()
		for {
			select {
			case <-load.Done():
				return
			case <-t.C:
			}
			s := takeSample(start, svc, o.dataRoot, h.turnsDone.Load(), llm.inflight.Load())
			smu.Lock()
			samples = append(samples, s)
			smu.Unlock()
			fmt.Fprintf(os.Stderr, "  t=%4.0fs turns=%-6d goroutines=%-4d heap=%6.1fMB db=%6.1fMB fds=%d reload=%dms\n", s.T, s.Turns, s.Goroutines, s.HeapMB, s.DBMB, s.FDs, s.ReloadMS)
		}
	}()
	<-load.Done()
	gens.Wait()
	<-sampled
	loadEnd := time.Now()

	// drain: the replies already accepted finish, then background work stops, as on a daemon stop
	drained := waitGroup(&h.inflight, o.drain)
	drainTime := time.Since(loadEnd)
	cancel()
	bg.Wait()
	svc.WaitJobs(15 * time.Second)
	http.DefaultTransport.(*http.Transport).CloseIdleConnections()
	providers.CloseIdleConnections()
	if !svc.Store.IsSQLite() {
		// a TiDB pool keeps idle connections (a goroutine and a socket each, more through -db-latency's
		// proxy); release them so what remains is what the service itself left running
		svc.Store.DB.SetMaxIdleConns(0)
		svc.Store.DB.SetMaxIdleConns(8)
	}
	// old memory indexes are closed a minute after a swap; wait for those timers before counting
	final := settleGoroutines(baseline.Goroutines, 75*time.Second)
	end := takeSample(start, svc, o.dataRoot, h.turnsDone.Load(), llm.inflight.Load())
	end.Goroutines = final

	if o.heapProfile != "" {
		if f, err := os.Create(o.heapProfile); err == nil {
			runtime.GC()
			_ = pprof.WriteHeapProfile(f)
			_ = f.Close()
		}
	}
	var integrity string
	if svc.Store.IsSQLite() {
		_ = svc.Store.DB.QueryRow(`PRAGMA integrity_check`).Scan(&integrity)
	} else {
		integrity = "ok" // TiDB keeps its own consistency; nothing to check from a client
	}
	end.TableMB = tableMB(svc.Store.DB)
	rep := buildReport(o, h, llm, samples, baseline, end, loadEnd.Sub(start), drained, drainTime, integrity)
	rep.Seed = seeded
	if o.features {
		var ff []string
		rep.Features, ff = h.featureChecks(gw)
		rep.Failures = append(rep.Failures, ff...)
	}
	if end.Goroutines-baseline.Goroutines > 10 {
		rep.Goroutines = goroutineDump()
	}
	md := rep.markdown()
	fmt.Println(md)
	if o.report != "" {
		if err := os.WriteFile(o.report, []byte(md), 0o644); err != nil {
			return err
		}
		js, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(strings.TrimSuffix(o.report, filepath.Ext(o.report))+".json", js, 0o644); err != nil {
			return err
		}
	}
	if o.strict && len(rep.Failures) > 0 {
		return fmt.Errorf("%d long-run check(s) failed: %s", len(rep.Failures), strings.Join(rep.Failures, "; "))
	}
	return nil
}

// prepare sets the instance up like a configured bot: fake model for every role, memory on, every
// channel answering and capturing, and budgets out of the way.
func prepare(svc *application.Service, o options, base string) error {
	if r := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: json.RawMessage(`{"instance":"load"}`), Locale: "en"}); !r.OK {
		return fmt.Errorf("setup: %+v", r.Error)
	}
	raw, _ := config.Marshal(svc.Doc())
	d, err := config.Parse(raw) // deep copy
	if err != nil {
		return err
	}
	d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: base, CredentialRef: "env:LOADTEST_MODEL_KEY"}
	d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
	d.Models = map[string]config.Model{
		"planner":  {Provider: "openrouter", Model: "fake/planner"},
		"response": {Provider: "openrouter", Model: "fake/response"},
		"summary":  {Provider: "openrouter", Model: "fake/summary"},
		"embed":    {Provider: "openrouter", Model: "fake/embed", Dimensions: o.dims},
		"rerank":   {Provider: "openrouter", Model: "fake/rerank"},
	}
	d.Memory.Capture, d.Memory.Embed, d.Memory.Summaries = o.memory, o.memory, o.memory
	d.Memory.Retrieve = "auto"
	d.Memory.MessageVectors = &o.messageVectors
	if !o.memory {
		d.Memory.Retrieve = "off"
	}
	d.Memory.IndexIntervalMin = 1
	d.Memory.IndexBudgetUSD = "1000"
	d.Limits.MonthlyBudgetUSD = "1000000"
	d.Limits.MaxConcurrentTurns = o.concurrency
	d.Tools = map[string]tools.Settings{"search.web": {Chat: boolp(false)}}
	if o.features {
		// short waits so candidates reach the judge within a run; the real limits stay in force
		d.Participation = config.Participation{Enabled: true, Mode: "conservative", WaitSec: 2}
		d.Discord.Enabled = true
	}
	out, _ := config.Marshal(d)
	in, _ := json.Marshal(map[string]any{"yaml": string(out), "base_revision": svc.Revision()})
	if r := svc.Execute(context.Background(), action.Request{Action: "config.apply", Input: in, BaseRevision: svc.Revision(), Locale: "en"}); !r.OK {
		return fmt.Errorf("config: %+v", r.Error)
	}
	for i := 0; i < o.channels; i++ {
		respond := "always"
		if o.features && participates(i) {
			respond = "mention" // people talk among themselves here; the bot answers when called or joins in
		}
		in, _ := json.Marshal(map[string]any{"guild_id": "g", "id": fmt.Sprintf("ch%03d", i), "respond": respond,
			"capture": o.memory, "memory_use": map[bool]string{true: "on", false: "off"}[o.memory]})
		if r := svc.Execute(context.Background(), action.Request{Action: "channels.configure", Input: in, Locale: "en"}); !r.OK {
			return fmt.Errorf("channel: %+v", r.Error)
		}
		if o.features && participates(i) {
			in, _ := json.Marshal(map[string]any{"guild_id": "g", "channel_id": fmt.Sprintf("ch%03d", i), "mode": participationMode(i)})
			if r := svc.Execute(context.Background(), action.Request{Action: "participation.mode", Input: in, Locale: "en"}); !r.OK {
				return fmt.Errorf("participation: %+v", r.Error)
			}
		}
	}
	return nil
}

func boolp(b bool) *bool { return &b }

type seedResult struct {
	Messages int           `json:"messages"`
	Capture  time.Duration `json:"capture_ns"`
	Index    time.Duration `json:"index_ns"`
	Leaves   int           `json:"leaves"`
	ReloadMS int64         `json:"index_rebuild_ms"`
	ReadMS   int64         `json:"index_rebuild_read_ms"`
	HeapMB   float64       `json:"heap_mb"`
	RSSMB    float64       `json:"rss_mb"`
}

// seedHistory stores o.seed past messages across the channels (a few minutes apart, so leaves close)
// and indexes them, as an instance that has been running for a long time would have.
func seedHistory(svc *application.Service, o options) (*seedResult, error) {
	eng, err := svc.MemoryEngine()
	if err != nil {
		return nil, err
	}
	r := &seedResult{Messages: o.seed}
	started := time.Now()
	const per = 3 * time.Minute // between two messages of one channel: leaves fill up to 8 messages
	first := time.Now().Add(-time.Duration(o.seed/max(1, o.channels)+1) * per)
	h := &harness{opt: o}
	for i := 0; i < o.seed; i++ {
		ch := fmt.Sprintf("ch%03d", i%o.channels)
		user := fmt.Sprintf("u%03d", i%o.users)
		at := first.Add(time.Duration(i/o.channels) * per)
		if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: ch, MessageID: fmt.Sprintf("seed%d", i),
			AuthorID: user, AuthorName: user, Content: h.message(), At: at}); err != nil {
			return nil, err
		}
	}
	r.Capture = time.Since(started)
	started = time.Now()
	rep, err := eng.IndexRooms(context.Background(), engine.IndexOptions{BudgetUSD: 1e9}, nil)
	if err != nil {
		return nil, err
	}
	_ = eng.DirtyRooms() // indexed: the worker has nothing left from the seed
	r.Index = time.Since(started)
	r.Leaves = rep.Reload["leaves"]
	r.ReloadMS = eng.LastReload().Milliseconds()
	// a second build right away: what every later rebuild costs (unchanged rows are reused)
	if _, err := eng.Reload(); err != nil {
		return nil, err
	}
	r.ReloadMS, r.ReadMS = eng.LastReload().Milliseconds(), eng.LastReloadRead().Milliseconds()
	settle()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	r.HeapMB = mb(m.HeapInuse)
	r.RSSMB = rssMB()
	return r, nil
}

func backendName(o options) string {
	if o.tidbDSN != "" {
		return "tidb"
	}
	return "sqlite"
}

func settle() {
	runtime.GC()
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
}

func settleGoroutines(baseline int, max time.Duration) int {
	deadline := time.Now().Add(max)
	n := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		settle()
		if n = runtime.NumGoroutine(); n <= baseline+5 {
			return n
		}
		time.Sleep(2 * time.Second)
	}
	return n
}

// goroutineDump groups the goroutines still running by stack (the leak suspects), largest first.
func goroutineDump() string {
	var b strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&b, 1)
	return clip(b.String(), 12000)
}

func waitGroup(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// ---------------------------------------------------------------------------
// report
// ---------------------------------------------------------------------------

type report struct {
	Options    map[string]any     `json:"options"`
	LoadSec    float64            `json:"load_sec"`
	DrainSec   float64            `json:"drain_sec"`
	Drained    bool               `json:"drained"`
	Turns      int64              `json:"turns"`
	TurnsPerS  float64            `json:"turns_per_sec"`
	Ops        []opStats          `json:"ops"`
	LLM        map[string]int64   `json:"llm"`
	Baseline   sample             `json:"baseline"`
	End        sample             `json:"end"`
	Samples    []sample           `json:"samples"`
	PerKTurns  map[string]float64 `json:"growth_per_1000_turns"`
	HeapSlope  float64            `json:"heap_mb_per_min_second_half"`
	HeapPerObs float64            `json:"heap_kb_per_stored_message"`
	Errors     map[string]int     `json:"errors"`
	Integrity  string             `json:"integrity_check"`
	Failures   []string           `json:"failures"`
	Seed       *seedResult        `json:"seed,omitempty"`
	Goroutines string             `json:"goroutines_left,omitempty"`
	Notes      []string           `json:"notes"`
	Features   map[string]int     `json:"features,omitempty"`
}

func buildReport(o options, h *harness, llm *fakeLLM, samples []sample, base, end sample, load time.Duration, drained bool, drain time.Duration, integrity string) report {
	r := report{
		Options: map[string]any{"duration": o.duration.String(), "rate": o.rate, "reply": o.reply, "cmd_rate": o.cmdRate, "console_rate": o.consoleRate,
			"channels": o.channels, "users": o.users, "concurrency": o.concurrency, "llm_latency": o.latency.String(), "llm_jitter": o.jitter.String(),
			"llm_error_rate": o.errRate, "memory": o.memory, "dims": o.dims, "config_every": o.configEvery.String(), "go": runtime.Version(), "cpus": runtime.NumCPU(),
			"backend": backendName(o), "db_latency": o.dbLatency.String(), "features": o.features, "feature_rate": o.featureRate},
		LoadSec: load.Seconds(), DrainSec: drain.Seconds(), Drained: drained, Turns: h.turnsDone.Load(), Ops: h.rec.stats(),
		LLM: map[string]int64{"calls": llm.calls.Load(), "injected_errors": llm.errors.Load(), "peak_inflight": llm.peak.Load(),
			"chat": llm.byPath[0].Load(), "embeddings": llm.byPath[1].Load(), "rerank": llm.byPath[2].Load(), "connections": llm.conns.Load()},
		Baseline: base, End: end, Samples: samples, Errors: h.errors, Integrity: integrity, PerKTurns: map[string]float64{},
	}
	r.TurnsPerS = float64(r.Turns) / math.Max(load.Seconds(), 1)
	if k := float64(r.Turns) / 1000; k > 0 {
		r.PerKTurns["db_mb"] = (end.DBMB + end.WALMB - base.DBMB - base.WALMB) / k
		r.PerKTurns["logs_mb"] = (end.LogMB - base.LogMB) / k
		for t, n := range end.Rows {
			if n >= 0 {
				r.PerKTurns["rows:"+t] = float64(n-base.Rows[t]) / k
			}
		}
	}
	// heap trend over the second half of the run (the first half includes warm-up and cache fill)
	var xs, ys []float64
	for _, s := range samples[len(samples)/2:] {
		xs, ys = append(xs, s.T/60), append(ys, s.HeapMB)
	}
	r.HeapSlope = slope(xs, ys)
	if obs := end.Rows["memory_observations"] - base.Rows["memory_observations"]; obs > 0 {
		last := samples[len(samples)-1]
		r.HeapPerObs = (last.HeapMB - base.HeapMB) * 1024 / float64(obs)
	}

	// long-run checks
	fail := func(f string, a ...any) { r.Failures = append(r.Failures, fmt.Sprintf(f, a...)) }
	if !drained {
		fail("in-flight work did not finish within %s after traffic stopped", o.drain)
	}
	if leak := end.Goroutines - base.Goroutines; leak > 10 {
		fail("goroutines: %d before load, %d after drain (+%d)", base.Goroutines, end.Goroutines, leak)
	}
	if base.FDs >= 0 && end.FDs-base.FDs > 10 {
		fail("open files: %d before load, %d after drain", base.FDs, end.FDs)
	}
	if integrity != "ok" {
		fail("database integrity_check: %s", integrity)
	}
	for _, op := range r.Ops {
		total := 0
		for _, n := range op.Outcomes {
			total += n
		}
		if bad := op.Outcomes["error"] + op.Outcomes["timeout"]; total > 0 && float64(bad)/float64(total) > 0.01 {
			fail("%s: %d of %d failed or timed out", op.Kind, bad, total)
		}
	}
	if r.Turns == 0 && o.rate > 0 && o.reply > 0 {
		fail("no chat turn completed")
	}
	return r
}

func (r report) markdown() string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	w("# Masamong load test\n\n")
	keys := make([]string, 0, len(r.Options))
	for k := range r.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w("| option | value |\n|---|---|\n")
	for _, k := range keys {
		w("| %s | %v |\n", k, r.Options[k])
	}
	verdict := "PASS"
	if len(r.Failures) > 0 {
		verdict = "FAIL"
	}
	w("\n**Verdict: %s** — %d chat turns in %.0fs (%.2f turns/s), drain %.1fs, integrity `%s`\n\n", verdict, r.Turns, r.LoadSec, r.TurnsPerS, r.DrainSec, r.Integrity)
	for _, f := range r.Failures {
		w("- FAIL: %s\n", f)
	}
	if len(r.Features) > 0 {
		keys := make([]string, 0, len(r.Features))
		for k := range r.Features {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.Features[k]))
		}
		w("\nPersonal features: %s\n", strings.Join(parts, " "))
	}
	if r.Seed != nil {
		w("\nExisting history: %d messages stored in %s and indexed in %s (%d leaves); index rebuild %d ms (%d ms reading the database); heap after indexing %.1f MB (process RSS %.1f MB).\n",
			r.Seed.Messages, r.Seed.Capture.Round(time.Millisecond), r.Seed.Index.Round(time.Millisecond), r.Seed.Leaves, r.Seed.ReloadMS, r.Seed.ReadMS, r.Seed.HeapMB, r.Seed.RSSMB)
	}
	w("\n## Operations\n\n| kind | ok | other outcomes | p50 ms | p95 ms | p99 ms | max ms |\n|---|---|---|---|---|---|---|\n")
	for _, op := range r.Ops {
		var other []string
		for k, v := range op.Outcomes {
			if k != "ok" {
				other = append(other, fmt.Sprintf("%s=%d", k, v))
			}
		}
		sort.Strings(other)
		w("| %s | %d | %s | %.1f | %.1f | %.1f | %.1f |\n", op.Kind, op.OK, strings.Join(other, " "), op.P50, op.P95, op.P99, op.Max)
	}
	w("\nModel server: %d calls (chat %d, embeddings %d, rerank %d) over %d connections, %d injected failures, peak %d in flight.\n",
		r.LLM["calls"], r.LLM["chat"], r.LLM["embeddings"], r.LLM["rerank"], r.LLM["connections"], r.LLM["injected_errors"], r.LLM["peak_inflight"])
	w("\n## Resources\n\n| | before load | after drain |\n|---|---|---|\n")
	w("| goroutines | %d | %d |\n| open files | %d | %d |\n| heap in use MB | %.1f | %.1f |\n| process RSS MB | %.1f | %.1f |\n| database MB (+WAL) | %.1f | %.1f |\n| logs MB | %.2f | %.2f |\n",
		r.Baseline.Goroutines, r.End.Goroutines, r.Baseline.FDs, r.End.FDs, r.Baseline.HeapMB, r.End.HeapMB,
		r.Baseline.RSSMB, r.End.RSSMB, r.Baseline.DBMB+r.Baseline.WALMB, r.End.DBMB+r.End.WALMB, r.Baseline.LogMB, r.End.LogMB)
	w("\nHeap trend (second half): %+.2f MB/min; heap per stored message: %.2f KB.\n", r.HeapSlope, r.HeapPerObs)
	if len(r.End.TableMB) > 0 {
		w("\nStored bytes by table (MB, payload columns): ")
		tk := make([]string, 0, len(r.End.TableMB))
		for k := range r.End.TableMB {
			tk = append(tk, k)
		}
		sort.Strings(tk)
		for i, k := range tk {
			if i > 0 {
				w(", ")
			}
			w("%s %.2f", k, r.End.TableMB[k])
		}
		w("\n")
	}
	w("\n## Growth per 1,000 chat turns\n\n| what | per 1,000 turns |\n|---|---|\n")
	gk := make([]string, 0, len(r.PerKTurns))
	for k := range r.PerKTurns {
		gk = append(gk, k)
	}
	sort.Strings(gk)
	for _, k := range gk {
		w("| %s | %.2f |\n", k, r.PerKTurns[k])
	}
	w("\n## Time series\n\n| t s | turns | goroutines | heap MB | db MB | wal MB | fds | index rebuild ms | turn_log | embed_cache |\n|---|---|---|---|---|---|---|---|---|---|\n")
	step := max(1, len(r.Samples)/24)
	for i, s := range r.Samples {
		if i%step == 0 || i == len(r.Samples)-1 {
			w("| %.0f | %d | %d | %.1f | %.1f | %.1f | %d | %d | %d | %d |\n", s.T, s.Turns, s.Goroutines, s.HeapMB, s.DBMB, s.WALMB, s.FDs, s.ReloadMS, s.Rows["turn_log"], s.Rows["memory_embed_cache"])
		}
	}
	if r.Goroutines != "" {
		w("\n## Goroutines left after drain\n\n```\n%s\n```\n", r.Goroutines)
	}
	if len(r.Errors) > 0 {
		w("\n## Distinct errors\n\n")
		for e, n := range r.Errors {
			w("- %d× %s\n", n, e)
		}
	}
	return b.String()
}
