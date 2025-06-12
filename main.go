package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"
)

type Config struct {
	HTTPPort   string
	RedisAddr  string
	MessageTTL int
	OTPTTL     int
}

type MessageRequest struct {
	RecipientFingerprint string `json:"recipient_fingerprint"`
	EncryptedBlob        string `json:"encrypted_blob"`
}

type ReceiveRequest struct {
	RecipientFingerprint string `json:"recipient_fingerprint"`
}

func loadConfig() Config {
	port := getEnv("HTTP_PORT", "8080")
	redisAddr := getEnv("REDIS_ADDR", "localhost:6379")
	messageTTL := getEnvAsInt("MESSAGE_TTL_SECONDS", 300)
	otpTTL := getEnvAsInt("OTP_TTL_SECONDS", 90)

	return Config{
		HTTPPort:   port,
		RedisAddr:  redisAddr,
		MessageTTL: messageTTL,
		OTPTTL:     otpTTL,
	}
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvAsInt(key string, defaultVal int) int {
	if valStr := os.Getenv(key); valStr != "" {
		if val, err := strconv.Atoi(valStr); err == nil {
			return val
		}
		log.Printf("Invalid value for %s: %s, using default %d\n", key, valStr, defaultVal)
	}
	return defaultVal
}

func main() {
	cfg := loadConfig()
	log.Printf("Starting Zapp! Relay Server with config: %+v\n", cfg)

	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis at %s: %v", cfg.RedisAddr, err)
	}
	log.Println("Connected to Redis successfully.")

	mux := http.NewServeMux()

	// Root handler
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Zapp! Relay Server is running")
	})

	// Health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, "Redis unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	})

	// POST /send
	mux.HandleFunc("/send", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var msgReq MessageRequest
		if err := json.NewDecoder(r.Body).Decode(&msgReq); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if msgReq.RecipientFingerprint == "" || msgReq.EncryptedBlob == "" {
			http.Error(w, "Missing required fields", http.StatusBadRequest)
			return
		}

		messageID := uuid.NewString()
		key := "zapp:mailbox:" + msgReq.RecipientFingerprint

		if err := rdb.LPush(ctx, key, msgReq.EncryptedBlob).Err(); err != nil {
			log.Printf("Redis LPUSH error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if err := rdb.Expire(ctx, key, time.Duration(cfg.MessageTTL)*time.Second).Err(); err != nil {
			log.Printf("Redis EXPIRE error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		log.Printf("Stored message %s for recipient %s", messageID, msgReq.RecipientFingerprint)
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status": "success", "message_id": "%s"}`+"\n", messageID)
	})

	// POST /receive
	mux.HandleFunc("/receive", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ReceiveRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}

		if req.RecipientFingerprint == "" {
			http.Error(w, "Missing recipient_fingerprint", http.StatusBadRequest)
			return
		}

		key := "zapp:mailbox:" + req.RecipientFingerprint

		messages, err := rdb.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			log.Printf("Redis LRANGE error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if len(messages) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Read-once delete
		if err := rdb.Del(ctx, key).Err(); err != nil {
			log.Printf("Redis DEL error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		resp, err := json.Marshal(messages)
		if err != nil {
			log.Printf("JSON marshal error: %v", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(resp)
	})

	server := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: mux,
	}

	go func() {
		log.Printf("Listening on :%s...\n", cfg.HTTPPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutdown signal received. Cleaning up...")

	ctxShutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctxShutdown); err != nil {
		log.Fatalf("Shutdown failed: %v", err)
	}
	if err := rdb.Close(); err != nil {
		log.Printf("Redis client shutdown error: %v", err)
	}

	log.Println("Server exited cleanly.")
}
