// setup performs a one-time interactive authentication of the Telegram user account
// via MTProto (Telethon-equivalent). Run this once before starting the bot.
//
// Usage:
//
//	go run ./cmd/setup
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"telegram-summarizer/internal/config"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if cfg.TelegramAPIID == 0 || cfg.TelegramAPIHash == "" {
		log.Fatal("TELEGRAM_API_ID and TELEGRAM_API_HASH must be set in .env")
	}

	if err := os.MkdirAll("session", 0o755); err != nil {
		log.Fatalf("create session dir: %v", err)
	}

	client := telegram.NewClient(cfg.TelegramAPIID, cfg.TelegramAPIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: cfg.SessionPath},
	})

	ctx := context.Background()
	err = client.Run(ctx, func(ctx context.Context) error {
		flow := auth.NewFlow(terminalAuth{}, auth.SendCodeOptions{})
		if err := client.Auth().IfNecessary(ctx, flow); err != nil {
			return fmt.Errorf("auth: %w", err)
		}

		me, err := tg.NewClient(client).UsersGetFullUser(ctx, &tg.InputUserSelf{})
		if err != nil {
			return fmt.Errorf("get self: %w", err)
		}

		if u, ok := me.Users[0].(*tg.User); ok {
			fmt.Printf("\n✅ Authenticated as: %s %s", u.FirstName, u.LastName)
			if u.Username != "" {
				fmt.Printf(" (@%s)", u.Username)
			}
			fmt.Printf("\n   ID: %d\n", u.ID)
		}
		fmt.Printf("   Session saved to: %s\n", cfg.SessionPath)
		fmt.Println("\nYou can now run the bot: go run ./cmd/bot")
		return nil
	})
	if err != nil {
		log.Fatalf("setup failed: %v", err)
	}
}

// terminalAuth implements auth.UserAuthenticator for interactive terminal auth.
type terminalAuth struct{}

func (terminalAuth) Phone(_ context.Context) (string, error) {
	fmt.Print("Enter your phone number (e.g. +79001234567): ")
	return readLine()
}

func (terminalAuth) Password(_ context.Context) (string, error) {
	fmt.Print("Enter 2FA password (press Enter if none): ")
	return readLine()
}

func (terminalAuth) Code(_ context.Context, _ *tg.AuthSentCode) (string, error) {
	fmt.Print("Enter the verification code: ")
	return readLine()
}

func (terminalAuth) AcceptTermsOfService(_ context.Context, tos tg.HelpTermsOfService) error {
	fmt.Printf("Terms of service: %s\n", tos.Text)
	return nil
}

func (terminalAuth) SignUp(_ context.Context) (auth.UserInfo, error) {
	fmt.Print("First name: ")
	first, _ := readLine()
	fmt.Print("Last name: ")
	last, _ := readLine()
	return auth.UserInfo{FirstName: first, LastName: last}, nil
}

func readLine() (string, error) {
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}
