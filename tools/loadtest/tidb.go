package main

import (
	"database/sql"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"masamong/internal/storage"
)

// useTiDB points the instance at a fresh database on the -tidb-dsn server (created here, dropped at the
// end unless -keep), optionally through a proxy that adds -db-latency each way, as a TiDB Cloud cluster
// in another region would. It returns a cleanup function.
//
// The product's TiDB backend is disabled (storage.TiDBEnabled, see its comment for why), so this refuses
// to run until it is switched back on; the code is kept with the backend.
func useTiDB(o options) (func(), error) {
	if !storage.TiDBEnabled {
		return nil, storage.ErrTiDBDisabled
	}
	base := o.tidbDSN
	slash := strings.LastIndex(base, "/")
	if slash < 0 {
		return nil, fmt.Errorf("-tidb-dsn must look like user@tcp(host:port)/")
	}
	params := ""
	if i := strings.Index(base[slash:], "?"); i >= 0 {
		params = base[slash+i:]
	}
	admin, err := sql.Open("mysql", base[:slash+1]+params)
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("masamong_load_%d", rand.Int64N(1e9))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		admin.Close()
		return nil, err
	}
	addr := base[strings.Index(base, "(")+1 : strings.Index(base, ")")]
	stop := func() {}
	if o.dbLatency > 0 {
		proxy, err := delayProxy(addr, o.dbLatency)
		if err != nil {
			return nil, err
		}
		stop = func() { proxy.Close() }
		addr = proxy.Addr().String()
	}
	dsn := base[:strings.Index(base, "(")+1] + addr + ")/" + name + params
	_ = os.Setenv("LOADTEST_TIDB_DSN", dsn)
	if err := storage.SaveSettings(o.dataRoot, storage.Settings{Backend: storage.BackendTiDB, DSNRef: "env:LOADTEST_TIDB_DSN"}); err != nil {
		return nil, err
	}
	return func() {
		stop()
		if !o.keep {
			_, _ = admin.Exec("DROP DATABASE " + name)
		}
		admin.Close()
	}, nil
}

// delayProxy forwards TCP to target, holding every chunk for d in each direction (a round trip costs
// 2d, like a database in another region).
func delayProxy(target string, d time.Duration) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.Dial("tcp", target)
				if err != nil {
					c.Close()
					return
				}
				var once sync.Once
				done := func() { once.Do(func() { c.Close(); up.Close() }) }
				go pipeDelayed(up, c, d, done)
				pipeDelayed(c, up, d, done)
			}()
		}
	}()
	return ln, nil
}

func pipeDelayed(dst io.Writer, src io.Reader, d time.Duration, done func()) {
	type chunk struct {
		b  []byte
		at time.Time
	}
	q := make(chan chunk, 1024)
	go func() {
		for c := range q {
			time.Sleep(time.Until(c.at))
			if _, err := dst.Write(c.b); err != nil {
				done()
				return
			}
		}
	}()
	buf := make([]byte, 64<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			q <- chunk{b: append([]byte(nil), buf[:n]...), at: time.Now().Add(d)}
		}
		if err != nil {
			close(q)
			done()
			return
		}
	}
}

// tidbSizeMB is the database's size as TiDB reports it (from statistics: approximate).
func tidbSizeMB(db *sql.DB) float64 {
	var n sql.NullFloat64
	_ = db.QueryRow(`SELECT SUM(DATA_LENGTH+INDEX_LENGTH) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()`).Scan(&n)
	return n.Float64 / (1 << 20)
}
