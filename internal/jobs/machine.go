// Package jobs is the durable job state machine (17장 §5).
// Persistence is separate; this file has no SQL so tests hit real transitions.
package jobs

import (
	"errors"
	"fmt"
	"time"

	"masamong/internal/errcode"
)

type Status string

const (
	Pending    Status = "pending"
	Leased     Status = "leased"
	Running    Status = "running"
	Paused     Status = "paused"
	RetryWait  Status = "retry_wait"
	Succeeded  Status = "succeeded"
	Failed     Status = "failed"
	Cancelled  Status = "cancelled"
)

type Job struct {
	ID             string
	Type           string
	Status         Status
	IdempotencyKey string
	LeaseToken     string
	LeaseExpires   time.Time
	Attempt        int
	Checkpoint     string
	Payload        string
	Result         string
	ErrorCode      string
	// CreatedAt and UpdatedAt are stored by persistence (RFC 3339); the state machine ignores them.
	CreatedAt string
	UpdatedAt string
}

func leaseHeld(j Job, now time.Time) bool {
	if j.LeaseToken == "" {
		return false
	}
	if j.LeaseExpires.IsZero() {
		return true
	}
	// Same rule as checkLease: expiry is still valid until now.After(expires).
	return !now.After(j.LeaseExpires)
}

func Lease(j Job, token string, now time.Time, ttl time.Duration) (Job, error) {
	if token == "" {
		return j, errors.New(errcode.InvalidInput)
	}
	switch j.Status {
	case Succeeded, Failed, Cancelled:
		return j, fmt.Errorf("%s: terminal", errcode.InvalidInput)
	case Leased, Running:
		if leaseHeld(j, now) && j.LeaseToken != token {
			return j, errors.New(errcode.LeaseMismatch)
		}
	case Pending, RetryWait, Paused:
		if leaseHeld(j, now) && j.LeaseToken != token {
			return j, errors.New(errcode.LeaseMismatch)
		}
	}
	if j.Status != Pending || j.Attempt == 0 {
		j.Attempt++ // a retried job (Pending again) counted its new attempt when it was retried
	}
	j.Status = Leased
	j.LeaseToken = token
	j.LeaseExpires = now.Add(ttl)
	return j, nil
}

func Start(j Job, token string, now time.Time) (Job, error) {
	if err := checkLease(j, token, now); err != nil {
		return j, err
	}
	j.Status = Running
	return j, nil
}

// Renew extends the lease of the run that holds it, so a run longer than its lease can still record
// its end. A lease that ran out but that no other run took is renewed too: the save compares the row,
// so a run that took it in between wins.
func Renew(j Job, token string, now time.Time, ttl time.Duration) (Job, error) {
	if token == "" || j.LeaseToken != token || (j.Status != Leased && j.Status != Running) {
		return j, errors.New(errcode.LeaseMismatch)
	}
	j.LeaseExpires = now.Add(ttl)
	return j, nil
}

// Complete accepts only the current lease token so a stale worker cannot overwrite a newer attempt.
func Complete(j Job, token string, now time.Time, result string) (Job, error) {
	if err := checkLease(j, token, now); err != nil {
		return j, err
	}
	j.Status = Succeeded
	j.Result = result
	j.LeaseToken = ""
	return j, nil
}

func Fail(j Job, token string, now time.Time, code string, retry bool) (Job, error) {
	if err := checkLease(j, token, now); err != nil {
		return j, err
	}
	if retry {
		j.Status = RetryWait
		j.ErrorCode = code
		j.LeaseToken = ""
		return j, nil
	}
	j.Status = Failed
	j.ErrorCode = code
	j.LeaseToken = ""
	return j, nil
}

func Cancel(j Job) (Job, error) {
	if j.Status == Succeeded {
		return j, fmt.Errorf("%s: already succeeded", errcode.InvalidInput)
	}
	j.Status = Cancelled
	j.LeaseToken = ""
	return j, nil
}

func Retry(j Job) (Job, error) {
	if j.Status != Failed && j.Status != Cancelled && j.Status != RetryWait {
		return j, fmt.Errorf("%s: cannot retry %s", errcode.InvalidInput, j.Status)
	}
	j.Status = Pending
	j.ErrorCode = ""
	j.LeaseToken = ""
	j.Result = ""
	// a new attempt at once: the old run, maybe still stopping, compares its attempt before every
	// write, and a retry cancelled again before any run took it must not look like the old run's row
	j.Attempt++
	return j, nil
}

func checkLease(j Job, token string, now time.Time) error {
	if j.LeaseToken != token {
		return errors.New(errcode.LeaseMismatch)
	}
	if !j.LeaseExpires.IsZero() && now.After(j.LeaseExpires) {
		return errors.New(errcode.LeaseMismatch)
	}
	return nil
}
