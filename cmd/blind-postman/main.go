// Команда blind-postman — Telegram-бот алертинга с REST API. С командой migrate вместо запуска
// бота управляет версиями схемы базы данных.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/blind-postman/blind-postman/internal/api"
	"github.com/blind-postman/blind-postman/internal/config"
	"github.com/blind-postman/blind-postman/internal/delivery"
	"github.com/blind-postman/blind-postman/internal/migrate"
	"github.com/blind-postman/blind-postman/internal/storage"
	"github.com/blind-postman/blind-postman/internal/tgbot"
)

const usage = `использование:
  %[1]s [-config файл]                  запустить бота
  %[1]s [-config файл] migrate команда  управлять схемой базы данных

`

func main() {
	configPath := flag.String("config", config.DefaultPath, "путь к файлу настроек")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage, filepath.Base(os.Args[0]))
		fmt.Fprint(os.Stderr, migrate.Usage, "\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if args := flag.Args(); len(args) > 0 {
		os.Exit(runCommand(*configPath, args))
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func runCommand(configPath string, args []string) int {
	if args[0] != "migrate" {
		fmt.Fprintf(os.Stderr, "неизвестная команда %q\n\n", args[0])
		flag.Usage()
		return 2
	}
	if err := migrate.Run(configPath, args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		return 1
	}
	return 0
}

func run(configPath string, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := storage.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		return err
	}
	log.Info("database connected")

	recipients, err := store.Recipients(ctx)
	if err != nil {
		return err
	}
	log.Info("recipients loaded", "count", len(recipients))
	for _, r := range recipients {
		log.Info("recipient", "tg_id", r.TelegramID, "alias", r.Alias)
	}

	tg, err := tgbot.New(ctx, tgbot.Config{
		Token:       cfg.TelegramToken,
		DeveloperID: cfg.DeveloperID,
		Secret:      cfg.SharedSecret,
		CallbackURL: cfg.CallbackURL,
		CallbackTTL: cfg.CallbackTTL,
	}, store, log)
	if err != nil {
		return err
	}

	dispatcher := delivery.New(delivery.Config{
		Workers:      cfg.Queue.Workers,
		GlobalRate:   cfg.Queue.GlobalRate,
		ChatInterval: cfg.Queue.ChatInterval,
		MaxAttempts:  cfg.Queue.MaxAttempts,
		PollInterval: cfg.Queue.PollInterval,
	}, store, tg, log)

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: api.New(store, tg, api.Config{
			Secret:     cfg.SharedSecret,
			APIKey:     cfg.APIKey,
			LinkTTL:    cfg.LinkTTL,
			MessageTTL: cfg.MessageTTL,
		}, log).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server started", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go tg.Run(ctx)
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		dispatcher.Run(ctx)
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	// Дожидаемся сообщений, которые уже отправляются, чтобы зафиксировать их результат.
	select {
	case <-dispatched:
	case <-shutdownCtx.Done():
		log.Warn("dispatcher did not stop in time")
	}
	return err
}
