package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"telegram-summarizer/internal/storage"
)

const (
	apiURL   = "https://api.anthropic.com/v1/messages"
	apiVer   = "2023-06-01"
	model    = "claude-sonnet-4-6"
	maxChars = 400 // per message in context
)

type Client struct {
	apiKey string
	http   *http.Client
}

func New(apiKey string) *Client {
	return &Client{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
}

// AnswerQuestion answers a user question based exclusively on the provided messages.
func (c *Client) AnswerQuestion(ctx context.Context, question string, msgs []storage.Message, channels []string) (string, error) {
	if len(msgs) == 0 {
		return "В добавленных каналах не найдено сообщений. Попробуйте выполнить /sync.", nil
	}

	channelsStr := "@" + strings.Join(channels, ", @")
	context_ := formatMessages(msgs, 50)

	prompt := fmt.Sprintf(`Ты помощник, который отвечает на вопросы строго на основе сообщений из Telegram-каналов пользователя. Не используй знания из других источников.

Каналы пользователя: %s

Сообщения из каналов:
%s

Вопрос: %s

Правила:
- Отвечай только на основе приведённых сообщений.
- Указывай источник (@channel) для каждого факта.
- Если ответа нет в сообщениях — честно скажи об этом.
- Отвечай на русском языке.`, channelsStr, context_, question)

	return c.call(ctx, prompt)
}

// SummarizeChannel returns a structured summary of the channel's recent messages.
func (c *Client) SummarizeChannel(ctx context.Context, username string, msgs []storage.Message) (string, error) {
	if len(msgs) == 0 {
		return fmt.Sprintf("В канале @%s нет сохранённых сообщений. Выполните /sync.", username), nil
	}

	context_ := formatMessages(msgs, 60)

	prompt := fmt.Sprintf(`Сделай структурированное резюме последних сообщений из Telegram-канала @%s.

Сообщения:
%s

Требования:
- Выдели 3–5 главных тем или событий.
- Для каждой темы — 1–2 предложения с сутью.
- В конце — одно предложение об общей тональности канала.
- Отвечай на русском языке.`, username, context_)

	return c.call(ctx, prompt)
}

// ── internal ──────────────────────────────────────────────────────────────────

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	Messages  []apiMessage `json:"messages"`
}

type apiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type apiResponse struct {
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) call(ctx context.Context, prompt string) (string, error) {
	body, err := json.Marshal(apiRequest{
		Model:     model,
		MaxTokens: 1500,
		Messages:  []apiMessage{{Role: "user", Content: prompt}},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", apiVer)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("anthropic request: %w", err)
	}
	defer resp.Body.Close()

	var result apiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("API error %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		return "", fmt.Errorf(msg)
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("empty response from Claude")
	}
	return result.Content[0].Text, nil
}

func formatMessages(msgs []storage.Message, limit int) string {
	if len(msgs) > limit {
		msgs = msgs[:limit]
	}
	var sb strings.Builder
	for _, m := range msgs {
		date := m.Date.Format("2006-01-02")
		text := strings.ReplaceAll(m.Text, "\n", " ")
		if len(text) > maxChars {
			text = text[:maxChars] + "…"
		}
		fmt.Fprintf(&sb, "[%s @%s] %s\n\n", date, m.Username, text)
	}
	return sb.String()
}
