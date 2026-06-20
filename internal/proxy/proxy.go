package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"teleagent2api/internal/config"
)

const (
	signPrefix       = "superagent-auth-v1"
	signVersion      = "v1"
	upstreamChatPath = "/superCowork/sapi/api/v1/chat/completions"
)

type UpstreamProxy struct {
	cfg     config.Config
	baseURL *url.URL
}

func NewUpstreamProxy(cfg config.Config) (*UpstreamProxy, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	return &UpstreamProxy{
		cfg:     cfg,
		baseURL: u,
	}, nil
}

func (p *UpstreamProxy) BuildRequest(r *http.Request, body []byte, cred config.Credential) (*http.Request, error) {
	timestamp := fmt.Sprintf("%d", time.Now().Unix())
	nonce, err := newUUID()
	if err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}

	bodySHA := sha256Hex(body)
	derivedKey, err := buildDerivedKey(cred.Token, cred.InstallID, timestamp, nonce)
	if err != nil {
		return nil, fmt.Errorf("build derived key: %w", err)
	}

	pathWithQuery, err := mapIncomingPath(r.URL)
	if err != nil {
		return nil, err
	}

	requestString := buildRequestString(http.MethodPost, pathWithQuery, timestamp, nonce, p.cfg.AppVersion, bodySHA)
	signature := hmacSHA256Hex(derivedKey, requestString)

	messageID := firstNonEmpty(r.Header.Get("x-message-id"), "msg_"+mustHexToken(16))
	sessionID := firstNonEmpty(r.Header.Get("x-session-id"), "ses_"+mustHexToken(16))

	endpoint := *p.baseURL
	endpoint.Path, endpoint.RawQuery = splitPathAndQuery(joinOriginPath(p.baseURL.Path, pathWithQuery))

	slog.DebugContext(r.Context(), "building upstream request",
		slog.String("upstream", endpoint.String()),
		slog.String("nonce", nonce),
	)

	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint.String(), strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("create upstream request: %w", err)
	}

	upstreamReq.Header.Set("Authorization", "Bearer "+p.cfg.UpstreamAPIKey)
	upstreamReq.Header.Set("Content-Type", firstNonEmpty(r.Header.Get("Content-Type"), "application/json"))
	upstreamReq.Header.Set("User-Agent", p.cfg.UserAgent)
	upstreamReq.Header.Set("X-App-Version", p.cfg.AppVersion)
	upstreamReq.Header.Set("X-SuperAgent-Device-Id", cred.DeviceID)
	upstreamReq.Header.Set("X-SuperAgent-Install-Id", cred.InstallID)
	upstreamReq.Header.Set("X-SuperAgent-Nonce", nonce)
	upstreamReq.Header.Set("X-SuperAgent-Sign-Version", signVersion)
	upstreamReq.Header.Set("X-SuperAgent-Signature", signature)
	upstreamReq.Header.Set("X-SuperAgent-Timestamp", timestamp)
	upstreamReq.Header.Set("X-Token", cred.Token)
	upstreamReq.Header.Set("x-message-id", messageID)
	upstreamReq.Header.Set("x-session-id", sessionID)
	if lastEventID := strings.TrimSpace(r.Header.Get("Last-Event-ID")); lastEventID != "" {
		upstreamReq.Header.Set("Last-Event-ID", lastEventID)
	}
	upstreamReq.Header.Set("Accept", firstNonEmpty(r.Header.Get("Accept"), "*/*"))
	upstreamReq.Header.Set("Accept-Encoding", "identity")
	upstreamReq.Header.Set("Connection", "close")

	return upstreamReq, nil
}

func mapIncomingPath(u *url.URL) (string, error) {
	switch u.Path {
	case "/v1/chat/completions", "/chat/completions", upstreamChatPath:
		if u.RawQuery == "" {
			return upstreamChatPath, nil
		}
		return upstreamChatPath + "?" + u.RawQuery, nil
	default:
		return "", fmt.Errorf("unsupported path: %s", u.Path)
	}
}

func buildDerivedKey(token, installID, timestamp, nonce string) (string, error) {
	third, err := jwtThirdSegment(token)
	if err != nil {
		return "", err
	}
	basic := fmt.Sprintf("%s/%s/%s/%s/%s", signPrefix, token, installID, timestamp, nonce)
	return hmacSHA256Hex(third, basic), nil
}

func buildRequestString(method, pathWithQuery, timestamp, nonce, appVersion, bodySHA string) string {
	return strings.Join([]string{
		signPrefix,
		strings.ToUpper(method),
		pathWithQuery,
		timestamp,
		nonce,
		appVersion,
		bodySHA,
	}, "\n")
}

func jwtThirdSegment(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || strings.TrimSpace(parts[2]) == "" {
		return "", errors.New("invalid token format")
	}
	return parts[2], nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256Hex(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func joinOriginPath(basePath, requestPath string) string {
	path, query := splitPathAndQuery(requestPath)
	base := strings.TrimRight(basePath, "/")
	joined := "/" + strings.TrimLeft(path, "/")
	if base != "" {
		joined = base + joined
	}
	if query == "" {
		return joined
	}
	return joined + "?" + query
}

func splitPathAndQuery(pathWithQuery string) (string, string) {
	parts := strings.SplitN(pathWithQuery, "?", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func newUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		buf[0:4],
		buf[4:6],
		buf[6:8],
		buf[8:10],
		buf[10:16],
	), nil
}

// mustHexToken generates a cryptographically random hex token.
// If the system PRNG fails it panics — this is preferable to a predictable fallback.
func mustHexToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand unavailable: %v", err))
	}
	return hex.EncodeToString(buf)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
