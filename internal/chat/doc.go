// Package chat gives the single operator a conversational control surface
// in Slack and Discord on top of the engine: slash commands, confirmed
// operator actions, proactive attention alerts and a scheduled digest.
// Both transports are outbound-only (Slack Socket Mode, Discord Gateway),
// so the private listener model is unchanged. See docs/chat.md.
package chat
