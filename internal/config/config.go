package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppEnv   string
	HTTPAddr string

	DatabaseURL string
	RedisAddr   string

	SourceStorageRoot string

	CookieHMACSecret    string
	CookieMaxAgeSeconds int
	PublicBaseURL       string
	PublicBaseOrigin    string

	MaxSourceBytes   int64
	RequestBodyLimit int64

	QueueMaxDepth         int
	QueueRedisKey         string
	StreamGroup           string
	StreamConsumer        string
	RedisOperationTimeout time.Duration

	SubmitRatePerMinute   int
	SubmitRatePerHour     int
	IPSubmitRatePerMinute int
	PollRatePerMinute     int

	MaxQueuedPerParticipant  int
	MaxRunningPerParticipant int

	MockWorkDuration   time.Duration
	WorkerHardTimeout  time.Duration
	WorkerStaleAfter   time.Duration
	OutboxPollInterval time.Duration
	OutboxMaxBackoff   time.Duration

	CSRFTokenDuration      time.Duration
	TrustedProxies         []*net.IPNet
	UseTrustedProxyHeaders bool
}

func Load() (*Config, error) {
	for _, key := range []string{"MOCK_WORK_DURATION", "REDIS_OPERATION_TIMEOUT", "WORKER_HARD_TIMEOUT", "WORKER_STALE_AFTER", "OUTBOX_POLL_INTERVAL", "OUTBOX_MAX_BACKOFF"} {
		if raw := os.Getenv(key); raw != "" {
			if _, err := time.ParseDuration(raw); err != nil {
				return nil, fmt.Errorf("invalid %s duration", key)
			}
		}
	}
	appEnv := getEnv("APP_ENV", "development")

	publicBase := getEnv("PUBLIC_BASE_URL", "http://localhost:8080")
	origin := publicBase
	if idx := strings.Index(origin, "://"); idx >= 0 {
		if hostIdx := strings.Index(origin[idx+3:], "/"); hostIdx >= 0 {
			origin = origin[:idx+3+hostIdx]
		}
	}

	cfg := &Config{
		AppEnv:                   appEnv,
		HTTPAddr:                 getEnv("HTTP_ADDR", ":8080"),
		DatabaseURL:              getEnv("DATABASE_URL", "postgres://hustack:hustack@localhost:5432/hustack?sslmode=disable"),
		RedisAddr:                getEnv("REDIS_ADDR", "localhost:6379"),
		SourceStorageRoot:        getEnv("SOURCE_STORAGE_ROOT", "/var/lib/hustack/sources"),
		CookieHMACSecret:         getEnv("COOKIE_HMAC_SECRET", ""),
		CookieMaxAgeSeconds:      604800,
		PublicBaseURL:            publicBase,
		PublicBaseOrigin:         origin,
		MaxSourceBytes:           getEnvInt64("MAX_SOURCE_BYTES", 10485760),
		RequestBodyLimit:         getEnvInt64("REQUEST_BODY_LIMIT", 11534336),
		QueueMaxDepth:            getEnvInt("QUEUE_MAX_DEPTH", 500),
		QueueRedisKey:            getEnv("STREAM_KEY", "hustack:queue:v2"),
		StreamGroup:              getEnv("STREAM_GROUP", "mock-workers"),
		StreamConsumer:           getEnv("STREAM_CONSUMER", "worker-1"),
		RedisOperationTimeout:    getEnvDuration("REDIS_OPERATION_TIMEOUT", 2*time.Second),
		SubmitRatePerMinute:      getEnvInt("SUBMIT_RATE_PER_MINUTE", 6),
		SubmitRatePerHour:        getEnvInt("SUBMIT_RATE_PER_HOUR", 120),
		IPSubmitRatePerMinute:    getEnvInt("IP_SUBMIT_RATE_PER_MINUTE", 20),
		PollRatePerMinute:        getEnvInt("POLL_RATE_PER_MINUTE", 60),
		MaxQueuedPerParticipant:  getEnvInt("MAX_QUEUED_PER_PARTICIPANT", 3),
		MaxRunningPerParticipant: getEnvInt("MAX_RUNNING_PER_PARTICIPANT", 1),
		MockWorkDuration:         getEnvDuration("MOCK_WORK_DURATION", 200*time.Millisecond),
		WorkerHardTimeout:        getEnvDuration("WORKER_HARD_TIMEOUT", 5*time.Second),
		WorkerStaleAfter:         getEnvDuration("WORKER_STALE_AFTER", 30*time.Second),
		OutboxPollInterval:       getEnvDuration("OUTBOX_POLL_INTERVAL", 200*time.Millisecond),
		OutboxMaxBackoff:         getEnvDuration("OUTBOX_MAX_BACKOFF", 30*time.Second),
		CSRFTokenDuration:        2 * time.Hour,
		UseTrustedProxyHeaders:   getEnv("TRUSTED_PROXY_CIDR", "") != "",
	}

	if cfg.CookieHMACSecret == "" {
		if appEnv == "production" {
			return nil, fmt.Errorf("COOKIE_HMAC_SECRET is required in production mode")
		}
		cfg.CookieHMACSecret = "dev-secret-change-in-production-min-32-chars!!"
	}

	if appEnv == "production" && len(cfg.CookieHMACSecret) < 32 {
		return nil, fmt.Errorf("COOKIE_HMAC_SECRET must be at least 32 characters in production mode")
	}
	if cfg.QueueMaxDepth <= 0 || cfg.MaxQueuedPerParticipant <= 0 || cfg.MaxRunningPerParticipant <= 0 {
		return nil, fmt.Errorf("queue limits must be positive")
	}
	if cfg.RedisOperationTimeout <= 0 || cfg.MockWorkDuration <= 0 || cfg.WorkerHardTimeout <= 0 || cfg.WorkerStaleAfter <= 0 || cfg.OutboxPollInterval <= 0 || cfg.OutboxMaxBackoff <= 0 {
		return nil, fmt.Errorf("worker and Redis durations must be positive")
	}
	if !(cfg.WorkerStaleAfter > cfg.WorkerHardTimeout && cfg.WorkerHardTimeout > cfg.MockWorkDuration) {
		return nil, fmt.Errorf("require WORKER_STALE_AFTER > WORKER_HARD_TIMEOUT > MOCK_WORK_DURATION")
	}
	if cfg.QueueRedisKey == "" || cfg.StreamGroup == "" || cfg.StreamConsumer == "" {
		return nil, fmt.Errorf("stream key, group, and consumer must be non-empty")
	}

	if cidrStr := getEnv("TRUSTED_PROXY_CIDR", ""); cidrStr != "" {
		for _, s := range strings.Split(cidrStr, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			_, network, err := net.ParseCIDR(s)
			if err != nil {
				return nil, fmt.Errorf("invalid TRUSTED_PROXY_CIDR %q: %w", s, err)
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, network)
		}
	}

	return cfg, nil
}

func (c *Config) IsProduction() bool {
	return c.AppEnv == "production"
}

func (c *Config) IsTrustedProxy(ip net.IP) bool {
	if !c.UseTrustedProxyHeaders {
		return false
	}
	for _, network := range c.TrustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvInt64(key string, fallback int64) int64 {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	val := os.Getenv(key)
	if val == "" {
		return fallback
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return fallback
	}
	return d
}
