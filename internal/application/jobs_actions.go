package application

import (
	"errors"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/jobs"
)

func (s *Service) jobsAction(req action.Request) action.Result {
	switch req.Action {
	case "jobs.list":
		list, err := listJobs(s.Store)
		if err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		items := make([]map[string]any, 0, len(list))
		for _, j := range list {
			items = append(items, jobJSON(j))
		}
		return action.OK(req, map[string]any{"jobs": items})
	case "jobs.get":
		id := strVal(inputMap(req), "id", req.ResourceID)
		j, err := getJob(s.Store, id)
		if err != nil {
			return action.Fail(req, errcode.JobNotFound, "generic.error", "job not found", nil)
		}
		return action.OK(req, jobJSON(j))
	case "jobs.cancel":
		id := strVal(inputMap(req), "id", req.ResourceID)
		// A running job's worker may take its lease between our read and write; re-read and try again.
		for attempt := 0; ; attempt++ {
			j, err := getJob(s.Store, id)
			if err != nil {
				return action.Fail(req, errcode.JobNotFound, "generic.error", "job not found", nil)
			}
			prev, prevStatus := j.LeaseToken, j.Status
			j, err = jobs.Cancel(j)
			if err != nil {
				return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
			}
			err = saveJobExpected(s.Store, j, prev, prevStatus)
			if err != nil && err.Error() == errcode.LeaseMismatch && attempt < 5 {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil {
				return action.Fail(req, "IO", "generic.error", err.Error(), nil)
			}
			s.jobChanged(j)
			return action.OK(req, jobJSON(j))
		}
	case "jobs.watch":
		list, err := listJobs(s.Store)
		if err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		items := make([]map[string]any, 0, len(list))
		for _, j := range list {
			items = append(items, jobJSON(j))
		}
		return action.OK(req, map[string]any{"jobs": items, "watch": true})
	case "jobs.retry":
		id := strVal(inputMap(req), "id", req.ResourceID)
		j, err := getJob(s.Store, id)
		if err != nil {
			return action.Fail(req, errcode.JobNotFound, "generic.error", "job not found", nil)
		}
		// like cancel: a run finishing in between may change the row; re-read and try again
		for attempt := 0; ; attempt++ {
			prev, prevStatus := j.LeaseToken, j.Status
			j, err = jobs.Retry(j)
			if err != nil {
				return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
			}
			err = saveJobExpected(s.Store, j, prev, prevStatus)
			if err != nil && err.Error() == errcode.LeaseMismatch && attempt < 5 {
				time.Sleep(20 * time.Millisecond)
				if j, err = getJob(s.Store, id); err != nil {
					return action.Fail(req, errcode.JobNotFound, "generic.error", "job not found", nil)
				}
				continue
			}
			if err != nil {
				return action.Fail(req, "IO", "generic.error", err.Error(), nil)
			}
			break
		}
		s.jobChanged(j)
		s.retriedJob(j)
		out := jobJSON(j)
		out["message"] = s.msg(req, "jobs.retry.ok")
		res := action.OK(req, out)
		res.JobID = j.ID
		return res
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

// Enqueue is used by tests and feature adapters. Same idempotency key returns the existing job.
func (s *Service) Enqueue(typ, idempotency, payload string) (jobs.Job, error) {
	if idempotency != "" {
		if j, err := getJobByIdempotency(s.Store, idempotency); err == nil {
			return j, nil
		}
	}
	j := jobs.Job{ID: newID("job_"), Type: typ, Status: jobs.Pending, IdempotencyKey: idempotency, Payload: payload}
	if err := insertJob(s.Store, j); err != nil {
		return j, err
	}
	s.jobChanged(j)
	return j, nil
}

func (s *Service) TakeLease(id, token string, ttl time.Duration) (jobs.Job, error) {
	j, err := getJob(s.Store, id)
	if err != nil {
		return j, err
	}
	prev := j.LeaseToken
	prevStatus := j.Status
	now := s.Clock.Now()
	j, err = jobs.Lease(j, token, now, ttl)
	if err != nil {
		return j, err
	}
	j, err = jobs.Start(j, token, now)
	if err != nil {
		return j, err
	}
	if err := saveJobExpected(s.Store, j, prev, prevStatus); err != nil {
		return j, err
	}
	s.jobChanged(j)
	return j, nil
}

func (s *Service) LeaseComplete(id, token, result string, ttl time.Duration) (jobs.Job, error) {
	j, err := s.TakeLease(id, token, ttl)
	if err != nil {
		return j, err
	}
	return s.CompleteWithToken(id, token, result)
}

func (s *Service) CompleteWithToken(id, token, result string) (jobs.Job, error) {
	j, err := getJob(s.Store, id)
	if err != nil {
		return j, err
	}
	j, err = jobs.Complete(j, token, s.Clock.Now(), result)
	if err != nil {
		return j, err
	}
	if err := saveJob(s.Store, j, token); err != nil {
		return j, err
	}
	s.jobChanged(j)
	return j, nil
}

// SetJobProgress writes Checkpoint JSON and notifies watchers. Never stores message bodies. token is
// the run's lease ("" before a run takes it): a run that lost its job (cancelled, or retried and taken
// by a new run) does not write its progress over the new one.
func (s *Service) SetJobProgress(id, token string, p jobs.Progress) (jobs.Job, error) {
	return s.setRunProgress(id, token, 0, p)
}

// setRunProgress is SetJobProgress for a run, which also names its attempt (0 = not a run).
func (s *Service) setRunProgress(id, token string, attempt int, p jobs.Progress) (jobs.Job, error) {
	j, err := getJob(s.Store, id)
	if err != nil {
		return j, err
	}
	if j.LeaseToken != token {
		return j, errors.New(errcode.LeaseMismatch)
	}
	// the status is compared too: with an empty lease (cancelled, or queued again by a retry) the token
	// alone cannot tell the row we read from the one that is there now. Only the checkpoint is written,
	// so a lease renewal made meanwhile stays.
	j.Checkpoint = jobs.EncodeProgress(p)
	if err := updateJob(s.Store, id, `checkpoint=?`, []any{j.Checkpoint}, token, j.Status, attempt); err != nil {
		return j, err
	}
	s.jobChanged(j)
	return j, nil
}

// renewJobLease extends a run's lease (jobs.Renew); LeaseMismatch means the run no longer owns the job.
func (s *Service) renewJobLease(id, token string, attempt int, ttl time.Duration) error {
	j, err := getJob(s.Store, id)
	if err != nil {
		return err
	}
	if j.Attempt != attempt {
		return errors.New(errcode.LeaseMismatch)
	}
	if j, err = jobs.Renew(j, token, s.Clock.Now(), ttl); err != nil {
		return err
	}
	return updateJob(s.Store, id, `lease_expires_at=?`, []any{nullTime(j.LeaseExpires)}, token, j.Status, attempt)
}

// jobChanged notifies SSE/TUI watchers; the payload carries status and percent, never job payloads.
func (s *Service) jobChanged(j jobs.Job) {
	body := map[string]any{"status": string(j.Status), "type": j.Type, "attempt": j.Attempt}
	p := jobs.ParseProgress(j.Checkpoint)
	if p.Label != "" {
		body["progress_pct"] = p.OverallPct
		body["progress_label"] = p.Label
		body["eta_label"] = p.ETALabel
		body["remaining"] = p.Remaining
	}
	s.emit("job", j.ID, body)
}
