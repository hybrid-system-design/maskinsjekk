// Command maskinsjekk is the machine-monitoring service: it subscribes to MQTT,
// normalizes sensor data into measurements in SQLite, and serves the HTTP API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"maskinsjekk/internal/api"
	"maskinsjekk/internal/config"
	"maskinsjekk/internal/ingest"
	"maskinsjekk/internal/store"
)

func main() {
	logger := log.New(os.Stdout, "maskinsjekk ", log.LstdFlags|log.Lmsgprefix)
	cfg := config.Load()

	logger.Printf("starting: http=%s mqtt=%s topic=%q db=%s machines=%s", cfg.HTTPAddr, cfg.MQTTURL, cfg.MQTTTopic, cfg.DBPath, cfg.MachinesPath)

	machines, err := config.LoadMachines(cfg.MachinesPath)
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Fatalf("store: %v", err)
	}
	defer st.Close()

	if err := st.SyncRegistry(context.Background(), machines); err != nil {
		logger.Fatalf("store: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// HTTP server.
	srv := api.New(st, cfg, logger)
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Printf("http: listening on %s", cfg.HTTPAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("http: %v", err)
		}
	}()

	// MQTT ingest.
	ing := ingest.New(cfg, st, logger)
	if err := ing.Start(); err != nil {
		logger.Fatalf("mqtt: %v", err)
	}

	go pruneLoop(ctx, st, cfg.RawRetention, logger)

	// Block until interrupted, then shut down gracefully.
	<-ctx.Done()
	logger.Printf("shutdown: signal received")

	ing.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("http: shutdown error: %v", err)
	}
	logger.Printf("shutdown: complete")
}

// pruneLoop drops raw measurements older than the retention window, hourly.
// Per-minute rollups are untouched, so long-range charts keep working.
func pruneLoop(ctx context.Context, st *store.Store, retention time.Duration, logger *log.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		cutoff := time.Now().Add(-retention).UnixMilli()
		if n, err := st.PruneRaw(ctx, cutoff); err != nil {
			logger.Printf("prune: %v", err)
		} else if n > 0 {
			logger.Printf("prune: removed %d raw measurements older than %s", n, retention)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
