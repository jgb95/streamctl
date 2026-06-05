package nostrpub

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip19"
)

const LiveActivityKind = 30311

type KeyInfo struct {
	Secret string
	PubKey string
	Npub   string
}

type PublishOptions struct {
	KeyFile      string
	Relays       []string
	Status       string
	DTag         string
	Title        string
	Summary      string
	StreamingURL string
	Starts       int64
}

type PublishResult struct {
	RelayURL string
	Error    error
}

func DecodeKey(input string) (*KeyInfo, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil, fmt.Errorf("nsec required")
	}
	prefix, value, err := nip19.Decode(input)
	if err != nil {
		return nil, err
	}
	if prefix != "nsec" {
		return nil, fmt.Errorf("expected nsec key, got %s", prefix)
	}
	secret, ok := value.(string)
	if !ok || secret == "" {
		return nil, fmt.Errorf("decoded nsec did not contain a private key")
	}
	pubkey, err := nostr.GetPublicKey(secret)
	if err != nil {
		return nil, err
	}
	npub, err := nip19.EncodePublicKey(pubkey)
	if err != nil {
		return nil, err
	}
	return &KeyInfo{Secret: secret, PubKey: pubkey, Npub: npub}, nil
}

func WriteSecretFile(path, secret string) error {
	if err := os.WriteFile(path, []byte(strings.TrimSpace(secret)+"\n"), 0400); err != nil {
		return err
	}
	return os.Chmod(path, 0400)
}

func PublishLiveEvent(ctx context.Context, opts PublishOptions) ([]PublishResult, error) {
	if opts.KeyFile == "" {
		return nil, fmt.Errorf("key file required")
	}
	secretBytes, err := os.ReadFile(opts.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("reading key file: %w", err)
	}
	secret := strings.TrimSpace(string(secretBytes))
	if secret == "" {
		return nil, fmt.Errorf("key file is empty")
	}
	if _, err := nostr.GetPublicKey(secret); err != nil {
		return nil, fmt.Errorf("invalid secret key: %w", err)
	}
	relays := cleanRelays(opts.Relays)
	if len(relays) == 0 {
		return nil, fmt.Errorf("at least one relay required")
	}
	if opts.DTag == "" {
		return nil, fmt.Errorf("d tag required")
	}
	status := normalizeStatus(opts.Status)
	if status == "" {
		return nil, fmt.Errorf("status must be planned, live, or ended")
	}
	if opts.StreamingURL == "" {
		return nil, fmt.Errorf("streaming URL required")
	}

	pubkey, err := nostr.GetPublicKey(secret)
	if err != nil {
		return nil, err
	}
	tags := nostr.Tags{
		{"d", opts.DTag},
		{"title", firstNonEmpty(opts.Title, opts.DTag)},
		{"status", status},
		{"streaming", opts.StreamingURL},
		{"t", "streamctl"},
	}
	if strings.TrimSpace(opts.Summary) != "" {
		tags = append(tags, nostr.Tag{"summary", strings.TrimSpace(opts.Summary)})
	}
	if opts.Starts > 0 {
		tags = append(tags, nostr.Tag{"starts", strconv.FormatInt(opts.Starts, 10)})
	}
	evt := nostr.Event{
		PubKey:    pubkey,
		CreatedAt: nostr.Now(),
		Kind:      LiveActivityKind,
		Tags:      tags,
		Content:   strings.TrimSpace(opts.Summary),
	}
	if err := evt.Sign(secret); err != nil {
		return nil, fmt.Errorf("signing event: %w", err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	pool := nostr.NewSimplePool(timeoutCtx)
	ch := pool.PublishMany(timeoutCtx, relays, evt)

	var results []PublishResult
	var successes int
	for range relays {
		res := <-ch
		results = append(results, PublishResult{RelayURL: res.RelayURL, Error: res.Error})
		if res.Error == nil {
			successes++
		}
	}
	if successes == 0 {
		return results, fmt.Errorf("publish failed on all relays")
	}
	return results, nil
}

func cleanRelays(relays []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, relay := range relays {
		relay = strings.TrimSpace(relay)
		if relay == "" || seen[relay] {
			continue
		}
		seen[relay] = true
		out = append(out, relay)
	}
	return out
}

func normalizeStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "planned", "live", "ended":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return ""
	}
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
