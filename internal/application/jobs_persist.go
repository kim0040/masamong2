package application

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"masamong/internal/errcode"
	"masamong/internal/jobs"
	"masamong/internal/storage"
)

const jobColumns = `id, type, status, idempotency_key, lease_token, lease_expires_at, attempt, checkpoint, payload, result, error_code, created_at, updated_at`
const consoleJobLimit = 100

type jobRow interface {
	Scan(...any) error
}

func insertJob(store *storage.Store, j jobs.Job) error {
	now := time.Now().UTC().Format(time.RFC3339)
	return store.WithWriter(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO jobs(id, type, status, idempotency_key, lease_token, lease_expires_at, attempt, checkpoint, payload, result, error_code, created_at, updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			j.ID, j.Type, string(j.Status), nullStr(j.IdempotencyKey), nullStr(j.LeaseToken), nullTime(j.LeaseExpires),
			j.Attempt, j.Checkpoint, j.Payload, j.Result, j.ErrorCode, now, now)
		return err
	})
}

func getJob(store *storage.Store, id string) (jobs.Job, error) {
	return scanJob(store.DB.QueryRow(`SELECT `+jobColumns+` FROM jobs WHERE id=?`, id))
}

func scanJob(row jobRow) (jobs.Job, error) {
	var j jobs.Job
	var status, leaseExp, idemp, token, created, updated sql.NullString
	err := row.Scan(&j.ID, &j.Type, &status, &idemp, &token, &leaseExp, &j.Attempt, &j.Checkpoint, &j.Payload, &j.Result, &j.ErrorCode, &created, &updated)
	j.CreatedAt, j.UpdatedAt = created.String, updated.String
	if err != nil {
		return j, err
	}
	j.Status = jobs.Status(status.String)
	if idemp.Valid {
		j.IdempotencyKey = idemp.String
	}
	if token.Valid {
		j.LeaseToken = token.String
	}
	if leaseExp.Valid {
		t, _ := time.Parse(time.RFC3339, leaseExp.String)
		j.LeaseExpires = t
	}
	return j, nil
}

func getJobByIdempotency(store *storage.Store, key string) (jobs.Job, error) {
	var id string
	err := store.DB.QueryRow(`SELECT id FROM jobs WHERE idempotency_key=?`, key).Scan(&id)
	if err != nil {
		return jobs.Job{}, err
	}
	return getJob(store, id)
}

// saveJob writes only if the row still holds matchLease. Complete/lease use the
// pre-update token so a stale worker cannot overwrite after a newer lease is stored.
func saveJob(store *storage.Store, j jobs.Job, matchLease string) error {
	return saveJobExpected(store, j, matchLease, "")
}

// saveJobExpected also compares the state read before a transition. Lease tokens alone cannot
// distinguish an old pending row from a cancelled row when both tokens are empty.
func saveJobExpected(store *storage.Store, j jobs.Job, matchLease string, matchStatus jobs.Status) error {
	return saveRunJob(store, j, matchLease, matchStatus, 0)
}

// saveRunJob is a run's write: it also compares the run's attempt (> 0). A job cancelled, retried and
// cancelled again has two cancelled rows with the same empty lease; only the attempt tells them apart.
func saveRunJob(store *storage.Store, j jobs.Job, matchLease string, matchStatus jobs.Status, attempt int) error {
	return updateJob(store, j.ID, `status=?, lease_token=?, lease_expires_at=?, attempt=?, checkpoint=?, payload=?, result=?, error_code=?`,
		[]any{string(j.Status), nullStr(j.LeaseToken), nullTime(j.LeaseExpires), j.Attempt, j.Checkpoint, j.Payload, j.Result, j.ErrorCode},
		matchLease, matchStatus, attempt)
}

// updateJob sets only the named columns while the row still has the lease, status (when given) and
// attempt (when > 0) the writer read. A run's progress and its lease renewal come from two goroutines;
// writing the whole row, one would put back the other's old value.
func updateJob(store *storage.Store, id, set string, setArgs []any, matchLease string, matchStatus jobs.Status, attempt int) error {
	query := `UPDATE jobs SET ` + set + `, updated_at=? WHERE id=? AND IFNULL(lease_token,'')=?`
	args := append(setArgs[:len(setArgs):len(setArgs)], time.Now().UTC().Format(time.RFC3339), id, matchLease)
	if matchStatus != "" {
		query += ` AND status=?`
		args = append(args, string(matchStatus))
	}
	if attempt > 0 {
		query += ` AND attempt=?`
		args = append(args, attempt)
	}
	return store.WithWriter(func(tx *sql.Tx) error {
		res, err := tx.Exec(query, args...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errors.New(errcode.LeaseMismatch)
		}
		return nil
	})
}

// finishedJobStates are the statuses retention may remove; everything else is queued, running or paused.
const finishedJobStates = `'succeeded','failed','cancelled'`

// listJobs is the console list: every job that can still change plus the consoleJobLimit most recently
// finished ones (by updated_at, as retention judges age), oldest first. MySQL refuses LIMIT directly
// inside IN, hence the derived table. A paused job older than the cut would otherwise disappear from the console.
func listJobs(store *storage.Store) ([]jobs.Job, error) {
	rows, err := store.Reader().Query(`SELECT `+jobColumns+` FROM jobs WHERE status NOT IN (`+finishedJobStates+`) OR id IN
		(SELECT id FROM (SELECT id FROM jobs WHERE status IN (`+finishedJobStates+`) ORDER BY updated_at DESC, id DESC LIMIT ?) AS recent_finished) ORDER BY created_at DESC, id DESC`, consoleJobLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]jobs.Job, 0, consoleJobLimit)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Keep the prior oldest-first order within the bounded recent window.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// listNonFinishedJobs bounds recovery and queue scans to jobs that can still be resumed or inspected.
func listNonFinishedJobs(store *storage.Store) ([]jobs.Job, error) {
	rows, err := store.Reader().Query(`SELECT ` + jobColumns + ` FROM jobs WHERE status IN ('pending','retry_wait','leased','running','paused') ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []jobs.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// jobInsertionOrder is the column that breaks a created_at tie: the row's insertion order (rowid),
// as the old read-everything loop did. created_at has one-second resolution and ids are random, so a
// retry made in the same second as the failed job it replaces must still count as the newer one.
func jobInsertionOrder(store *storage.Store) string {
	if store.IsSQLite() {
		return "rowid"
	}
	return "id" // TiDB has no rowid; its job ids are the only stable tiebreaker
}

// latestPrepareIDsSQL picks the newest prepare job of every source room in ONE ordered pass over the
// type's rows (a window function). A per-row "is there a newer one" subquery is quadratic: 5,000 jobs
// took 98 s. Its single argument is the job type. Shared by the status list and by retention, which
// must keep exactly the rows the status list shows.
func latestPrepareIDsSQL(store *storage.Store) string {
	room := `CASE WHEN json_valid(payload) THEN json_extract(payload,'$.source_room') ELSE '' END`
	if !store.IsSQLite() {
		room = `CASE WHEN JSON_VALID(payload) THEN JSON_UNQUOTE(JSON_EXTRACT(payload,'$.source_room')) ELSE '' END`
	}
	return `SELECT id FROM (
		SELECT id, room, ROW_NUMBER() OVER (PARTITION BY room ORDER BY created_at DESC, tie DESC) AS rn FROM (
			SELECT id, created_at, ` + jobInsertionOrder(store) + ` AS tie, ` + room + ` AS room FROM jobs WHERE type=?) AS prepare_rows
		) AS ranked_jobs WHERE rn=1 AND room<>''` // MySQL/TiDB need an alias on every derived table
}

// latestPrepareJobs reads only the newest durable prepare job for each source room. The block list
// needs that row to show progress; older history belongs in the bounded console list.
func latestPrepareJobs(store *storage.Store) ([]jobs.Job, error) {
	rows, err := store.Reader().Query(`SELECT `+jobColumns+` FROM jobs WHERE id IN (`+latestPrepareIDsSQL(store)+`) ORDER BY created_at, `+jobInsertionOrder(store), jobPrepare)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []jobs.Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// latestPrepareJobIDs is the set of job ids latestPrepareJobs returns, computed once for retention.
func latestPrepareJobIDs(store *storage.Store) (map[string]struct{}, error) {
	rows, err := store.Reader().Query(latestPrepareIDsSQL(store), jobPrepare)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func jobJSON(j jobs.Job) map[string]any {
	b, _ := json.Marshal(j)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	delete(m, "LeaseToken")
	delete(m, "LeaseExpires")
	m["created_at"], m["updated_at"], m["error_code"], m["payload"] = j.CreatedAt, j.UpdatedAt, j.ErrorCode, j.Payload
	m["status"] = string(j.Status)
	m["id"] = j.ID
	m["type"] = j.Type
	m["attempt"] = j.Attempt
	m["result"] = j.Result
	p := jobs.ParseProgress(j.Checkpoint)
	if p.Label != "" {
		m["progress_label"] = p.Label
		m["progress_pct"] = p.OverallPct
		m["eta_label"] = p.ETALabel
		m["eta_s"] = p.ETAS
		m["remaining"] = p.Remaining
		m["progress_done"] = p.Done
		m["progress_total"] = p.Total
		m["progress_phase"] = p.Phase
	}
	return m
}

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
