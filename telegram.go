package main

// Минимальный клиент Telegram Bot API на net/http — без внешних зависимостей.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type tgClient struct {
	base string // https://api.telegram.org/bot<token>
	http *http.Client
}

// newTG создаёт клиент. proxy — необязательный URL прокси (http://, https:// или socks5://).
func newTG(token, apiURL, proxy string) (*tgClient, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("TELEGRAM_PROXY: %w", err)
		}
		tr.Proxy = http.ProxyURL(u)
	}
	return &tgClient{
		base: strings.TrimRight(apiURL, "/") + "/bot" + token,
		// Таймаут больше, чем long polling (50 с), чтобы getUpdates не обрывался.
		http: &http.Client{Transport: tr, Timeout: 75 * time.Second},
	}, nil
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
}

// call выполняет метод Bot API с JSON-телом и раскладывает result в out (если out != nil).
func (c *tgClient) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// Токен сидит в URL — не даём ему утечь в логи через текст ошибки.
		return errors.New(strings.ReplaceAll(err.Error(), c.base, "<telegram>"))
	}
	defer resp.Body.Close()
	var r tgResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("%s: разбор ответа: %w", method, err)
	}
	if !r.OK {
		return fmt.Errorf("%s: %d %s", method, r.ErrorCode, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type Message struct {
	MessageID int64  `json:"message_id"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
	Text      string `json:"text"`
}

type Chat struct {
	ID int64 `json:"id"`
}

type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Button struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type Keyboard struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

func (c *tgClient) getUpdates(ctx context.Context, offset int64) ([]Update, error) {
	var ups []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         50,
		"allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// send отправляет HTML-сообщение. Если Telegram не смог разобрать разметку,
// повторяет отправку обычным текстом, чтобы сообщение не потерялось.
func (c *tgClient) send(ctx context.Context, chatID int64, html string, kb *Keyboard) error {
	params := map[string]any{
		"chat_id":                  chatID,
		"text":                     truncRunes(html, 4096),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb != nil {
		params["reply_markup"] = kb
	}
	err := c.call(ctx, "sendMessage", params, nil)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(params, "parse_mode")
		params["text"] = truncRunes(stripTags(html), 4096)
		err = c.call(ctx, "sendMessage", params, nil)
	}
	return err
}

// edit заменяет текст и кнопки уже отправленного сообщения.
func (c *tgClient) edit(ctx context.Context, chatID, messageID int64, html string, kb *Keyboard) error {
	params := map[string]any{
		"chat_id":                  chatID,
		"message_id":               messageID,
		"text":                     truncRunes(html, 4096),
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}
	if kb != nil {
		params["reply_markup"] = kb
	}
	err := c.call(ctx, "editMessageText", params, nil)
	if err != nil && strings.Contains(err.Error(), "can't parse entities") {
		delete(params, "parse_mode")
		params["text"] = truncRunes(stripTags(html), 4096)
		err = c.call(ctx, "editMessageText", params, nil)
	}
	return err
}

func (c *tgClient) answerCallback(ctx context.Context, id, text string) {
	_ = c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}

func (c *tgClient) setCommands(ctx context.Context, cmds [][2]string) error {
	list := make([]map[string]string, 0, len(cmds))
	for _, cm := range cmds {
		list = append(list, map[string]string{"command": cm[0], "description": cm[1]})
	}
	return c.call(ctx, "setMyCommands", map[string]any{"commands": list}, nil)
}
