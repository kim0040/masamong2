// Package clock injects time so CAS, jobs, and poll close tests do not depend on wall clock.
package clock

import "time"

type Clock interface {
	Now() time.Time
}

type Real struct{}

func (Real) Now() time.Time { return time.Now().UTC() }

type Fixed struct{ T time.Time }

func (f Fixed) Now() time.Time { return f.T }

type Stepping struct {
	T    time.Time
	Step time.Duration
}

func (s *Stepping) Now() time.Time {
	now := s.T
	s.T = s.T.Add(s.Step)
	return now
}
