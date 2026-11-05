package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edmand46/no-slop-project-ya/internal/bot"
	"github.com/edmand46/no-slop-project-ya/internal/store"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		log.Fatal("TELEGRAM_BOT_TOKEN is required")
	}

	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "todo.db"
	}

	loc := time.Local
	if name := os.Getenv("TZ"); name != "" {
		l, err := time.LoadLocation(name)
		if err != nil {
			log.Fatalf("invalid TZ %q: %v", name, err)
		}
		loc = l
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer st.Close()

	b, err := bot.New(token, st, loc)
	if err != nil {
		log.Fatalf("create bot: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b.Run(ctx)
}
