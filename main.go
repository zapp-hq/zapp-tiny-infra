package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
)

type Config struct {
	HTTPPort   string
	RedisAddr  string
	MessageTTL int
	OTPTTL     int
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

	// Initialize Redis client
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
	})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("Failed to connect to Redis at %s: %v", cfg.RedisAddr, err)
	}
	log.Println("Connected to Redis successfully.")

	// Set up HTTP routes
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Zapp! Relay Server is running")
	})

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, "Redis unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "OK")
	})

	server := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: mux,
	}

	// Start server in background
	go func() {
		log.Printf("Listening on :%s...\n", cfg.HTTPPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutdown signal received. Cleaning up...")

	// Graceful shutdown
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
