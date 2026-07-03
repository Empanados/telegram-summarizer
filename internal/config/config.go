package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	BotToken              string
	GeminiAPIKey          string
	TelegramAPIID         int
	TelegramAPIHash       string
	SessionPath           string
	DBPath                string
	MaxMessagesPerChannel int
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	apiID := 0
	if v := os.Getenv("TELEGRAM_API_ID"); v != "" {
		var err error
		apiID, err = strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("TELEGRAM_API_ID must be an integer")
		}
	}

	maxMsgs := 500
	if v := os.Getenv("MAX_MESSAGES_PER_CHANNEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMsgs = n
		}
	}

	sessionName := os.Getenv("SESSION_NAME")
	if sessionName == "" {
		sessionName = "session/userbot"
	}

	return &Config{
		BotToken:              os.Getenv("BOT_TOKEN"),
		GeminiAPIKey:          os.Getenv("GEMINI_API_KEY"),
		TelegramAPIID:         apiID,
		TelegramAPIHash:       os.Getenv("TELEGRAM_API_HASH"),
		SessionPath:           sessionName,
		DBPath:                filepath.Join("data", "bot.db"),
		MaxMessagesPerChannel: maxMsgs,
	}, nil
}

func (c *Config) Validate() error {
	var missing []string
	if c.BotToken == ""     { missing = append(missing, "BOT_TOKEN") }
	if c.GeminiAPIKey == "" { missing = append(missing, "GEMINI_API_KEY") }
	if c.TelegramAPIID == 0 { missing = append(missing, "TELEGRAM_API_ID") }
	if c.TelegramAPIHash == "" { missing = append(missing, "TELEGRAM_API_HASH") }
	if len(missing) > 0 {
		return fmt.Errorf("missing required env vars: %s", strings.Join(missing, ", "))
	}
	return nil
}
