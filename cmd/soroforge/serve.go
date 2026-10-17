package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/soroworks/soroforge/internal/config"
)

// serve runs the HTTP server until interrupted, then shuts it down gracefully
// so an in-flight deploy is not cut off mid-submission.
func serve(ctx context.Context, srv *http.Server, useTLS bool) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		slog.Info("HTTP API listening", "addr", srv.Addr, "tls", useTLS)

		var err error
		if useTLS {
			// The certificates are already on srv.TLSConfig, so the file
			// arguments are empty.
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down, waiting for in-flight requests")

		// Long enough for a deploy that is already waiting on confirmation to
		// finish rather than being abandoned half-recorded.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}

// resolveTLSFiles decides which certificate and key to serve with.
//
// Flags and environment are not mixed: passing one flag without the other is
// an error even when an environment variable could fill the gap, because a
// half-overridden pair is far more likely to be a typo than an intention.
// With no flags at all, the environment pair is used as configured.
func resolveTLSFiles(flagCert, flagKey string, env config.Env) (certFile, keyFile string, err error) {
	if flagCert != "" || flagKey != "" {
		if flagCert == "" || flagKey == "" {
			return "", "", fmt.Errorf("TLS needs both a certificate and a key: pass both --tls-cert and --tls-key")
		}
		return flagCert, flagKey, nil
	}

	return env.TLSFiles()
}

// tlsConfig loads a certificate/key pair and pins the minimum protocol
// version to TLS 1.2. Older versions have known weaknesses, and these
// listeners carry a signing key.
func tlsConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate %s and key %s: %w", certFile, keyFile, err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// isLoopbackAddr reports whether addr binds only the loopback interface.
//
// It is deliberately conservative: anything that cannot be shown to be
// loopback (an empty host, an unparsable address, a name that is not
// "localhost") counts as exposed, so the plaintext warning cannot be silenced
// by an address this function fails to understand.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		// ":8080" and "0.0.0.0:8080" both mean every interface.
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
