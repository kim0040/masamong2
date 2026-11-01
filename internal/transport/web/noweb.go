//go:build noweb

package web

import (
	"context"

	"masamong/internal/application"
)

// noweb builds omit the console from the binary.

type Server struct {
	svc      *application.Service
	Force    bool
	OnListen func(addr string)
}

func Embedded() bool { return false }

func New(svc *application.Service) *Server { return &Server{svc: svc} }

func (s *Server) Bind() string { return "" }

func (s *Server) UseRestartBind(string) {}

func (s *Server) PreserveSessionsForRestart() {}

func (s *Server) Listen(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
