package main

import (
	"crypto/tls"
"crypto/x509"
"net/http"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	maxbot "github.com/max-messenger/max-bot-api-client-go"
)

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "Environment variable %s is not set\n", key)
		os.Exit(1)
	}
	return v
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func genKey() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func logLevel() slog.Level {
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()})))

	cfg := Config{
		MaxToken:              mustEnv("MAX_TOKEN"),
		MaxTokenOld:           os.Getenv("MAX_TOKEN_OLD"),
		TgBotURL:              envOr("TG_BOT_URL", "https://t.me/MaxTelegramBridgeBot"),
		MaxBotURL:             envOr("MAX_BOT_URL", "https://max.ru/id710708943262_bot"),           // основной (старый разбанен)
		MaxBotURLReserve:      envOr("MAX_BOT_URL_RESERVE", "https://max.ru/id710708943262_4_bot"), // запасной (failover)
		MaxWebhookURL:         os.Getenv("MAX_WEBHOOK_URL"),
		MaxWebhookPort:        envOr("MAX_WEBHOOK_PORT", "8443"),
		MaxCommentProbeMarker: os.Getenv("MAX_COMMENT_PROBE_MARKER"),
		TgWebhookURL:          os.Getenv("TG_WEBHOOK_URL"),
		TgWebhookPort:         envOr("TG_WEBHOOK_PORT", "8444"),
		TgAPIURL:              os.Getenv("TG_API_URL"),
	}

	// Parse ALLOWED_USERS whitelist
	if v := os.Getenv("ALLOWED_USERS"); v != "" {
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			id, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				slog.Error("Invalid ALLOWED_USERS value", "value", s, "err", err)
				os.Exit(1)
			}
			cfg.AllowedUsers = append(cfg.AllowedUsers, id)
		}
		slog.Info("User whitelist enabled", "count", len(cfg.AllowedUsers))
	}

	// Parse file size limits
	if v := os.Getenv("TG_MAX_FILE_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.TgMaxFileSizeMB = n
		} else {
			slog.Error("Invalid TG_MAX_FILE_SIZE_MB value", "value", v)
			os.Exit(1)
		}
	}
	if v := os.Getenv("MAX_MAX_FILE_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
			cfg.MaxMaxFileSizeMB = n
		} else {
			slog.Error("Invalid MAX_MAX_FILE_SIZE_MB value", "value", v)
			os.Exit(1)
		}
	}

	// Parse MAX_ALLOWED_EXTENSIONS whitelist (e.g. "pdf,docx,zip")
	// Если не задан — расширения не проверяются локально (ошибка придёт от CDN).
	if v := os.Getenv("MAX_ALLOWED_EXTENSIONS"); v != "" {
		cfg.MaxAllowedExts = make(map[string]struct{})
		for _, ext := range strings.Split(v, ",") {
			ext = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(ext, ".")))
			if ext != "" {
				cfg.MaxAllowedExts[ext] = struct{}{}
			}
		}
		slog.Info("MAX file extension whitelist enabled", "count", len(cfg.MaxAllowedExts))
	}

	// MESSAGE_FORMAT=newline — текст с новой строки после имени: "Имя:\nтекст"
	// По умолчанию (или MESSAGE_FORMAT=inline) — "Имя: текст"
	if strings.ToLower(os.Getenv("MESSAGE_FORMAT")) == "newline" {
		cfg.MessageNewline = true
		slog.Info("Message format: newline")
	}

	// DISABLE_PREFIX=true — глобально выключить префиксы [TG]/[MAX] на всех чатах.
	if v := strings.ToLower(os.Getenv("DISABLE_PREFIX")); v == "true" || v == "1" || v == "yes" {
		cfg.DisablePrefix = true
		slog.Info("Prefix [TG]/[MAX] globally disabled via DISABLE_PREFIX")
	}

	dbPath := envOr("DB_PATH", "bridge.db")

	var repo Repository
	var err error
	if dsn := os.Getenv("DATABASE_URL"); dsn != "" {
		repo, err = NewPostgresRepo(dsn)
		if err != nil {
			slog.Error("PostgreSQL error", "err", err)
			os.Exit(1)
		}
		slog.Info("DB: PostgreSQL")
	} else {
		repo, err = NewSQLiteRepo(dbPath)
		if err != nil {
			slog.Error("SQLite error", "err", err)
			os.Exit(1)
		}
		slog.Info("DB: SQLite", "path", dbPath)
	}
	defer repo.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := envOr("PORT", "10000")

mux := http.NewServeMux()
mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})
whatsappVerifyToken := os.Getenv("WHATSAPP_VERIFY_TOKEN")

mux.HandleFunc("/whatsapp/webhook", func(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		mode := r.URL.Query().Get("hub.mode")
		token := r.URL.Query().Get("hub.verify_token")
		challenge := r.URL.Query().Get("hub.challenge")

		if mode == "subscribe" && whatsappVerifyToken != "" && token == whatsappVerifyToken {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(challenge))
			return
		}

		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

if r.Method == http.MethodPost {
	slog.Info("WhatsApp webhook POST received")
	w.WriteHeader(http.StatusOK)
	return

	}

	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
})
go func() {
	if err := http.ListenAndServe("0.0.0.0:"+port, mux); err != nil {
		slog.Error("HTTP server error", "err", err)
	}
}()

	tgToken := mustEnv("TG_TOKEN")
	tg, err := NewTGBotSender(ctx, tgToken, cfg.TgAPIURL)
	if err != nil {
		slog.Error("TG bot error", "err", err)
		os.Exit(1)
	}
roots, err := x509.SystemCertPool()
if err != nil || roots == nil {
	roots = x509.NewCertPool()
}

for _, certPath := range []string{
	"/usr/local/share/ca-certificates/russian-trusted-ca.crt",
	"/usr/local/share/ca-certificates/russian-trusted-sub-ca.crt",
} {
	pemData, err := os.ReadFile(certPath)
	if err != nil {
		slog.Error("read MAX CA", "path", certPath, "err", err)
		os.Exit(1)
	}
	if ok := roots.AppendCertsFromPEM(pemData); !ok {
		slog.Error("parse MAX CA", "path", certPath)
		os.Exit(1)
	}
}

maxTransport := http.DefaultTransport.(*http.Transport).Clone()
maxTransport.TLSClientConfig = &tls.Config{
	RootCAs:    roots,
	MinVersion: tls.VersionTLS12,
}
maxHTTPClient := &http.Client{Transport: maxTransport}
maxApi, err := maxbot.New(cfg.MaxToken, maxbot.WithBaseURL(maxAPIBaseURL), maxbot.WithHTTPClient(maxHTTPClient))
	if err != nil {
		slog.Error("MAX bot error", "err", err)
		os.Exit(1)
	}
	var maxUID int64
	maxInfo, err := maxApi.Bots.GetBot(context.Background())
	if err != nil {
		// НЕ валим старт, если MAX недоступен или аккаунт бота заблокирован
		// (403 account.blocked) — поднимаемся в деградированном режиме: Telegram,
		// polling продолжает работать; MAX-отправки уходят в очередь.
		// Иначе при бане/простое MAX любой рестарт убивал бы бота целиком (был
		// прод-инцидент: GetBot → account.blocked → os.Exit → крэш-луп).
		slog.Error("MAX bot info error — продолжаю в деградированном режиме (MAX недоступен/заблокирован)", "err", err)
	} else {
		maxUID = maxInfo.UserId
		slog.Info("MAX bot started", "name", maxInfo.Name)
	}

	// Второй (старый) MAX-бот — резерв + обслуживание чатов, где нового бота нет.
	// Best-effort: если токен не задан/невалиден — работаем одним ботом.
	var maxApiOld *maxbot.Api
	var maxOldUID int64
	if cfg.MaxTokenOld != "" {
		if old, err := maxbot.New(cfg.MaxTokenOld, maxbot.WithBaseURL(maxAPIBaseURL)); err != nil {
			slog.Error("MAX old bot init error — работаю без резервного бота", "err", err)
		} else {
			maxApiOld = old
			if info, err := old.Bots.GetBot(context.Background()); err != nil {
				slog.Error("MAX old bot info error", "err", err)
			} else {
				maxOldUID = info.UserId
				slog.Info("MAX old bot started", "name", info.Name, "uid", maxOldUID)
			}
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		// Grace-период: даём in-flight кросспостам (загрузка фото/видео в MAX CDN + отправка)
		// докрутиться перед отменой ctx. Иначе рестарт/деплой рвёт их «context canceled» и бот
		// сообщает о ложной ошибке отправки. deploy.sh делает atomic swap, новый
		// процесс уже принимает трафик, так что переживём короткий overlap.
		slog.Info("Shutting down... (grace 8s for in-flight crossposts)")
		time.Sleep(8 * time.Second)
		cancel()
	}()

	bridge := NewBridge(cfg, repo, tg, maxApi, maxUID, maxApiOld, maxOldUID)
	bridge.Run(ctx)
	slog.Info("Bridge stopped")
}
