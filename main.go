// Command devin-codeium runs a CLIProxyAPI server that exposes the Codeium /
// Devin (Windsurf) "GetChatMessage" backend as an OpenAI-compatible provider.
//
// Log in by dropping an auth file under the configured auth dir (see README):
//
//	{
//	  "id": "my-devin",
//	  "provider": "codeium",
//	  "attributes": {
//	    "session_token": "devin-session-token$<jwt>",
//	    "team_id": "devin-team$account-...",
//	    "user_id": "user-...",
//	    "device_id": "<uuid>",
//	    "hw_hash": "<hex>", "hash27": "<hex>", "hex31": "<hex>"
//	  }
//	}
//
// Then request any listed model (swe-1-7, claude-sonnet-5, gpt-5.6-terra, ...)
// through the standard /v1/chat/completions endpoint.
package main

import (
	"context"
	"errors"
	"strings"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktr "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const (
	fOpenAI        = sdktr.Format("openai.chat")
	fCodeium       = sdktr.Format("codeium.chat")
	configYML      = "config.yaml"
	modelsClientID = "codeium-provider-models"
)

// init registers identity translators. The executor already speaks OpenAI in and
// out, so request/response bodies pass through unchanged.
func init() {
	sdktr.Register(fOpenAI, fCodeium,
		func(model string, raw []byte, stream bool) []byte { return raw },
		sdktr.ResponseTransform{
			Stream: func(ctx context.Context, model string, orig, tr, raw []byte, p *any) [][]byte {
				return [][]byte{raw}
			},
			NonStream: func(ctx context.Context, model string, orig, tr, raw []byte, p *any) []byte {
				return raw
			},
		},
	)
}

func hasCodeiumAuth(core *coreauth.Manager) bool {
	for _, a := range core.List() {
		if strings.EqualFold(a.Provider, providerKey) {
			return true
		}
	}
	return false
}

func main() {
	cfg, err := config.LoadConfig(configYML)
	if err != nil {
		panic(err)
	}

	tokenStore := sdkAuth.GetTokenStore()
	if setter, ok := tokenStore.(interface{ SetBaseDir(string) }); ok {
		setter.SetBaseDir(cfg.AuthDir)
	}
	core := coreauth.NewManager(tokenStore, nil, nil)
	core.RegisterExecutor(codeiumExecutor{})

	hooks := cliproxy.Hooks{
		OnAfterStart: func(s *cliproxy.Service) {
			models := make([]*cliproxy.ModelInfo, 0, len(codeiumModels))
			for _, m := range codeiumModels {
				models = append(models, &cliproxy.ModelInfo{
					ID:          m.ID,
					Object:      "model",
					Type:        providerKey,
					DisplayName: m.Display,
				})
			}
			// The catalogue must be registered under each codeium auth's own id so
			// the scheduler considers that auth capable of serving the models
			// (ClientSupportsModel keys on auth id). The service clears an auth's
			// model list on (re)load when it can't resolve provider models itself,
			// so we (re)register periodically to keep the models available.
			go func() {
				for {
					if hasCodeiumAuth(core) {
						// The service installs an OpenAI-compat executor for unknown
						// providers on auth (re)load, clobbering ours; restore it.
						core.RegisterExecutor(codeiumExecutor{})
						for _, a := range core.List() {
							if strings.EqualFold(a.Provider, providerKey) {
								cliproxy.GlobalModelRegistry().RegisterClient(a.ID, providerKey, models)
							}
						}
					}
					time.Sleep(2 * time.Second)
				}
			}()
		},
	}

	svc, err := cliproxy.NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configYML).
		WithCoreAuthManager(core).
		WithHooks(hooks).
		Build()
	if err != nil {
		panic(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if errRun := svc.Run(ctx); errRun != nil && !errors.Is(errRun, context.Canceled) {
		panic(errRun)
	}
}
