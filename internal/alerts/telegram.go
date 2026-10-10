package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Telegram's limit is 4096 characters per message; stay well under it.
const maxMessage = 3500

type telegram struct {
	token, chatID string
	base          string // https://api.telegram.org, swapped in tests
	client        *http.Client
}

func telegramFromEnv() *telegram {
	token, chat := os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID")
	if token == "" || chat == "" {
		return nil
	}
	return &telegram{token: token, chatID: chat, base: "https://api.telegram.org",
		client: &http.Client{Timeout: 30 * time.Second}}
}

// Send posts one HTML message. Errors never include the token, which is part
// of the request URL.
func (t *telegram) Send(ctx context.Context, msg string) error {
	form := url.Values{
		"chat_id":                  {t.chatID},
		"text":                     {msg},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.base+"/bot"+t.token+"/sendMessage", strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("telegram: build request")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "stocks_but_fr-radar (+https://github.com/Antonio-1110/stocks_but_fr)")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: %s", strings.ReplaceAll(err.Error(), t.token, "<token>"))
	}
	defer resp.Body.Close()
	var body struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusOK || !body.OK {
		return fmt.Errorf("telegram: HTTP %d: %s", resp.StatusCode, body.Description)
	}
	return nil
}

// format turns the newly entered stocks into one or more messages.
func format(r rule, hits []Hit, asOf string) []string {
	head := fmt.Sprintf("<b>%s</b>: %d new\n<i>%s</i>\n", html.EscapeString(r.title), len(hits), html.EscapeString(asOf))
	var out []string
	var b strings.Builder
	b.WriteString(head)
	for _, h := range hits {
		line := fmt.Sprintf("\n<a href=\"https://goodinfo.tw/tw/StockDetail.asp?STOCK_ID=%s\">%s %s</a> %s\n%s\n",
			url.QueryEscape(h.Ticker), html.EscapeString(h.Ticker), html.EscapeString(h.Name),
			html.EscapeString(h.Industry), html.EscapeString(h.Detail))
		if b.Len()+len(line) > maxMessage {
			out = append(out, b.String())
			b.Reset()
			b.WriteString(head)
		}
		b.WriteString(line)
	}
	return append(out, b.String())
}
