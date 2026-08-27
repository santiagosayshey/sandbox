// Command sandboxd serves the sandbox: an ephemeral file drop with a live
// page and credential-injecting proxies.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/santiagosayshey/sandbox/internal/proxy"
	"github.com/santiagosayshey/sandbox/internal/server"
	"github.com/santiagosayshey/sandbox/internal/store"
	"github.com/santiagosayshey/sandbox/internal/watch"
)

var version = "dev"

func main() {
	log.SetFlags(0)
	addr := envOr("SANDBOX_ADDR", ":8080")
	dir := envOr("SANDBOX_DATA", "/data")
	token := os.Getenv("SANDBOX_TOKEN")
	if token == "" {
		log.Fatal("SANDBOX_TOKEN is required")
	}
	maxUpload, err := strconv.ParseInt(envOr("SANDBOX_MAX_UPLOAD", "52428800"), 10, 64)
	if err != nil || maxUpload <= 0 {
		log.Fatalf("SANDBOX_MAX_UPLOAD: not a positive integer")
	}
	upstreams, err := proxy.FromEnv(os.Environ())
	if err != nil {
		log.Fatal(err)
	}

	st, err := store.Open(dir)
	if err != nil {
		log.Fatalf("open %s: %v", dir, err)
	}
	defer st.Close()
	w, err := watch.New(dir, 100*time.Millisecond)
	if err != nil {
		log.Fatalf("watch %s: %v", dir, err)
	}
	defer w.Close()

	h, err := server.New(st, w, server.Config{
		Token:     token,
		MaxUpload: maxUpload,
		PublicURL: os.Getenv("SANDBOX_PUBLIC_URL"),
		Version:   version,
		Upstreams: upstreams,
	})
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	names := make([]string, 0, len(upstreams))
	for _, u := range upstreams {
		names = append(names, u.Name)
	}
	log.Printf("sandboxd %s listening on %s, data in %s, upstreams %v", version, addr, dir, names)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
