// Command pos-ticker prints contact-form messages on an ESC/POS thermal printer.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/badhrinadhbade/pos-ticker/internal/config"
	"github.com/badhrinadhbade/pos-ticker/internal/domain"
	"github.com/badhrinadhbade/pos-ticker/internal/escpos"
	"github.com/badhrinadhbade/pos-ticker/internal/httpapi"
	"github.com/badhrinadhbade/pos-ticker/internal/metrics"
	"github.com/badhrinadhbade/pos-ticker/internal/notify"
	"github.com/badhrinadhbade/pos-ticker/internal/printer"
	"github.com/badhrinadhbade/pos-ticker/internal/render"
	"github.com/badhrinadhbade/pos-ticker/internal/store"
	"github.com/badhrinadhbade/pos-ticker/web"
)

// version is set at build time: -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "version", "--version":
			fmt.Println(version)
			return
		default:
			fmt.Fprintf(os.Stderr, "usage: %s [healthcheck|version]\n", os.Args[0])
			os.Exit(2)
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthcheck GETs the local admin /health (used by the container HEALTHCHECK;
// the scratch image has no curl or shell).
func healthcheck() int {
	addr := os.Getenv("ADMIN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8000" // matches config's bare-binary default
	}
	host, port := "127.0.0.1", addr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			port = addr[i+1:]
			break
		}
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + host + ":" + port + "/health")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	st.MaxSubmissionsPerHour = cfg.MaxSubmissionsPerHour // set before the store is shared
	st.MaxPendingRows = cfg.MaxPendingRows
	if n, err := st.ReleaseStaleClaims(ctx); err != nil {
		return err
	} else if n > 0 {
		log.Warn("released stale in-flight print claims", "count", n)
	}

	settings := config.NewSettings(st, cfg.SettingDefaults)
	if err := settings.Load(ctx); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	rend := render.New(render.Config{
		WidthPx: cfg.PrinterWidthPx, MaxImageBytes: cfg.MaxImageBytes,
		MaxImageHeight: cfg.MaxImageHeight, MaxImagePixels: cfg.MaxImagePixels,
		DisplayTZ: cfg.DisplayLocation,
	})
	layout := render.Layout{
		WidthPx: cfg.PrinterWidthPx, ASCIIColumns: cfg.ASCIIColumns, ASCIILineSpacing: cfg.ASCIILineSpacing,
		BandPx: cfg.ImageBandPx, BandPause: cfg.ImageBandPause, ImageImpl: cfg.ImageImpl,
		MaxImageBytes: cfg.MaxImageBytes, MaxImageHeight: cfg.MaxImageHeight, MaxImagePixels: cfg.MaxImagePixels,
	}
	build := func(j domain.Job, banded bool) (escpos.Doc, error) {
		l := layout
		l.Banded = banded
		return render.BuildJob(j, settings, l, cfg.DisplayLocation)
	}

	hub := httpapi.NewHub()
	met := metrics.New()
	worker := printer.NewWorker(printer.ConfigFromEnv(os.Getenv), settings, build, st, hub.Publish)
	worker.Start(ctx)

	mailer := notify.New(notify.Config{
		SMTPHost: cfg.SMTPHost, SMTPPort: cfg.SMTPPort,
		User: cfg.SMTPUser, Pass: cfg.SMTPPass, To: cfg.NotifyEmail,
		MaxPerHour: envInt("NOTIFY_MAX_PER_HOUR", 0),
	}, func() bool { return settings.Get("email_notifications") == "true" }, log)

	deps := httpapi.Deps{
		Store: st, Settings: settings, Queue: worker, Renderer: rend,
		Mailer: mailer, Stats: statsAdapter{worker}, Hub: hub, Metrics: met,
		UI: web.FS, Logger: log,
		Config: httpapi.Config{
			AdminKey: cfg.AdminKey, TrustProxy: cfg.TrustProxy,
			MaxImageBytes: cfg.MaxImageBytes, MaxMessageChars: cfg.MaxMessageChars,
			ASCIIColumns: cfg.ASCIIColumns, DisplayTZ: cfg.DisplayLocation,
			AllowedHosts: cfg.AdminAllowedHosts,
		},
	}
	pub, err := httpapi.NewPublicHandler(deps)
	if err != nil {
		return err
	}
	adm, err := httpapi.NewAdminHandler(deps)
	if err != nil {
		return err
	}
	pubSrv := httpapi.NewServer(cfg.PublicAddr, pub)
	admSrv := httpapi.NewServer(cfg.AdminAddr, adm)

	errc := make(chan error, 2)
	for name, srv := range map[string]*http.Server{"public": pubSrv, "admin": admSrv} {
		go func() {
			log.Info("listening", "listener", name, "addr", srv.Addr, "version", version)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s listener: %w", name, err)
			}
		}()
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = pubSrv.Shutdown(sctx)
	_ = admSrv.Shutdown(sctx)
	_ = worker.Shutdown(sctx)
	return err
}

type statsAdapter struct{ w *printer.Worker }

func (a statsAdapter) PrintTotals() (uint64, uint64) {
	s := a.w.Stats()
	return s.PrintedOK, s.PrintedFailed
}

// envInt reads an optional integer env var; 0 (or unparsable) keeps the package default.
func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
