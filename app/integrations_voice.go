//go:build agent

package app

import (
	"os"

	"github.com/znasllc-io/memql/integrations/voice"
)

func (a *App) wireAskVoice() {
	config := voice.Config{URL: os.Getenv("MEMQL_LIVEKIT_URL"), PublicURL: os.Getenv("MEMQL_LIVEKIT_PUBLIC_URL"), APIKey: os.Getenv("MEMQL_LIVEKIT_API_KEY"), APISecret: os.Getenv("MEMQL_LIVEKIT_API_SECRET")}
	if config.URL == "" && config.PublicURL == "" && config.APIKey == "" && config.APISecret == "" {
		return
	}
	transport, err := voice.New(config)
	if err != nil {
		a.fatal("Ask voice configuration is incomplete", "error", err)
		return
	}
	a.engine.SetVoiceTransport(transport)
}
