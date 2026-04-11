package serve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
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

func wrapCORS(next http.Handler, allowedOrigins []string) http.Handler {
	if len(allowedOrigins) == 0 {
		return next
	}

	origins := slices.Clone(allowedOrigins)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		allowedOrigin, ok := matchAllowedOrigin(origins, origin)
		if !ok {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		headers := w.Header()
		headers.Set("Access-Control-Allow-Origin", allowedOrigin)
		headers.Add("Vary", "Origin")
		headers.Set("Access-Control-Allow-Methods", strings.Join([]string{
			http.MethodGet,
			http.MethodHead,
			http.MethodPost,
			http.MethodDelete,
			http.MethodOptions,
		}, ", "))

		requestHeaders := r.Header.Get("Access-Control-Request-Headers")
		if requestHeaders != "" {
			headers.Set("Access-Control-Allow-Headers", requestHeaders)
			headers.Add("Vary", "Access-Control-Request-Headers")
		} else {
			headers.Set("Access-Control-Allow-Headers", "Content-Type")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func matchAllowedOrigin(allowedOrigins []string, origin string) (string, bool) {
	for _, allowed := range allowedOrigins {
		if allowed == "*" || allowed == origin {
			return allowed, true
		}
	}
	return "", false
}
