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

	cfg := Config{
		HTTPPort:   port,
		RedisAddr:  redisAddr,
		MessageTTL: messageTTL,
		OTPTTL:     otpTTL,
	}

	return cfg
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

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "Zapp! Relay Server is running")
	})

	server := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: mux,
	}

	go func() {
		log.Printf("Listening on :%s...\n", cfg.HTTPPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v\n", err)
		}
	}()

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutdown signal received. Cleaning up...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("Shutdown failed: %v\n", err)
	}
	log.Println("Server exited cleanly.")
}
