package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
)

var (
	rdb    *redis.Client
	ctx    = context.Background()
	otpTTL = 300 * time.Second
)

func main() {
	// Initialize Redis client
	rdb = redis.NewClient(&redis.Options{
		Addr:     getEnv("REDIS_ADDR", "localhost:6379"),
		Password: getEnv("REDIS_PASSWORD", ""),
		DB:       0,
	})

	if ttlStr := os.Getenv("OTP_TTL_SECONDS"); ttlStr != "" {
		if ttlVal, err := time.ParseDuration(ttlStr + "s"); err == nil {
			otpTTL = ttlVal
		}
	}

	http.HandleFunc("/receive", handleReceive)
	http.HandleFunc("/link/initiate", handleLinkInitiate)

	port := getEnv("PORT", "8080")
	log.Println("Zapp! Relay Server running on port", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// Structs

type ReceiveRequest struct {
	RecipientFingerprint string `json:"recipient_fingerprint"`
}

type LinkInitiateRequest struct {
	DeviceName  string `json:"device_name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

type LinkInitiateResponse struct {
	OTP                  string `json:"otp"`
	InitiatorFingerprint string `json:"initiator_fingerprint"`
}

// Handlers

func handleReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req ReceiveRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil || req.RecipientFingerprint == "" {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	key := "zapp:mailbox:" + req.RecipientFingerprint
	msgs, err := rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		http.Error(w, "Redis error", http.StatusInternalServerError)
		return
	}

	if len(msgs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := rdb.Del(ctx, key).Err(); err != nil {
		http.Error(w, "Failed to delete mailbox", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

func handleLinkInitiate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req LinkInitiateRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil ||
		req.DeviceName == "" || req.PublicKey == "" || req.Fingerprint == "" {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	var otp string
	const maxAttempts = 5
	for i := 0; i < maxAttempts; i++ {
		otp = generateOTP()
		key := "zapp:otp:" + otp
		exists, err := rdb.Exists(ctx, key).Result()
		if err != nil {
			http.Error(w, "Redis error", http.StatusInternalServerError)
			return
		}
		if exists == 0 {
			data, _ := json.Marshal(req)
			if err := rdb.Set(ctx, key, data, otpTTL).Err(); err != nil {
				http.Error(w, "Redis write error", http.StatusInternalServerError)
				return
			}
			break
		}
		if i == maxAttempts-1 {
			http.Error(w, "OTP generation failed", http.StatusInternalServerError)
			return
		}
	}

	resp := LinkInitiateResponse{
		OTP:                  otp,
		InitiatorFingerprint: req.Fingerprint,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Helper Functions

func generateOTP() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1000000))
	return fmt.Sprintf("%06d", n.Int64())
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}
