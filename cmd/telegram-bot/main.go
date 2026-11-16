package main

import (
	"context"
	"fooddiary/internal/bot"
	"fooddiary/internal/core"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		panic("set TELEGRAM_BOT_TOKEN in .env")
	}
	db, e := core.OpenDB(core.Env("DB_PATH", "data/bot.db"), bot.Schema)
	if e != nil {
		panic(e)
	}
	defer db.Close()
	b := bot.Bot{DB: db, TG: bot.NewTelegram(token), Client: core.NewClient(7 * time.Second), UserURL: core.Env("USER_URL", "http://localhost:8081"), AnalysisURL: core.Env("ANALYSIS_URL", "http://localhost:8082"), DiaryURL: core.Env("DIARY_URL", "http://localhost:8083"), InternalKey: core.Secret("INTERNAL_KEY")}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e = b.Run(ctx); e != nil && ctx.Err() == nil {
		panic(e)
	}
}
