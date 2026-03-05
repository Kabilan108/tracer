package cmd

import (
	"log/slog"

	"github.com/tracer-ai/tracer-cli/pkg/cloud"
	"github.com/tracer-ai/tracer-cli/pkg/log"
)

// CheckAndWarnAuthentication warns the user if cloud sync is enabled but authentication
// is missing or has failed. Uses log.IsSilent() to respect silent mode.
func CheckAndWarnAuthentication(noCloudSync bool) {
	if !noCloudSync && !cloud.IsAuthenticated() && !log.IsSilent() {
		// Check if this was due to a 401 authentication failure
		if cloud.HadAuthFailure() {
			// Show the specific message for auth failures with orange warning and emoji
			slog.Warn("Cloud sync authentication failed (401)")
			log.UserWarn("⚠️ Unable to authenticate with Tracer Cloud. This could be due to revoked or expired credentials, or network/server issues.\n")
			log.UserMessage("ℹ️ If this persists, run `tracer logout` then `tracer login` to reset your Tracer Cloud authentication.\n")
		} else {
			// Regular "not authenticated" message
			msg := "⚠️ Cloud sync not available. You're not authenticated."
			slog.Warn(msg)
			log.UserWarn("%s\n", msg)
			log.UserMessage("ℹ️ Use `tracer login` to authenticate, or `--no-cloud-sync` to skip this warning.\n")
		}
	}
}
