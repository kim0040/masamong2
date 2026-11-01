// masamongd is the long-running daemon: it owns the instance store (SQLite or
// optional TiDB/MySQL protocol), serves the embedded admin console (unless built
// with -tags noweb), and keeps the Discord gateway open when configured.
//
// One daemon owns a data folder (daemon.lock). It records its console address in daemon.json so the
// launcher (`masamong open`, the app icon) can reuse it instead of starting a second copy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"masamong/internal/application"
	"masamong/internal/connectors/discord"
	"masamong/internal/fsutil"
	"masamong/internal/hostprofile"
	"masamong/internal/launcher"
	"masamong/internal/logfile"
	"masamong/internal/memlimit"
	"masamong/internal/observability"
	"masamong/internal/storage"
	"masamong/internal/transport/web"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"output": map[string]string{"version": application.Version}})
		return
	}
	var dataRoot, dbBackend, dsnRef, logFile, maintenance string
	var forceWeb, supervise, yes bool
	flag.StringVar(&dataRoot, "data-root", "", "instance data directory (default: the launcher's data folder)")
	flag.StringVar(&dbBackend, "db-backend", "", "sqlite (the only backend; tidb is disabled, see storage.TiDBEnabled)")
	flag.StringVar(&dsnRef, "dsn-ref", "", "TiDB DSN reference (unused while the TiDB backend is disabled)")
	flag.BoolVar(&forceWeb, "web", false, "serve the admin console even if admin.web.enabled is false")
	flag.StringVar(&logFile, "log-file", "", `log file, rotated at 10 MiB with 5 kept (default: <data-root>/logs/daemon.log; "-" = stderr only)`)
	flag.BoolVar(&supervise, "supervise", false, "run the daemon as a child and start it again if it ends abnormally")
	flag.StringVar(&maintenance, "maintenance", "", "offline maintenance: status, doctor, retry, rollback, cancel, abandon-update")
	flag.BoolVar(&yes, "yes", false, "with --maintenance abandon-update: confirm without asking")
	flag.Parse()
	if dataRoot == "" {
		dataRoot = launcher.DefaultDataRoot()
	}
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		fmt.Fprintf(os.Stderr, "masamongd: protect data folder %s: %v\n", dataRoot, err)
		os.Exit(1)
	}
	if maintenance != "" {
		maintenanceCLI(dataRoot, maintenance, yes)
		return
	}
	if supervise {
		var args []string
		for _, a := range os.Args[1:] {
			if a != "--supervise" && a != "-supervise" && a != "--supervise=true" && a != "-supervise=true" {
				args = append(args, a)
			}
		}
		os.Exit(runSupervisor(dataRoot, args))
	}
	supervisor, _ := strconv.Atoi(os.Getenv(supervisedEnv))
	supervised := supervisor > 0
	_ = os.Unsetenv(supervisedEnv)
	log, closeLog := openLog(dataRoot, logFile)
	defer closeLog()
	memlimit.ApplyAutomatic(log)
	// WHY before the store opens: the read pool and the page cache are chosen
	// once, from the profile installed here. Current stays at the medium limits
	// until Apply, which is what tests and library users keep.
	profile := hostprofile.Detect()
	hostprofile.Apply(profile)
	memoryMiB := "unknown"
	if profile.MemoryBytes > 0 {
		memoryMiB = strconv.FormatUint(profile.MemoryBytes>>20, 10)
	}
	log.Info("host profile", "tier", profile.Tier, "cpus", profile.CPUs, "memory_mib", memoryMiB, "source", profile.Source)
	restartBind := os.Getenv("_MASAMONG_RESTART_BIND")
	_ = os.Unsetenv("_MASAMONG_RESTART_BIND")
	if dbBackend != "" || dsnRef != "" {
		st, err := storage.LoadSettings(dataRoot)
		if err != nil {
			log.Error("storage.yaml", "err", err)
			os.Exit(1)
		}
		if dbBackend != "" {
			st.Backend = dbBackend
		}
		if dsnRef != "" {
			st.DSNRef = dsnRef
		}
		if err := storage.SaveSettings(dataRoot, st); err != nil {
			log.Error("storage.yaml", "err", err)
			os.Exit(1)
		}
	}
	lock, err := launcher.AcquireLock(dataRoot)
	if err != nil {
		log.Error("lock", "err", err)
		os.Exit(launcher.ExitLocked)
	}
	restart, bind := run(dataRoot, forceWeb, restartBind, supervisor, log)
	lock.Release()
	if restart && supervised {
		os.Exit(launcher.ExitRestart) // the supervisor starts a fresh copy on the same address
	}
	if restart {
		self, err := os.Executable()
		if err == nil {
			log.Info("restarting")
			if bind != "" {
				err = os.Setenv("_MASAMONG_RESTART_BIND", bind)
			}
		}
		if err == nil {
			err = reexec(self)
		}
		if err == nil {
			return
		}
		log.Error("restart failed; start masamong again", "err", err)
		os.Exit(1)
	}
}

// openLog writes JSON lines to a rotating file so months of uptime cannot fill the disk. The lines also go
// to stderr when that is a terminal, a pipe or the systemd journal; a launcher that redirects stderr to a
// plain file gets only crashes there, not a second unbounded copy of the log. Records at Error or above
// are also kept in errors.log next to it, a small file that stays readable after a busy week.
//
// The logger is installed as the slog default too, so code that logs through slog.Default() reaches the
// same files. Every record passes through redaction, and carries the turn or job id of its context.
func openLog(dataRoot, path string) (*slog.Logger, func()) {
	if env := os.Getenv("MASAMONG_LOG_LEVEL"); env != "" {
		_ = observability.SetLevel(env) // a bad value is reported once the configuration is read
	}
	build := func(w io.Writer, errs io.Writer) *slog.Logger {
		var h slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{Level: observability.Level})
		if errs != nil {
			h = observability.Fanout(h, observability.MinLevel(slog.NewJSONHandler(errs, nil), slog.LevelError))
		}
		l := slog.New(observability.ContextHandler(observability.RedactHandler(h)))
		slog.SetDefault(l)
		return l
	}
	if path == "-" {
		return build(os.Stderr, nil), func() {}
	}
	if path == "" {
		path = filepath.Join(dataRoot, "logs", "daemon.log")
	}
	f, err := logfile.Open(path, 10<<20, 5)
	if err != nil {
		l := build(os.Stderr, nil)
		l.Warn("log file unavailable; logging to stderr only", "path", path, "err", err)
		return l, func() {}
	}
	var w io.Writer = f
	if st, err := os.Stderr.Stat(); err == nil && !st.Mode().IsRegular() {
		w = io.MultiWriter(f, os.Stderr)
	}
	closeLog := func() { _ = f.Close() }
	ef, err := logfile.Open(filepath.Join(filepath.Dir(path), "errors.log"), 5<<20, 3)
	if err != nil {
		l := build(w, nil)
		l.Warn("error log file unavailable; errors go to the main log only", "err", err)
		return l, closeLog
	}
	return build(w, ef), func() { _ = ef.Close(); closeLog() }
}

// Shutdown budget of the daemon itself. The supervisor derives its kill grace from these
// (childStopGrace) so the two cannot drift.
const (
	shutdownLifecycleDelay = 300 * time.Millisecond // console answers before the listener closes
	shutdownJobWait        = 15 * time.Second       // running jobs stop and record why before the database closes
	shutdownReplyWait      = 18 * time.Second       // in-flight replies record their outcome (gateway 12s + 3s)
)

// run serves until a signal or an operator stop (false) or an operator restart request (true).
func run(dataRoot string, forceWeb bool, restartBind string, supervisor int, log *slog.Logger) (bool, string) {
	// a reset staged by instance.reset, or a restore staged by backup.restore, runs before the database
	// opens; the programs an in-app update replaced are cleaned up
	if err := application.CleanUpdateLeftovers(); err != nil {
		return runMaintenance(dataRoot, restartBind, err, log)
	}
	if err := application.RecoverMaintenance(dataRoot, log); err != nil {
		return runMaintenance(dataRoot, restartBind, err, log)
	}
	svc, err := application.Open(dataRoot, nil, web.Embedded())
	if err != nil {
		log.Error("open", "err", err)
		os.Exit(1)
	}
	defer svc.Close()
	svc.Daemon = true
	log = svc.CaptureLogs(log)
	svc.Log = log
	if !svc.Ready() {
		log.Warn("instance not initialised; finish setup in the console")
	}
	svc.BackupOnUpgrade()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	restart := false
	srv := web.New(svc)
	srv.Force = forceWeb
	srv.UseRestartBind(restartBind)
	go func() {
		select {
		case l := <-svc.LifecycleRequests():
			restart = l == application.Lifecycle("restart")
			if restart {
				srv.PreserveSessionsForRestart()
			}
			log.Info("lifecycle request", "request", string(l))
			// let the console answer the request before the listener closes
			time.Sleep(shutdownLifecycleDelay)
			cancel()
		case <-ctx.Done():
		}
	}()
	defer func() { svc.NoteStop(restart) }()

	svc.ResumeJobs(ctx)
	defer svc.WaitJobs(shutdownJobWait) // before svc.Close: running jobs stop and record why
	go svc.RunMemoryWorker(ctx)
	go svc.RunScheduler(ctx)
	if svc.Doc().Discord.Enabled {
		// A missing token is not fatal: the console and CLI keep working and the dashboard shows the reason.
		gw, err := discord.Start(ctx, svc, svc.Doc().Discord, log)
		switch {
		case errors.Is(err, discord.ErrNoToken):
			log.Warn("discord enabled but no token present", "token_ref", svc.Doc().Discord.TokenRef)
		case err != nil:
			log.Error("discord gateway", "err", err)
		default:
			// replies in progress stop with the run context; let them record their outcome before the
			// database closes
			defer func() {
				if !gw.Wait(shutdownReplyWait) { // the gateway gives replies 12s + 3s (LIVE-20)
					log.Warn("discord: replies still running at shutdown")
				}
			}()
		}
	}
	if web.Embedded() {
		// written once the console listens, with the address it really got
		srv.OnListen = func(addr string) {
			_ = launcher.WriteInfo(dataRoot, launcher.DaemonInfo{PID: os.Getpid(), Supervisor: supervisor, Bind: addr,
				Started: time.Now().UTC().Format(time.RFC3339), Version: application.Version})
		}
		defer launcher.RemoveInfo(dataRoot)
	}
	if err := srv.Listen(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	return restart, srv.Bind()
}

// A failed maintenance operation never opens the SQL store. This small loopback
// recovery console remains reachable even when config or the database is broken.
func runMaintenance(root, bind string, problem error, log *slog.Logger) (bool, string) {
	log.Error("maintenance mode: bot and schedulers are off", "err", problem)
	bind = maintenanceConsoleBind(root, bind)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	listener, err := net.Listen("tcp", bind)
	if err != nil {
		listener, err = net.Listen("tcp", "127.0.0.1:0")
	}
	if err != nil {
		log.Error("maintenance console unavailable", "err", err)
		return false, ""
	}
	bind = listener.Addr().String()
	srv := &http.Server{Addr: bind, Handler: maintenanceHandler(root, stop), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	_ = launcher.WriteInfo(root, launcher.DaemonInfo{PID: os.Getpid(), Bind: bind, Version: application.Version, Started: time.Now().UTC().Format(time.RFC3339)})
	defer launcher.RemoveInfo(root)
	log.Warn("maintenance console reachable; stop daemon before using offline recovery commands", "bind", bind)
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("maintenance console", "err", err)
	}
	return false, bind
}

func maintenanceCLI(root, command string, yes bool) {
	if command == "status" || command == "doctor" {
		_ = json.NewEncoder(os.Stdout).Encode(application.MaintenanceStatus(root))
		return
	}
	lock, err := launcher.AcquireLock(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(launcher.ExitLocked)
	}
	defer lock.Release()
	switch command {
	case "retry":
		err = application.CleanUpdateLeftovers()
		if err == nil {
			err = application.RecoverMaintenance(root, slog.Default())
		}
	case "rollback":
		var handled bool
		handled, err = application.RollbackPendingUpdate()
		if err == nil && !handled {
			err = application.RollbackMaintenance(root)
		}
	case "cancel":
		err = application.CancelMaintenance(root)
	case "abandon-update":
		err = abandonUpdate(yes)
	default:
		err = fmt.Errorf("unknown maintenance command %q", command)
	}
	if err != nil {
		lock.Release()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, "maintenance recovery complete; start masamongd again")
}
