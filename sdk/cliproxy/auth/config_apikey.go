package auth

import "strings"

// IsConfigAPIKeyAuth reports whether the auth entry is synthesized from config *-api-key lists.
func IsConfigAPIKeyAuth(auth *Auth) bool {
	if auth == nil {
		return false
	}
	if auth.AuthKind() != AuthKindAPIKey {
		return false
	}
	return auth.AuthSourceKind() == AuthSourceConfig
}

// isConfigOpenAICompatBlockAuth reports whether the auth entry was synthesized
// from an openai-compatibility config block. Such blocks carry the
// "compat_name" attribute and a "config:<name>" source, but a block with no
// api-key entries is not classified as an API-key auth. Persisting these
// entries as standalone credential files would re-load them without their
// config origin (no api-key, no compat_name), which silently disables
// alias-to-upstream model translation for the block. They must never be
// persisted.
func isConfigOpenAICompatBlockAuth(auth *Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	if strings.TrimSpace(auth.Attributes["compat_name"]) == "" {
		return false
	}
	return auth.AuthSourceKind() == AuthSourceConfig
}
