// Команда genlink генерирует ссылку регистрации так же, как это должна делать служба.
// Общий секрет берётся из файла настроек.
//
//	go run ./cmd/genlink -bot my_alert_bot -mode token -ttl 1h
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/blind-postman/blind-postman/internal/config"
	"github.com/blind-postman/blind-postman/pkg/startlink"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "путь к файлу настроек")
	botName := flag.String("bot", "", "имя бота без @")
	mode := flag.String("mode", "token", "режим регистрации: token | callback")
	ttl := flag.Duration("ttl", 24*time.Hour, "срок действия ссылки")
	flag.Parse()

	if *botName == "" {
		fail("-bot is required")
	}
	m, err := startlink.ParseMode(*mode)
	if err != nil {
		fail("%v", err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fail("%v", err)
	}

	param, p, err := startlink.New(cfg.SharedSecret, m, *ttl)
	if err != nil {
		fail("%v", err)
	}
	fmt.Println(startlink.DeepLink(*botName, param))
	fmt.Fprintf(os.Stderr, "nonce=%s expires_at=%s\n", p.NonceHex(), p.ExpiresAt.Format(time.RFC3339))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
