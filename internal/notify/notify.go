// Package notify 提供「下载完成 / 出错」的出网通知。
//
// 只做两件事：把消息发出去、别把自己发炸（节流 + 超时 + 失败不阻塞主流程）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/YoisakiKnd/IWantWatchAnime/internal/config"
)

// Message 是一条通知。
type Message struct {
	Title string
	Body  string
}

// Multi 把消息扇出到所有已配置的通道。
type Multi struct {
	webhooks []string
	tgToken  string
	tgChat   string
	tgBase   string // Telegram API 根地址，测试时可换成假服务
	hc       *http.Client
	log      *slog.Logger

	mu    sync.Mutex
	last  map[string]time.Time // 节流：同一 key 在这个时间点之前不再发
	quiet time.Duration
}

// New 构造通知器；未配置任何通道时返回 nil（调用方需判空）。
func New(cfg config.Notify, hc *http.Client, log *slog.Logger) *Multi {
	if len(cfg.Webhooks) == 0 && cfg.Telegram.BotToken == "" {
		return nil
	}
	return &Multi{
		webhooks: cfg.Webhooks,
		tgToken:  cfg.Telegram.BotToken,
		tgChat:   cfg.Telegram.ChatID,
		hc:       hc,
		log:      log,
		tgBase:   "https://api.telegram.org",
		last:     map[string]time.Time{},
		quiet:    time.Duration(cfg.ThrottleMin) * time.Minute,
	}
}

// Enabled 报告是否配置了通道。
func (m *Multi) Enabled() bool { return m != nil }

// Send 发送一条通知。
//
// 节流按 Title 去重：番剧站偶发 502 时不会把同一个错误刷屏到 Telegram。
// 任何通道失败只记日志——通知是旁路，绝不能拖垮下载流水线。
func (m *Multi) Send(ctx context.Context, msg Message) {
	if m == nil {
		return
	}
	if m.quiet > 0 {
		m.mu.Lock()
		key := msg.Title
		if t, ok := m.last[key]; ok && time.Since(t) < m.quiet {
			m.mu.Unlock()
			return
		}
		m.last[key] = time.Now()
		m.mu.Unlock()
	}
	for _, hook := range m.webhooks {
		if err := m.postWebhook(ctx, hook, msg); err != nil {
			m.log.Warn("webhook 通知失败", "url", hook, "err", err)
		}
	}
	if m.tgToken != "" && m.tgChat != "" {
		if err := m.postTelegram(ctx, msg); err != nil {
			m.log.Warn("telegram 通知失败", "err", err)
		}
	}
}

func (m *Multi) postWebhook(ctx context.Context, hook string, msg Message) error {
	payload, _ := json.Marshal(map[string]any{
		"source": "suzu",
		"title":  msg.Title,
		"body":   msg.Body,
		"ts":     time.Now().Unix(),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (m *Multi) postTelegram(ctx context.Context, msg Message) error {
	form := url.Values{
		"chat_id": {m.tgChat},
		"text":    {strings.TrimSpace(msg.Title + "\n" + msg.Body)},
	}
	base := m.tgBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	endpoint := base + "/bot" + m.tgToken + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Description string `json:"description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("telegram HTTP %d: %s", resp.StatusCode, e.Description)
	}
	return nil
}
