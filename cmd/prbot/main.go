// Command prbot is a self-hosted pull request review bot driven by a local
// or hosted LLM (LM Studio / Ollama), managed through a small web UI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/OMG-Software/omg-review-bot/internal/bot"
	"github.com/OMG-Software/omg-review-bot/internal/store"
	"github.com/OMG-Software/omg-review-bot/internal/web"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("PRBOT_LISTEN", "127.0.0.1:8080"), "address for the web UI (use 0.0.0.0:8080 to allow other machines)")
	data := flag.String("data", env("PRBOT_DATA", "data"), "data directory for the database and skill")
	resetPw := flag.Bool("reset-password", false, "clear the admin password (set a new one in the web UI), then exit")
	flag.Parse()

	if err := run(*listen, *data, *resetPw); err != nil {
		fmt.Fprintln(os.Stderr, "prbot:", err)
		os.Exit(1)
	}
}

func run(listen, dataDir string, resetPw bool) error {
	service := isService()
	if !filepath.IsAbs(dataDir) && service {
		// Services start in a system directory; keep data next to the binary.
		if exe, err := os.Executable(); err == nil {
			dataDir = filepath.Join(filepath.Dir(exe), dataDir)
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}

	var out io.Writer = os.Stderr
	if service {
		f, err := os.OpenFile(filepath.Join(dataDir, "prbot.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	}
	logger := slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.Open(filepath.Join(dataDir, "prbot.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	if resetPw {
		if err := st.Delete("admin_password"); err != nil {
			return err
		}
		fmt.Println("Admin password cleared. Start prbot and open the web UI to set a new one; doing so signs everyone out.")
		return nil
	}

	b := bot.New(st, dataDir, logger)
	srv, err := web.New(st, b, logger)
	if err != nil {
		return err
	}

	return runPlatform(func(ctx context.Context) error {
		go b.Run(ctx)
		hs := &http.Server{Addr: listen, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
		errc := make(chan error, 1)
		go func() {
			logger.Info("web UI listening", "url", "http://"+listen, "data", dataDir)
			errc <- hs.ListenAndServe()
		}()
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			logger.Info("shutting down")
			if err := hs.Shutdown(sctx); err != nil {
				return err
			}
			return srv.Close()
		case err := <-errc:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	})
}
