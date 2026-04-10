package serve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
)

type httpServer struct {
	target serveTarget
	server *http.Server
}

func (s *httpServer) run(ctx context.Context) error {
	if s.target.network == "unix" {
		if err := ensureSocketDir(s.target.address); err != nil {
			return err
		}
		if err := removeStaleSocket(s.target.address); err != nil {
			return err
		}
	}

	listener, err := net.Listen(s.target.network, s.target.address)
	if err != nil {
		return err
	}
	defer func() {
		_ = listener.Close()
		if s.target.network == "unix" {
			_ = os.Remove(s.target.address)
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- s.server.Serve(listener)
	}()

	select {
	case <-ctx.Done():
		shutdownErr := s.server.Shutdown(context.Background())
		serveErr := <-errCh
		if shutdownErr != nil {
			return shutdownErr
		}
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
		return serveErr
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return errors.New("refusing to replace non-socket file at " + path)
	}
	return os.Remove(path)
}
