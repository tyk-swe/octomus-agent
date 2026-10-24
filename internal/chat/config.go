package chat

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/tyk-swe/octomus-agent/internal/config"
	"github.com/tyk-swe/octomus-agent/internal/store"
)

// Provider names one chat platform.
type Provider string

const (
	SlackProvider   Provider = "slack"
	DiscordProvider Provider = "discord"
)

// ProviderConfig is one platform's host-level settings: credentials, the
// operator allowlist, channel->repository bindings and the proactive-alert
// channel. It is deployment configuration, never dashboard-editable.
type ProviderConfig struct {
	Provider     Provider
	AppToken     string // Slack app-level token (Socket Mode); unused for Discord
	BotToken     string
	AppID        string // Discord application ID; unused for Slack
	GuildID      string // Discord guild for fast command registration; empty means global
	Operators    map[string]bool
	Bindings     map[string]string // channel ID -> OWNER/REPOSITORY
	AlertChannel string
}

// Config is the whole chat surface. A provider is enabled when its tokens
// are present; with neither present chat is fully disabled.
type Config struct {
	Slack      *ProviderConfig
	Discord    *ProviderConfig
	DigestTime string // "HH:MM" in Timezone; empty disables the scheduled digest
	Timezone   *time.Location
}

func (c Config) Enabled() bool { return c.Slack != nil || c.Discord != nil }

// FromEnv reads chat configuration from the deployment environment.
func FromEnv(env func(string) (string, bool)) (Config, error) {
	get := func(name string) string {
		v, _ := env(name)
		return strings.TrimSpace(v)
	}
	var cfg Config
	cfg.Timezone = time.UTC
	if tz := get(config.ChatTimezoneEnv); tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return Config{}, fmt.Errorf("%s must name an IANA time zone: %w", config.ChatTimezoneEnv, err)
		}
		cfg.Timezone = loc
	}
	cfg.DigestTime = get(config.ChatDigestTimeEnv)
	if cfg.DigestTime != "" {
		if _, err := time.Parse("15:04", cfg.DigestTime); err != nil {
			return Config{}, fmt.Errorf("%s must be HH:MM (24-hour)", config.ChatDigestTimeEnv)
		}
	}
	slackApp, slackBot := get(config.SlackAppTokenEnv), get(config.SlackBotTokenEnv)
	if slackApp != "" || slackBot != "" {
		if slackApp == "" || slackBot == "" {
			return Config{}, fmt.Errorf("Set both %s and %s to enable Slack chat", config.SlackAppTokenEnv, config.SlackBotTokenEnv)
		}
		pc, err := providerFromEnv(env, SlackProvider, config.SlackOperatorsEnv, config.SlackChannelsEnv)
		if err != nil {
			return Config{}, err
		}
		pc.AppToken, pc.BotToken = slackApp, slackBot
		pc.AlertChannel = get(config.SlackAlertChannelEnv)
		cfg.Slack = pc
	}
	discordBot, discordApp := get(config.DiscordBotTokenEnv), get(config.DiscordAppIDEnv)
	if discordBot != "" || discordApp != "" {
		if discordBot == "" || discordApp == "" {
			return Config{}, fmt.Errorf("Set both %s and %s to enable Discord chat", config.DiscordBotTokenEnv, config.DiscordAppIDEnv)
		}
		pc, err := providerFromEnv(env, DiscordProvider, config.DiscordOperatorsEnv, config.DiscordChannelsEnv)
		if err != nil {
			return Config{}, err
		}
		pc.BotToken, pc.AppID = discordBot, discordApp
		pc.GuildID = get(config.DiscordGuildIDEnv)
		pc.AlertChannel = get(config.DiscordAlertChannelEnv)
		cfg.Discord = pc
	}
	return cfg, nil
}

func providerFromEnv(env func(string) (string, bool), provider Provider, operatorsEnv, channelsEnv string) (*ProviderConfig, error) {
	get := func(name string) string {
		v, _ := env(name)
		return strings.TrimSpace(v)
	}
	pc := &ProviderConfig{Provider: provider, Operators: map[string]bool{}, Bindings: map[string]string{}}
	for _, id := range strings.Split(get(operatorsEnv), ",") {
		if id = strings.TrimSpace(id); id != "" {
			pc.Operators[id] = true
		}
	}
	if len(pc.Operators) == 0 {
		return nil, fmt.Errorf("Set %s to your comma-separated %s user IDs: chat commands are operator-only", operatorsEnv, provider)
	}
	for _, pair := range strings.Split(get(channelsEnv), ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		channel, repo, ok := strings.Cut(pair, "=")
		if !ok || strings.TrimSpace(channel) == "" || !config.ValidGitHubRepo(strings.TrimSpace(repo)) {
			return nil, fmt.Errorf("%s entries must look like CHANNEL_ID=OWNER/REPOSITORY", channelsEnv)
		}
		pc.Bindings[strings.TrimSpace(channel)] = strings.TrimSpace(repo)
	}
	return pc, nil
}

// provider returns the enabled configuration for p, or nil.
func (c Config) provider(p Provider) *ProviderConfig {
	switch p {
	case SlackProvider:
		return c.Slack
	case DiscordProvider:
		return c.Discord
	}
	return nil
}

// destinations lists the proactive-alert channels as outbox destinations.
func (c Config) destinations() []store.ChatDestination {
	var out []store.ChatDestination
	for _, pc := range []*ProviderConfig{c.Slack, c.Discord} {
		if pc != nil && pc.AlertChannel != "" {
			out = append(out, store.ChatDestination{
				ID:   DestinationID(pc.Provider, pc.AlertChannel),
				Kind: string(pc.Provider), Target: pc.AlertChannel,
			})
		}
	}
	return out
}

// DestinationID derives the stable outbox identity of a chat channel, in
// the same spirit as the webhook's URL hash.
func DestinationID(provider Provider, channel string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(string(provider)+":"+channel)))
}
