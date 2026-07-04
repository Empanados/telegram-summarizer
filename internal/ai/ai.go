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
	maxChars = 600 // max chars per message in context
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

// AnswerQuestion answers a user question based exclusively on the provided messages of the active channel.
func (c *Client) AnswerQuestion(ctx context.Context, question string, msgs []storage.Message, channel string) (string, error) {
	if len(msgs) == 0 {
		return "В канале не найдено сообщений. Пожалуйста, выполните /sync для загрузки истории.", nil
	}

	context_ := formatMessages(msgs)

	prompt := fmt.Sprintf(`Ты — ассистент, который отвечает на вопросы пользователя, используя исключительно предоставленный лог сообщений из Telegram-канала.
Твоя задача — строго следовать правилам ниже.

Правила:
1. Отвечай на вопрос пользователя только на основе информации из приведённых сообщений канала.
2. Не используй никаких внешних знаний, предположений или информации, которой нет в предоставленном логе.
3. Если в сообщениях канала нет ответа на вопрос, ответь строго следующей фразой: "В истории этого канала нет информации по вашему вопросу."
4. Отвечай на русском языке.

Выбранный Telegram-канал: @%s

Сообщения из канала (в хронологическом порядке):
---
%s
---

Вопрос пользователя: %s

Твой ответ:`, channel, context_, question)

	return c.call(ctx, prompt)
}

// SummarizeChannel returns a structured summary of the channel's recent messages.
func (c *Client) SummarizeChannel(ctx context.Context, username string, msgs []storage.Message) (string, error) {
	if len(msgs) == 0 {
		return fmt.Sprintf("В канале @%s нет сохранённых сообщений. Выполните /sync.", username), nil
	}

	context_ := formatMessages(msgs)

	prompt := fmt.Sprintf(`Сделай структурированное резюме последних сообщений из Telegram-канала @%s.

Сообщения:
---
%s
---

Требования:
- Выдели 3–5 главных тем или событий.
- Для каждой темы — 1–2 предложения с сутью.
- В конце — одно предложение об общей тональности канала.
- Отвечай на русском языке.`, username, context_)

	return c.call(ctx, prompt)
}

// ── Gemini API Integration ──────────────────────────────────────────────────

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type geminiEmbedRequest struct {
	Content              geminiContent `json:"content"`
	OutputDimensionality int           `json:"output_dimensionality,omitempty"`
}

type geminiBatchRequest struct {
	Model                string        `json:"model"`
	Content              geminiContent `json:"content"`
	OutputDimensionality int           `json:"output_dimensionality,omitempty"`
}

type geminiBatchEmbedRequest struct {
	Requests []geminiBatchRequest `json:"requests"`
}

type geminiEmbedResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

type geminiBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
}

func (c *Client) call(ctx context.Context, prompt string) (string, error) {
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: prompt},
				},
			},
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-3.1-flash-lite:generateContent?key=%s", c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	var result geminiResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode gemini response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("Gemini API error %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		return "", fmt.Errorf(msg)
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from Gemini")
	}

	return result.Candidates[0].Content.Parts[0].Text, nil
}

func formatMessages(msgs []storage.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		date := m.Date.Format("2006-01-02 15:04:05")
		text := strings.ReplaceAll(m.Text, "\n", " ")
		if len(text) > maxChars {
			text = text[:maxChars] + "…"
		}
		fmt.Fprintf(&sb, "[%s] %s\n", date, text)
	}
	return sb.String()
}

func (c *Client) EmbedText(ctx context.Context, text string) ([]float32, error) {
	reqBody := geminiEmbedRequest{
		Content: geminiContent{
			Parts: []geminiPart{
				{Text: text},
			},
		},
		OutputDimensionality: 768,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-embedding-2:embedContent?key=%s", c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini embed status %d", resp.StatusCode)
	}

	var result geminiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return result.Embedding.Values, nil
}

func (c *Client) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqs := make([]geminiBatchRequest, len(texts))
	for i, txt := range texts {
		reqs[i] = geminiBatchRequest{
			Model: "models/gemini-embedding-2",
			Content: geminiContent{
				Parts: []geminiPart{
					{Text: txt},
				},
			},
			OutputDimensionality: 768,
		}
	}

	reqBody := geminiBatchEmbedRequest{Requests: reqs}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/gemini-embedding-2:batchEmbedContents?key=%s", c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gemini batch embed status %d", resp.StatusCode)
	}

	var result geminiBatchEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	vals := make([][]float32, len(result.Embeddings))
	for i, emb := range result.Embeddings {
		vals[i] = emb.Values
	}
	return vals, nil
}
