package chat

// Embed the IANA time-zone database so OCTOMUS_CHAT_TIMEZONE works in
// minimal container images that do not install system tzdata.
import _ "time/tzdata"
