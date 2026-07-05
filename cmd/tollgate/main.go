// Tollgate is a provider-transparent proxy for AI agents' LLM API traffic:
// attribution, budgets with real-time enforcement, and audit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/opslync/tollgate/internal/api"
	"github.com/opslync/tollgate/internal/auth"
	"github.com/opslync/tollgate/internal/budget"
	"github.com/opslync/tollgate/internal/config"
	"github.com/opslync/tollgate/internal/proxy"
	"github.com/opslync/tollgate/internal/store"
	"github.com/opslync/tollgate/pricing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tollgate:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to YAML config file")
	logJSON := flag.Bool("log-json", false, "emit logs as JSON instead of text")
	flag.Parse()

	var handler slog.Handler = slog.NewTextHandler(os.Stdout, nil)
	if *logJSON {
		handler = slog.NewJSONHandler(os.Stdout, nil)
	}
	logger := slog.New(handler)

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	provider := cfg.Providers[0]
	upstream, err := url.Parse(provider.BaseURL)
	if err != nil {
		return err
	}

	prices, err := pricing.Load()
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.Storage.Path)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer st.Close()
	logger.Info("usage storage ready", "path", cfg.Storage.Path, "pricing_version", prices.Version)

	engine := budget.New(st, cfg.Budgets, logger)
	if err := engine.LoadKills(context.Background()); err != nil {
		return err
	}
	logger.Info("budget enforcement ready", "budgets", len(cfg.Budgets))

	p := proxy.New(provider.Name, upstream, provider.APIKey, logger)
	p.SetRecorder(func(rec proxy.RequestRecord) {
		record := store.Record{
			Time: rec.Time, Agent: rec.Agent.Name, Team: rec.Agent.Team,
			Namespace: rec.Agent.Namespace, Provider: rec.Provider,
			Model: rec.Model, Method: rec.Method, Path: rec.Path,
			Status: rec.Status, DurationMS: rec.DurationMS, Stream: rec.Stream,
			Usage: rec.Usage,
		}
		if rec.UsageOK {
			cost, priced := prices.Cost(rec.Model, rec.Usage)
			record.CostUSD = cost
			if !priced {
				logger.Warn("model missing from pricing table, cost recorded as 0", "model", rec.Model)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := st.Insert(ctx, record); err != nil {
			logger.Error("failed to persist usage record", "error", err)
		}
		engine.Record(rec.Agent, rec.Usage.InputTokens+rec.Usage.OutputTokens, record.CostUSD)
	})

	authn := auth.New(cfg.Agents)
	wrap := func(h http.Handler) http.Handler { return h }
	if len(cfg.Agents) > 0 {
		wrap = authn.Middleware
	} else {
		logger.Warn("no agents configured: authentication disabled, requests pass through unattributed")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.Handle("GET /usage", wrap(api.UsageHandler(st)))
	mux.Handle("/", wrap(engine.Middleware(p)))

	if cfg.Server.AdminKey != "" {
		agentNames := make([]string, len(cfg.Agents))
		for i, a := range cfg.Agents {
			agentNames[i] = a.Name
		}
		mux.Handle("/admin/", api.Admin(engine, cfg.Server.AdminKey, agentNames))
		logger.Info("admin endpoints enabled")
	} else {
		logger.Info("admin endpoints disabled (server.admin_key not set)")
	}

	srv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("tollgate listening", "addr", cfg.Server.Listen, "provider", cfg.Providers[0].Name)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
