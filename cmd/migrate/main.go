// Команда migrate управляет версиями схемы базы данных. Запускается из корня репозитория.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/pressly/goose/v3"

	"github.com/blind-postman/blind-postman/internal/config"
	"github.com/blind-postman/blind-postman/internal/storage"
)

const migrationsDir = "internal/storage/migrations"

const usage = `использование: migrate [-config файл] команда

  status        показать все миграции и их состояние
  version       показать текущую версию схемы
  up            применить все новые миграции
  up-to N       применить миграции до версии N
  down          откатить последнюю миграцию
  down-to N     откатить миграции до версии N (0 — откатить всё)
  create ИМЯ    создать файл новой миграции
`

func main() {
	configPath := flag.String("config", config.DefaultPath, "путь к файлу настроек")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, usage)
		flag.PrintDefaults()
	}
	flag.Parse()

	if err := run(*configPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "migrate:", err)
		os.Exit(1)
	}
}

func run(configPath string, args []string) error {
	if len(args) == 0 {
		flag.Usage()
		return errors.New("не указана команда")
	}
	command, args := args[0], args[1:]

	// create не обращается к базе.
	if command == "create" {
		if len(args) != 1 {
			return errors.New("create: укажите имя миграции")
		}
		goose.SetSequential(true)
		return goose.Create(nil, migrationsDir, args[0], "sql")
	}

	exec, err := commandFunc(command, args)
	if err != nil {
		return err
	}
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

	return store.WithMigrations(ctx, func(p *goose.Provider) error { return exec(ctx, p) })
}

type commandExec func(ctx context.Context, p *goose.Provider) error

func commandFunc(command string, args []string) (commandExec, error) {
	switch command {
	case "status":
		return status, nil
	case "version":
		return func(ctx context.Context, p *goose.Provider) error {
			version, err := p.GetDBVersion(ctx)
			if err != nil {
				return err
			}
			fmt.Println(version)
			return nil
		}, nil
	case "up":
		return func(ctx context.Context, p *goose.Provider) error {
			results, err := p.Up(ctx)
			printResults(results...)
			return err
		}, nil
	case "up-to":
		version, err := versionArg(command, args)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *goose.Provider) error {
			results, err := p.UpTo(ctx, version)
			printResults(results...)
			return err
		}, nil
	case "down":
		return func(ctx context.Context, p *goose.Provider) error {
			result, err := p.Down(ctx)
			if result != nil {
				printResults(result)
			}
			return err
		}, nil
	case "down-to":
		version, err := versionArg(command, args)
		if err != nil {
			return nil, err
		}
		return func(ctx context.Context, p *goose.Provider) error {
			results, err := p.DownTo(ctx, version)
			printResults(results...)
			return err
		}, nil
	}
	return nil, fmt.Errorf("неизвестная команда %q", command)
}

func versionArg(command string, args []string) (int64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("%s: укажите номер версии", command)
	}
	version, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || version < 0 {
		return 0, fmt.Errorf("%s: неверный номер версии %q", command, args[0])
	}
	return version, nil
}

func status(ctx context.Context, p *goose.Provider) error {
	migrations, err := p.Status(ctx)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		applied := "не применена"
		if m.State == goose.StateApplied {
			applied = m.AppliedAt.Local().Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%05d  %-19s  %s\n", m.Source.Version, applied, m.Source.Path)
	}
	return nil
}

func printResults(results ...*goose.MigrationResult) {
	for _, r := range results {
		fmt.Printf("%s %05d %s (%s)\n", r.Direction, r.Source.Version, r.Source.Path, r.Duration)
	}
}
