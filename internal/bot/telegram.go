package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

type TGUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
}
type Message struct {
	ID   int64  `json:"message_id"`
	From TGUser `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text  string `json:"text"`
	Photo []struct {
		ID   string `json:"file_id"`
		Size int    `json:"file_size"`
	} `json:"photo"`
}
type Update struct {
	ID       int64    `json:"update_id"`
	Message  *Message `json:"message"`
	Callback *struct {
		ID      string   `json:"id"`
		From    TGUser   `json:"from"`
		Message *Message `json:"message"`
		Data    string   `json:"data"`
	} `json:"callback_query"`
}
type Button struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type Keyboard struct {
	Rows [][]Button `json:"inline_keyboard"`
}

func button(text, data string) Button { return Button{text, data} }
func keys(rows ...[]Button) *Keyboard { return &Keyboard{rows} }
func menu() *Keyboard {
	return keys([]Button{button("📷 Фото", "photo"), button("✍️ Вручную", "manual")}, []Button{button("📖 Дневник", "today"), button("📊 Остаток", "remaining")}, []Button{button("🎯 Норма", "target"), button("⚙️ Настройки", "profile")})
}

type Telegram struct {
	Token string
	HTTP  *http.Client
}

func (t *Telegram) call(ctx context.Context, method string, v, out any) error {
	b, _ := json.Marshal(v)
	req, e := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+t.Token+"/"+method, bytes.NewReader(b))
	if e != nil {
		return errors.New("ошибка Telegram запроса")
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := t.HTTP.Do(req)
	if e != nil {
		slog.Warn("Telegram request failed", "method", method)
		return errors.New("Telegram недоступен")
	}
	defer res.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Code        int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			Retry int `json:"retry_after"`
		} `json:"parameters"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&envelope) != nil {
		return errors.New("неверный ответ Telegram")
	}
	if !envelope.OK {
		description := envelope.Description
		if t.Token != "" {
			description = strings.ReplaceAll(description, t.Token, "[REDACTED]")
		}
		slog.Warn("Telegram API rejected request", "method", method, "http_status", res.StatusCode,
			"error_code", envelope.Code, "description", description, "retry_after", envelope.Parameters.Retry)
		return fmt.Errorf("Telegram API error %d: %s (retry_after=%d)", envelope.Code, description, envelope.Parameters.Retry)
	}
	if out != nil {
		return json.Unmarshal(envelope.Result, out)
	}
	return nil
}
func (t *Telegram) send(ctx context.Context, chat int64, text string, k *Keyboard) error {
	request := struct {
		ChatID      int64     `json:"chat_id"`
		Text        string    `json:"text"`
		ReplyMarkup *Keyboard `json:"reply_markup,omitempty"`
	}{ChatID: chat, Text: text, ReplyMarkup: k}
	return t.call(ctx, "sendMessage", request, nil)
}
func (t *Telegram) photo(ctx context.Context, file string) ([]byte, error) {
	var f struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if e := t.call(ctx, "getFile", map[string]string{"file_id": file}, &f); e != nil {
		return nil, e
	}
	if f.Size > 5<<20 {
		return nil, errors.New("фотография должна быть до 5 МБ")
	}
	if f.Path == "" || strings.Contains(f.Path, "..") {
		return nil, errors.New("неверный путь фотографии")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", "https://api.telegram.org/file/bot"+t.Token+"/"+f.Path, nil)
	if e != nil {
		return nil, errors.New("ошибка загрузки фото")
	}
	res, e := t.HTTP.Do(req)
	if e != nil {
		return nil, errors.New("не удалось скачать фото")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, errors.New("не удалось скачать фото")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (5<<20)+1))
	if e != nil || len(b) > 5<<20 {
		return nil, errors.New("фотография должна быть до 5 МБ")
	}
	return b, nil
}
func NewTelegram(token string) *Telegram {
	return &Telegram{Token: token, HTTP: &http.Client{Timeout: 45 * time.Second}}
}
