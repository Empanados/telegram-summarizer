package collector

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"

	"telegram-summarizer/internal/storage"
)

type ChannelInfo struct {
	ID    int64
	Title string
}

// Collector wraps a Telethon-style MTProto client for reading public channels.
type Collector struct {
	appID   int
	appHash string
	session string
	db      *storage.DB
	maxMsgs int

	api    *tg.Client
	ready  chan struct{}
	done   chan struct{}
	cancel context.CancelFunc
}

func New(appID int, appHash, sessionPath string, db *storage.DB, maxMsgs int) *Collector {
	return &Collector{
		appID:   appID,
		appHash: appHash,
		session: sessionPath,
		db:      db,
		maxMsgs: maxMsgs,
		ready:   make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start connects the MTProto client using the stored session.
// Returns error if the session doesn't exist — run the setup binary first.
func (c *Collector) Start(parentCtx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(c.session), 0o755); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	ctx, cancel := context.WithCancel(parentCtx)
	c.cancel = cancel

	client := telegram.NewClient(c.appID, c.appHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: c.session},
	})

	errCh := make(chan error, 1)
	go func() {
		defer close(c.done)
		errCh <- client.Run(ctx, func(ctx context.Context) error {
			c.api = tg.NewClient(client)

			// Verify the session is authenticated.
			if _, err := c.api.UpdatesGetState(ctx); err != nil {
				return fmt.Errorf("not authenticated — run 'setup' first: %w", err)
			}

			close(c.ready)
			<-ctx.Done()
			return nil
		})
	}()

	select {
	case <-c.ready:
		return nil
	case err := <-errCh:
		if err != nil {
			return err
		}
		return fmt.Errorf("MTProto client stopped unexpectedly")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop gracefully shuts down the MTProto connection.
func (c *Collector) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	<-c.done
}

// ResolveChannel looks up a public channel by username.
func (c *Collector) ResolveChannel(ctx context.Context, username string) (*ChannelInfo, error) {
	username = norm(username)
	resolved, err := c.api.ContactsResolveUsername(ctx, username)
	if err != nil {
		return nil, fmt.Errorf("resolve @%s: %w", username, err)
	}
	for _, chat := range resolved.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			return &ChannelInfo{ID: ch.ID, Title: ch.Title}, nil
		}
	}
	return nil, fmt.Errorf("channel @%s not found", username)
}

// Collect fetches new messages from a channel and saves them to the DB.
// Returns the number of newly saved messages.
func (c *Collector) Collect(ctx context.Context, username string) (int, error) {
	username = norm(username)

	resolved, err := c.api.ContactsResolveUsername(ctx, username)
	if err != nil {
		return 0, fmt.Errorf("resolve @%s: %w", username, err)
	}

	var inputPeer tg.InputPeerClass
	for _, chat := range resolved.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			inputPeer = &tg.InputPeerChannel{
				ChannelID:  ch.ID,
				AccessHash: ch.AccessHash,
			}
			break
		}
	}
	if inputPeer == nil {
		return 0, fmt.Errorf("could not build input peer for @%s", username)
	}

	lastID, _ := c.db.GetLastMessageID(username)

	result, err := c.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:  inputPeer,
		Limit: c.maxMsgs,
		MinID: lastID,
	})
	if err != nil {
		return 0, fmt.Errorf("get history @%s: %w", username, err)
	}

	rawMsgs := extractMessages(result)
	msgs := make([]storage.Message, 0, len(rawMsgs))
	for _, raw := range rawMsgs {
		if m, ok := raw.(*tg.Message); ok && strings.TrimSpace(m.Message) != "" {
			msgs = append(msgs, storage.Message{
				MessageID: m.ID,
				Username:  username,
				Date:      time.Unix(int64(m.Date), 0).UTC(),
				Text:      m.Message,
			})
		}
	}

	return c.db.SaveMessages(username, msgs)
}

// CollectForUser syncs all channels belonging to a user.
func (c *Collector) CollectForUser(ctx context.Context, userID int64) (map[string]int, error) {
	channels, err := c.db.GetUserChannels(userID)
	if err != nil {
		return nil, err
	}
	results := make(map[string]int, len(channels))
	for _, ch := range channels {
		n, err := c.Collect(ctx, ch.Username)
		if err != nil {
			// Log but continue with other channels
			results[ch.Username] = 0
		} else {
			results[ch.Username] = n
		}
	}
	return results, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func norm(s string) string {
	return strings.ToLower(strings.TrimPrefix(s, "@"))
}

func extractMessages(result tg.MessagesMessagesClass) []tg.MessageClass {
	switch r := result.(type) {
	case *tg.MessagesMessages:
		return r.Messages
	case *tg.MessagesMessagesSlice:
		return r.Messages
	case *tg.MessagesChannelMessages:
		return r.Messages
	default:
		return nil
	}
}
