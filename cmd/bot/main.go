package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"telegram-summarizer/internal/ai"
	"telegram-summarizer/internal/bot"
	"telegram-summarizer/internal/collector"
	"telegram-summarizer/internal/config"
	"telegram-summarizer/internal/storage"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config validation: %v\nCopy .env.example to .env and fill in the values.", err)
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	col := collector.New(cfg.TelegramAPIID, cfg.TelegramAPIHash, cfg.SessionPath, db, cfg.MaxMessagesPerChannel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Println("Starting MTProto client…")
	if err := col.Start(ctx); err != nil {
		log.Fatalf("collector start: %v\nRun './setup' to authenticate first.", err)
	}
	defer col.Stop()

	aiClient := ai.New(cfg.GeminiAPIKey)

	b, err := bot.New(cfg.BotToken, db, col, aiClient)
	if err != nil {
		log.Fatalf("bot init: %v", err)
	}

	log.Println("Bot is running. Press Ctrl+C to stop.")
	if err := b.Run(ctx); err != nil {
		log.Printf("bot stopped: %v", err)
	}
}
