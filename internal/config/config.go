package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Token          string        `json:"-"`
	DeviceID       string        `json:"deviceId"`
	InstallID      string        `json:"-"`
	APIKey         string        `json:"-"`
	UpstreamAPIKey string        `json:"-"`
	BaseURL        string        `json:"baseURL,omitempty"`
	AppVersion     string        `json:"appVersion,omitempty"`
	UserAgent      string        `json:"userAgent,omitempty"`
	Listen         string        `json:"listen,omitempty"`
	Models         []string      `json:"models,omitempty"`
	Timeout        time.Duration `json:"timeout,omitempty"`
	LogLevel       string        `json:"logLevel,omitempty"`
	LogFormat      string        `json:"logFormat,omitempty"`
	RetryCount     int           `json:"retryCount,omitempty"`
}

// fileConfig mirrors Config but allows deserializing sensitive fields from file.
type fileConfig struct {
	Token          string   `json:"token"`
	DeviceID       string   `json:"deviceId"`
	InstallID      string   `json:"installId"`
	APIKey         string   `json:"apiKey"`
	UpstreamAPIKey string   `json:"upstreamApiKey"`
	BaseURL        string   `json:"baseURL"`
	AppVersion     string   `json:"appVersion"`
	UserAgent      string   `json:"userAgent"`
	Listen         string   `json:"listen"`
	Models         []string `json:"models"`
	Timeout        string   `json:"timeout"`
	LogLevel       string   `json:"logLevel"`
	LogFormat      string   `json:"logFormat"`
	RetryCount     int      `json:"retryCount"`
}

func Load() Config {
	// Phase 1: defaults + config file
	c := Config{
		UpstreamAPIKey: "sk-qvp4m6h9g20uM6PgYP7E",
		BaseURL:        "https://agent.teleai.com.cn",
		AppVersion:     "2.0.0",
		UserAgent:      "opencode/1.2.27 ai-sdk/provider-utils/3.0.20 runtime/bun/1.3.10",
		Listen:         ":10000",
		Timeout:        120 * time.Second,
		LogLevel:       "info",
		LogFormat:      "text",
		RetryCount:     0,
	}

	configPath := getEnv("TELEAGENT_CONFIG", "config.json")
	if data, err := os.ReadFile(configPath); err == nil {
		var fc fileConfig
		if err := json.Unmarshal(data, &fc); err != nil {
			slog.Error("config file parse error, ignoring file",
				slog.String("path", configPath),
				slog.String("error", err.Error()),
			)
		} else {
			slog.Info("loaded config file", slog.String("path", configPath))
			if fc.Token != "" {
				c.Token = fc.Token
			}
			if fc.DeviceID != "" {
				c.DeviceID = fc.DeviceID
			}
			if fc.InstallID != "" {
				c.InstallID = fc.InstallID
			}
			if fc.APIKey != "" {
				c.APIKey = fc.APIKey
			}
			if fc.UpstreamAPIKey != "" {
				c.UpstreamAPIKey = fc.UpstreamAPIKey
			}
			if fc.BaseURL != "" {
				c.BaseURL = fc.BaseURL
			}
			if fc.AppVersion != "" {
				c.AppVersion = fc.AppVersion
			}
			if fc.UserAgent != "" {
				c.UserAgent = fc.UserAgent
			}
			if fc.Listen != "" {
				c.Listen = fc.Listen
			}
			if len(fc.Models) > 0 {
				c.Models = fc.Models
			}
			if fc.Timeout != "" {
				if d, err := time.ParseDuration(fc.Timeout); err == nil {
					c.Timeout = d
				}
			}
			if fc.LogLevel != "" {
				c.LogLevel = fc.LogLevel
			}
			if fc.LogFormat != "" {
				c.LogFormat = fc.LogFormat
			}
			if fc.RetryCount > 0 {
				c.RetryCount = fc.RetryCount
			}
		}
	}

	// Phase 2: environment variables override file values (12-factor compliance)
	if v, ok := os.LookupEnv("TELEAGENT_TOKEN"); ok {
		c.Token = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_DEVICE_ID"); ok {
		c.DeviceID = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_INSTALL_ID"); ok {
		c.InstallID = v
	}
	if v, ok := os.LookupEnv("API_KEY"); ok {
		c.APIKey = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_UPSTREAM_KEY"); ok {
		c.UpstreamAPIKey = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_BASE_URL"); ok {
		c.BaseURL = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_APP_VERSION"); ok {
		c.AppVersion = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_USER_AGENT"); ok {
		c.UserAgent = v
	}
	if v, ok := os.LookupEnv("TELEAGENT2API_LISTEN"); ok {
		c.Listen = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_TIMEOUT"); ok {
		if d, err := time.ParseDuration(v); err == nil {
			c.Timeout = d
		}
	}
	if v, ok := os.LookupEnv("TELEAGENT_LOG_LEVEL"); ok {
		c.LogLevel = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_LOG_FORMAT"); ok {
		c.LogFormat = v
	}
	if v, ok := os.LookupEnv("TELEAGENT_RETRY_COUNT"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.RetryCount = n
		}
	}

	if len(c.Models) == 0 {
		modelsStr := getEnv("TELEAGENT_MODELS", "chat-lite,chat-pro,chat-flash")
		for _, m := range strings.Split(modelsStr, ",") {
			if m = strings.TrimSpace(m); m != "" {
				c.Models = append(c.Models, m)
			}
		}
	}

	return c
}

// SafeSummary returns a human-readable summary with sensitive fields redacted.
func (c Config) SafeSummary() string {
	tokenHint := ""
	if c.Token != "" {
		tokenHint = fmt.Sprintf("%s…%s", c.Token[:min(4, len(c.Token))], c.Token[max(0, len(c.Token)-4):])
	}
	apiKeyHint := ""
	if c.APIKey != "" {
		apiKeyHint = fmt.Sprintf("%s…", c.APIKey[:min(4, len(c.APIKey))])
	}
	return fmt.Sprintf("listen=%s upstream=%s token=%s apiKey=%s models=%v timeout=%s logLevel=%s retryCount=%d",
		c.Listen, c.BaseURL, tokenHint, apiKeyHint, c.Models, c.Timeout, c.LogLevel, c.RetryCount,
	)
}

// SlogLevel converts the string log level to slog.Level.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func getEnv(key, fallback string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return fallback
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}