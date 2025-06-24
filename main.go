package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	rdb        *redis.Client
	ctx        = context.Background()
	otpTTL     = 300 * time.Second
	messageTTL = 5 * time.Minute

	// Prometheus metrics
	requestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "zapp_requests_total",
			Help: "Total number of requests received by the Zapp! Relay Server",
		},
		[]string{"handler", "method"},
	)

	requestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "zapp_request_duration_seconds",
			Help:    "Duration of request processing in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"handler", "method"},
	)

	messagesSent = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zapp_messages_sent_total",
			Help: "Total number of messages successfully sent through relay",
		},
	)

	messagesReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zapp_messages_received_total",
			Help: "Total number of messages successfully retrieved by recipients",
		},
	)

	devicesLinked = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "zapp_devices_linked_total",
			Help: "Total number of successful device links",
		},
	)

	activeMailboxes = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "zapp_active_mailboxes",
			Help: "Number of active mailboxes in the system",
		},
	)
)

func main() {
	var redisOptions *redis.Options
	redisURL := os.Getenv("REDIS_URL")

	if redisURL != "" {
		// Attempt to parse the REDIS_URL
		opt, err := redis.ParseURL(redisURL)
		if err != nil {
			log.Fatalf("[FATAL] Failed to parse REDIS_URL: %v", err)
		}
		redisOptions = opt
		log.Printf("[INFO] Connecting to Redis using REDIS_URL")
	} else {
		// Fallback to separate REDIS_ADDR and REDIS_PASSWORD
		redisOptions = &redis.Options{
			Addr:     getEnv("REDIS_ADDR", "localhost:6379"),
			Password: getEnv("REDIS_PASSWORD", ""),
			DB:       0,
		}
		log.Printf("[INFO] Connecting to Redis using REDIS_ADDR and REDIS_PASSWORD")
	}

	rdb = redis.NewClient(redisOptions)

	// Ping Redis to ensure connection is established
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatalf("[FATAL] Could not connect to Redis: %v", err)
	}
	log.Printf("[INFO] Successfully connected to Redis!")

	if ttlStr := os.Getenv("OTP_TTL_SECONDS"); ttlStr != "" {
		if ttlVal, err := time.ParseDuration(ttlStr + "s"); err == nil {
			otpTTL = ttlVal
		}
	}

	if ttlStr := os.Getenv("MESSAGE_TTL_SECONDS"); ttlStr != "" {
		if ttlVal, err := time.ParseDuration(ttlStr + "s"); err == nil {
			messageTTL = ttlVal
		}
	}

	mux := http.NewServeMux()

	// Add Prometheus metrics endpoint
	mux.Handle("/metrics", promhttp.Handler())

	// Wrap handlers with Prometheus instrumentation
	mux.HandleFunc("/health", instrumentHandler("health", handleHealth))
	mux.HandleFunc("/receive", instrumentHandler("receive", handleReceive))
	mux.HandleFunc("/link/initiate", instrumentHandler("link_initiate", handleLinkInitiate))
	mux.HandleFunc("/link/complete", instrumentHandler("link_complete", handleLinkComplete))
	mux.HandleFunc("/relay", instrumentHandler("relay", handleRelay))

	port := getEnv("PORT", "8080")
	log.Printf("[INFO] Zapp! Relay Server starting on port %s with Prometheus metrics enabled", port)
	log.Fatal(http.ListenAndServe(":"+port, loggingMiddleware(mux)))
}

// instrumentHandler wraps an HTTP handler with Prometheus instrumentation
func instrumentHandler(name string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestsTotal.WithLabelValues(name, r.Method).Inc()

		timer := prometheus.NewTimer(prometheus.ObserverFunc(func(duration float64) {
			requestDuration.WithLabelValues(name, r.Method).Observe(duration)
		}))
		defer timer.ObserveDuration()

		handler(w, r)
	}
}

// Data Types

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

type LinkCompleteRequest struct {
	OTP         string `json:"otp"`
	DeviceName  string `json:"device_name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

type LinkedDevice struct {
	Name        string `json:"name"`
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
}

type LinkCompleteResponse struct {
	Status        string         `json:"status"`
	LinkedDevices []LinkedDevice `json:"linked_devices"`
}

type RelayRequest struct {
	Sender   string `json:"sender"`
	Receiver string `json:"receiver"`
	Payload  string `json:"payload"`
}

// Handlers

// handleHealth provides a basic health check for the server and Redis connection.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		sendErrorResponse(w, http.StatusMethodNotAllowed, "Only GET allowed")
		return
	}

	// Ping Redis to check connectivity
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Printf("[ERROR] Health check failed: Redis connection error: %v", err)
		sendErrorResponse(w, http.StatusServiceUnavailable, "Redis not connected")
		return
	}

	// If Redis is connected, return success
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "OK",
		"redis_connected": true,
		"server_time":     time.Now().Format(time.RFC3339),
	})
	log.Printf("[INFO] Health check successful")
}

func handleRelay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendErrorResponse(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	var req RelayRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil {
		log.Printf("[WARN] Invalid /relay payload: %v", err)
		sendErrorResponse(w, http.StatusBadRequest, "Invalid JSON or missing field")
		return
	}

	if req.Sender == "" || req.Receiver == "" || req.Payload == "" {
		sendErrorResponse(w, http.StatusBadRequest, "Missing required fields")
		return
	}

	key := "zapp:mailbox:" + req.Receiver
	entry := fmt.Sprintf(`{"from":"%s","message":%q}`, req.Sender, req.Payload)

	exists, err := rdb.Exists(ctx, key).Result()
	if err != nil {
		log.Printf("[ERROR] Redis EXISTS check failed for mailbox %s: %v", key, err)
		sendErrorResponse(w, http.StatusInternalServerError, "Failed to check mailbox existence")
		return
	}
	if exists == 0 {
		activeMailboxes.Inc()
	}

	if err := rdb.RPush(ctx, key, entry).Err(); err != nil {
		log.Printf("[ERROR] Redis RPUSH failed: %v", err)
		sendErrorResponse(w, http.StatusInternalServerError, "Failed to relay message")
		return
	}

	if err := rdb.Expire(ctx, key, messageTTL).Err(); err != nil {
		log.Printf("[ERROR] Redis EXPIRE failed for mailbox %s: %v", key, err)
		// Log the error but don't necessarily fail the request, as message is still queued.
		// In a real-world scenario, you might want more robust retry/error handling.
	}

	log.Printf("[INFO] Message from %s queued to %s", req.Sender, req.Receiver)
	messagesSent.Inc()

	// --- ADDED FOR RTT MEASUREMENT ---
	redisStartTime := time.Now()
	exists, err = rdb.Exists(ctx, key).Result()
	if err != nil {
	    log.Printf("[ERROR] Redis EXISTS check failed for mailbox %s: %v", key, err) //
	    sendErrorResponse(w, http.StatusInternalServerError, "Failed to check mailbox existence") //
	    return
	}
	log.Printf("[DEBUG] Redis EXISTS took %v", time.Since(redisStartTime))

	redisStartTime = time.Now()
	err = rdb.RPush(ctx, key, entry).Err()
	if err != nil {
	    log.Printf("[ERROR] Redis RPUSH failed: %v", err) //
	    sendErrorResponse(w, http.StatusInternalServerError, "Failed to relay message") //
	    return
	}
	log.Printf("[DEBUG] Redis RPUSH took %v", time.Since(redisStartTime))

	redisStartTime = time.Now()
	err = rdb.Expire(ctx, key, messageTTL).Err()
	if err != nil {
	    log.Printf("[ERROR] Redis EXPIRE failed for mailbox %s: %v", key, err) //
	}
	log.Printf("[DEBUG] Redis EXPIRE took %v", time.Since(redisStartTime))
	// --- END ADDED ---

	w.Header().Set("Content-Type", "application/json") //
	json.NewEncoder(w).Encode(map[string]string{"status": "message queued"}) //
}

func handleReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendErrorResponse(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	var req ReceiveRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil {
		log.Printf("[WARN] Invalid /receive payload: %v", err)
		sendErrorResponse(w, http.StatusBadRequest, "Invalid JSON or missing field")
		return
	}

	if req.RecipientFingerprint == "" {
		sendErrorResponse(w, http.StatusBadRequest, "Missing recipient_fingerprint")
		return
	}

	key := "zapp:mailbox:" + req.RecipientFingerprint
	msgs, err := rdb.LRange(ctx, key, 0, -1).Result()
	if err != nil {
		log.Printf("[ERROR] Redis LRange failed: %v", err)
		sendErrorResponse(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	if len(msgs) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// NEW: Check if the key exists before deleting, to avoid double decrement if DEL fails silently
	// or if another client deletes it right after LRange.
	// For simplicity, we'll keep it as is, as the `activeMailboxes.Dec()` assumes a successful deletion.
	// In a robust system, you might use a Lua script for atomic fetch-and-delete.
	if err := rdb.Del(ctx, key).Err(); err != nil {
		log.Printf("[ERROR] Failed to delete mailbox: %v", err)
		sendErrorResponse(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	activeMailboxes.Dec() // Decrement because the mailbox is now empty/deleted

	log.Printf("[INFO] Delivered %d message(s) to %s and deleted mailbox", len(msgs), req.RecipientFingerprint)
	messagesReceived.Inc()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(msgs)
}

func handleLinkInitiate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendErrorResponse(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	var req LinkInitiateRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil {
		log.Printf("[WARN] Invalid /link/initiate payload: %v", err)
		sendErrorResponse(w, http.StatusBadRequest, "Invalid JSON or missing field")
		return
	}

	if req.DeviceName == "" || req.PublicKey == "" || req.Fingerprint == "" {
		sendErrorResponse(w, http.StatusBadRequest, "All fields are required")
		return
	}

	var otp string
	const maxAttempts = 5
	for i := 0; i < maxAttempts; i++ {
		otp = generateOTP()
		key := "zapp:otp:" + otp
		exists, err := rdb.Exists(ctx, key).Result()
		if err != nil {
			log.Printf("[ERROR] Redis EXISTS failed: %v", err)
			sendErrorResponse(w, http.StatusInternalServerError, "Internal server error")
			return
		}
		if exists == 0 {
			data, _ := json.Marshal(req)
			if err := rdb.Set(ctx, key, data, otpTTL).Err(); err != nil {
				log.Printf("[ERROR] Redis SET failed: %v", err)
				sendErrorResponse(w, http.StatusInternalServerError, "Internal server error")
				return
			}
			break
		}
		if i == maxAttempts-1 {
			log.Printf("[ERROR] OTP generation failed after %d attempts", maxAttempts)
			sendErrorResponse(w, http.StatusInternalServerError, "OTP generation failed")
			return
		}
	}

	log.Printf("[INFO] OTP %s issued for device %s (%s)", otp, req.DeviceName, req.Fingerprint)
	resp := LinkInitiateResponse{
		OTP:                  otp,
		InitiatorFingerprint: req.Fingerprint,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func handleLinkComplete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		sendErrorResponse(w, http.StatusMethodNotAllowed, "Only POST allowed")
		return
	}

	var req LinkCompleteRequest
	body, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(body, &req) != nil {
		log.Printf("[WARN] Invalid /link/complete payload: %v", err)
		sendErrorResponse(w, http.StatusBadRequest, "Invalid JSON or missing field")
		return
	}

	if req.OTP == "" || req.DeviceName == "" || req.PublicKey == "" || req.Fingerprint == "" {
		sendErrorResponse(w, http.StatusBadRequest, "All fields are required")
		return
	}

	key := "zapp:otp:" + req.OTP
	data, err := rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		sendErrorResponse(w, http.StatusNotFound, "OTP not found or expired")
		return
	} else if err != nil {
		log.Printf("[ERROR] Redis GET failed: %v", err)
		sendErrorResponse(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	if err := rdb.Del(ctx, key).Err(); err != nil {
		log.Printf("[ERROR] Failed to delete OTP: %v", err)
		// This is a non-critical error for the user, but important for system state
		// We'll proceed with the response but log the error.
	}

	var initiator LinkInitiateRequest
	if err := json.Unmarshal([]byte(data), &initiator); err != nil {
		log.Printf("[ERROR] Invalid initiator JSON from Redis: %v", err)
		sendErrorResponse(w, http.StatusInternalServerError, "Internal server error")
		return
	}

	log.Printf("[INFO] Link complete: %s <--> %s", initiator.Fingerprint, req.Fingerprint)
	devicesLinked.Inc()
	resp := LinkCompleteResponse{
		Status: "success",
		LinkedDevices: []LinkedDevice{
			{
				Name:        initiator.DeviceName,
				PublicKey:   initiator.PublicKey,
				Fingerprint: initiator.Fingerprint,
			},
			{
				Name:        req.DeviceName,
				PublicKey:   req.PublicKey,
				Fingerprint: req.Fingerprint,
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Utility

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

func sendErrorResponse(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		remoteIP, _, _ := net.SplitHostPort(r.RemoteAddr)
		log.Printf("[INFO] %s %s from %s", r.Method, r.URL.Path, remoteIP)

		// Create a wrapped response writer to capture the status code
		wrw := &wrappedResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrw, r)

		duration := time.Since(start)
		log.Printf("[INFO] Completed %s in %v with status %d", r.URL.Path, duration, wrw.statusCode)
	})
}

// wrappedResponseWriter is a wrapper around http.ResponseWriter that captures the status code
type wrappedResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

// WriteHeader captures the status code and passes it to the wrapped ResponseWriter
func (wrw *wrappedResponseWriter) WriteHeader(statusCode int) {
	wrw.statusCode = statusCode
	wrw.ResponseWriter.WriteHeader(statusCode)
}
