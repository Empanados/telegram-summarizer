package bot

import (
	"context"
	"fmt"
	"log"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"telegram-summarizer/internal/ai"
	"telegram-summarizer/internal/collector"
	"telegram-summarizer/internal/storage"
)

// Bot wires together the Telegram bot API, storage, collector and AI client.
type Bot struct {
	api       *tgbotapi.BotAPI
	db        *storage.DB
	collector *collector.Collector
	ai        *ai.Client
}

func New(token string, db *storage.DB, col *collector.Collector, aiClient *ai.Client) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("connect bot API: %w", err)
	}
	log.Printf("Authorized as @%s", api.Self.UserName)
	return &Bot{api: api, db: db, collector: col, ai: aiClient}, nil
}

// Run starts the update polling loop and blocks until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := b.api.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			b.api.StopReceivingUpdates()
			return nil
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if update.Message == nil {
				continue
			}
			go b.handleUpdate(ctx, update.Message)
		}
	}
}

func (b *Bot) handleUpdate(ctx context.Context, msg *tgbotapi.Message) {
	username := msg.From.UserName
	if username == "" {
		username = msg.From.FirstName
	}
	log.Printf("[Bot] Incoming message from @%s: %q", username, msg.Text)
	if msg.IsCommand() {
		b.handleCommand(ctx, msg)
	} else {
		b.handleQuestion(ctx, msg)
	}
}

// ── Commands ──────────────────────────────────────────────────────────────────

func (b *Bot) handleCommand(ctx context.Context, msg *tgbotapi.Message) {
	switch msg.Command() {
	case "start", "help":
		b.cmdStart(msg)
	case "add":
		b.cmdAdd(ctx, msg)
	case "select":
		b.cmdSelect(msg)
	case "active":
		b.cmdActive(msg)
	case "channels":
		b.cmdChannels(msg)
	case "remove":
		b.cmdRemove(msg)
	case "sync":
		b.cmdSync(ctx, msg)
	case "summary":
		b.cmdSummary(ctx, msg)
	}
}

func (b *Bot) cmdStart(msg *tgbotapi.Message) {
	b.send(msg.Chat.ID, `👋 *Привет\!* Я помогаю отвечать на вопросы по конкретным Telegram\-каналам с помощью Gemini\.

📌 *Команды:*
/add @channel — добавить канал и сделать его активным
/select @channel — сделать ранее добавленный канал активным
/active — показать текущий активный канал
/channels — список всех добавленных каналов
/remove @channel — удалить канал
/sync — обновить сообщения во всех каналах
/summary @channel — краткое резюме канала

💬 После подключения и выбора канала, вы можете задать вопрос в свободной форме прямо в чате\. Я отвечу на него строго на основе сообщений этого канала\.`)
}

func (b *Bot) cmdAdd(ctx context.Context, msg *tgbotapi.Message) {
	arg := strings.TrimSpace(msg.CommandArguments())
	if arg == "" {
		b.send(msg.Chat.ID, "Укажите канал: /add @channelname")
		return
	}
	username := norm(arg)

	wait := b.sendMD(msg.Chat.ID, fmt.Sprintf("Проверяю канал @%s…", escMD(username)))

	info, err := b.collector.ResolveChannel(ctx, username)
	if err != nil {
		b.edit(msg.Chat.ID, wait, fmt.Sprintf("❌ Канал `@%s` не найден или недоступен\\.", escMD(username)))
		return
	}

	added, err := b.db.AddChannel(msg.Chat.ID, username)
	if err != nil {
		b.edit(msg.Chat.ID, wait, "❌ Ошибка базы данных\\.")
		return
	}
	_ = b.db.UpdateChannelInfo(msg.Chat.ID, username, info.ID, info.Title)
	_ = b.db.SetActiveChannel(msg.Chat.ID, username)

	if added {
		b.edit(msg.Chat.ID, wait, fmt.Sprintf(
			"✅ Канал *%s* \\(`@%s`\\) добавлен и установлен как активный\\!\n\nИспользуйте /sync для загрузки сообщений\\.",
			escMD(info.Title), escMD(username),
		))
	} else {
		b.edit(msg.Chat.ID, wait, fmt.Sprintf("Канал `@%s` установлен как активный\\.", escMD(username)))
	}
}

func (b *Bot) cmdChannels(msg *tgbotapi.Message) {
	channels, err := b.db.GetUserChannels(msg.Chat.ID)
	if err != nil || len(channels) == 0 {
		b.send(msg.Chat.ID, "У вас нет добавленных каналов\\. Используйте /add @channel")
		return
	}

	active, _ := b.db.GetActiveChannel(msg.Chat.ID)

	var sb strings.Builder
	sb.WriteString("📋 *Ваши каналы:*\n\n")
	for _, ch := range channels {
		title := ch.Title
		if title == "" {
			title = ch.Username
		}
		count, _ := b.db.MessageCount(ch.Username)
		marker := ""
		if strings.ToLower(ch.Username) == strings.ToLower(active) {
			marker = " ⭐ *[Активный]*"
		}
		fmt.Fprintf(&sb, "• *%s* \\(`@%s`\\)%s — %d сообщений\n",
			escMD(title), escMD(ch.Username), marker, count)
	}
	b.sendMD(msg.Chat.ID, sb.String())
}

func (b *Bot) cmdRemove(msg *tgbotapi.Message) {
	arg := strings.TrimSpace(msg.CommandArguments())
	if arg == "" {
		b.send(msg.Chat.ID, "Укажите канал: /remove @channelname")
		return
	}
	username := norm(arg)
	removed, err := b.db.RemoveChannel(msg.Chat.ID, username)
	if err != nil || !removed {
		b.sendMD(msg.Chat.ID, fmt.Sprintf("Канал `@%s` не найден в вашем списке\\.", escMD(username)))
		return
	}

	active, _ := b.db.GetActiveChannel(msg.Chat.ID)
	if strings.ToLower(username) == strings.ToLower(active) {
		_ = b.db.ClearActiveChannel(msg.Chat.ID)
	}

	b.sendMD(msg.Chat.ID, fmt.Sprintf("✅ Канал `@%s` удалён\\.", escMD(username)))
}

func (b *Bot) cmdSelect(msg *tgbotapi.Message) {
	arg := strings.TrimSpace(msg.CommandArguments())
	if arg == "" {
		b.send(msg.Chat.ID, "Укажите канал: /select @channelname")
		return
	}
	username := norm(arg)

	belongs, err := b.db.ChannelBelongsToUser(msg.Chat.ID, username)
	if err != nil || !belongs {
		b.sendMD(msg.Chat.ID, fmt.Sprintf("❌ Канал `@%s` не найден в вашем списке\\. Сначала добавьте его с помощью `/add @%s`\\.", escMD(username), escMD(username)))
		return
	}

	err = b.db.SetActiveChannel(msg.Chat.ID, username)
	if err != nil {
		b.send(msg.Chat.ID, "❌ Ошибка при выборе активного канала.")
		return
	}

	b.sendMD(msg.Chat.ID, fmt.Sprintf("⭐ Канал `@%s` теперь выбран как активный\\. Все вопросы в свободной форме будут задаваться по нему\\.", escMD(username)))
}

func (b *Bot) cmdActive(msg *tgbotapi.Message) {
	active, err := b.db.GetActiveChannel(msg.Chat.ID)
	if err != nil {
		b.send(msg.Chat.ID, "❌ Ошибка базы данных.")
		return
	}
	if active == "" {
		b.send(msg.Chat.ID, "У вас нет активного канала. Выберите его с помощью команды /select @channel или добавьте новый через /add @channel.")
		return
	}

	b.sendMD(msg.Chat.ID, fmt.Sprintf("⭐ Текущий активный канал: `@%s`\\. Все ваши вопросы будут анализировать историю этого канала\\.", escMD(active)))
}

func (b *Bot) cmdSync(ctx context.Context, msg *tgbotapi.Message) {
	channels, _ := b.db.GetUserChannels(msg.Chat.ID)
	if len(channels) == 0 {
		b.send(msg.Chat.ID, "Нет каналов для синхронизации\\. Добавьте их через /add")
		return
	}

	wait := b.sendMD(msg.Chat.ID, fmt.Sprintf("⏳ Синхронизирую %d канал\\(ов\\)…", len(channels)))

	results, err := b.collector.CollectForUser(ctx, msg.Chat.ID)
	if err != nil {
		b.edit(msg.Chat.ID, wait, "❌ Ошибка при синхронизации\\.")
		return
	}

	var sb strings.Builder
	sb.WriteString("✅ *Синхронизация завершена:*\n\n")
	for username, count := range results {
		total, _ := b.db.MessageCount(username)
		fmt.Fprintf(&sb, "• `@%s`: \\+%d новых \\(всего %d\\)\n", escMD(username), count, total)
	}
	b.edit(msg.Chat.ID, wait, sb.String())
}

func (b *Bot) cmdSummary(ctx context.Context, msg *tgbotapi.Message) {
	arg := strings.TrimSpace(msg.CommandArguments())
	if arg == "" {
		b.send(msg.Chat.ID, "Укажите канал: /summary @channelname")
		return
	}
	username := norm(arg)

	ok, _ := b.db.ChannelBelongsToUser(msg.Chat.ID, username)
	if !ok {
		b.sendMD(msg.Chat.ID, fmt.Sprintf(
			"Канал `@%s` не в вашем списке\\. Сначала добавьте его через /add", escMD(username),
		))
		return
	}

	wait := b.sendMD(msg.Chat.ID, "⏳ Генерирую резюме…")

	newMsgs, _ := b.collector.Collect(ctx, username)
	msgs, err := b.db.SearchMessages([]string{username}, "", 60)
	if err != nil {
		b.edit(msg.Chat.ID, wait, "❌ Ошибка при чтении сообщений\\.")
		return
	}

	summary, err := b.ai.SummarizeChannel(ctx, username, msgs)
	if err != nil {
		b.edit(msg.Chat.ID, wait, fmt.Sprintf("❌ Ошибка Claude API: %s", escMD(err.Error())))
		return
	}

	suffix := ""
	if newMsgs > 0 {
		suffix = fmt.Sprintf("\n\n_\\+%d новых сообщений загружено_", newMsgs)
	}
	b.edit(msg.Chat.ID, wait, fmt.Sprintf("📝 *Резюме @%s:*\n\n%s%s", escMD(username), escMD(summary), suffix))
}

// ── Free-text question ────────────────────────────────────────────────────────

func (b *Bot) handleQuestion(ctx context.Context, msg *tgbotapi.Message) {
	active, err := b.db.GetActiveChannel(msg.Chat.ID)
	if err != nil {
		b.send(msg.Chat.ID, "❌ Ошибка базы данных.")
		return
	}
	if active == "" {
		b.send(msg.Chat.ID, "Пожалуйста, сначала выберите активный канал с помощью `/select @channel` или добавьте его через `/add @channel`.")
		return
	}

	wait := b.sendMD(msg.Chat.ID, fmt.Sprintf("🔍 Анализирую сообщения канала @%s…", escMD(active)))

	// Get recent 300 messages to feed into Gemini context
	msgs, err := b.db.GetRecentMessages(active, 300)
	if err != nil {
		b.edit(msg.Chat.ID, wait, "❌ Ошибка при чтении сообщений из базы данных\\.")
		return
	}

	answer, err := b.ai.AnswerQuestion(ctx, msg.Text, msgs, active)
	if err != nil {
		b.edit(msg.Chat.ID, wait, fmt.Sprintf("❌ Ошибка Gemini API: %s", escMD(err.Error())))
		return
	}

	b.edit(msg.Chat.ID, wait, escMD(answer))
}

// ── Telegram helpers ──────────────────────────────────────────────────────────

func (b *Bot) send(chatID int64, text string) int {
	m := tgbotapi.NewMessage(chatID, text)
	sent, err := b.api.Send(m)
	if err != nil {
		log.Printf("[Bot] Error sending message: %v", err)
	}
	return sent.MessageID
}

func (b *Bot) sendMD(chatID int64, text string) int {
	m := tgbotapi.NewMessage(chatID, text)
	m.ParseMode = tgbotapi.ModeMarkdownV2
	sent, err := b.api.Send(m)
	if err != nil {
		log.Printf("[Bot] Error sending MD message: %v", err)
	}
	return sent.MessageID
}

func (b *Bot) edit(chatID int64, msgID int, text string) {
	e := tgbotapi.NewEditMessageText(chatID, msgID, text)
	e.ParseMode = tgbotapi.ModeMarkdownV2
	_, err := b.api.Send(e)
	if err != nil {
		log.Printf("[Bot] Error editing message %d: %v", msgID, err)
	}
}

// escMD escapes special characters for Telegram MarkdownV2.
func escMD(s string) string {
	replacer := strings.NewReplacer(
		`_`, `\_`, `*`, `\*`, `[`, `\[`, `]`, `\]`,
		`(`, `\(`, `)`, `\)`, `~`, `\~`, "`", "\\`",
		`>`, `\>`, `#`, `\#`, `+`, `\+`, `-`, `\-`,
		`=`, `\=`, `|`, `\|`, `{`, `\{`, `}`, `\}`,
		`.`, `\.`, `!`, `\!`,
	)
	return replacer.Replace(s)
}

func norm(s string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(s), "@"))
}
