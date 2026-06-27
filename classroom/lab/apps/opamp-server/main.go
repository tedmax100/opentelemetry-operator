// 一個最小可跑的 OpAMP 控制面 server。
//
// 它做的事：
//   - 在 :4320 的 /v1/opamp 接受 OpAMP 連線（OpAMP Bridge 會連進來）
//   - 每收到一個 AgentToServer 訊息，就把 agent 身分、health、以及它回報的
//     「目前生效的 collector 設定（EffectiveConfig）」印到 log
//
// 這示範了「一個 Go server 如何看見並（可進一步）管理 Operator 跑起來的 collector」：
// OpAMP Bridge 把 Operator 建立的每個 OpenTelemetryCollector 當成一個 agent 回報上來，
// server 因此能集中觀察、（延伸練習）下推 remote config。
//
// 這是教學用的最小實作，不是 production server。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
)

// stdLogger 把 opamp-go 要求的 Logger 介面接到標準 log。
type stdLogger struct{}

func (stdLogger) Debugf(_ context.Context, format string, v ...any) {
	log.Printf("[debug] "+format, v...)
}
func (stdLogger) Errorf(_ context.Context, format string, v ...any) {
	log.Printf("[error] "+format, v...)
}

func main() {
	listen := os.Getenv("LISTEN_ENDPOINT")
	if listen == "" {
		listen = ":4320"
	}

	opampSrv := server.New(stdLogger{})

	callbacks := types.Callbacks{
		OnConnecting: func(_ *http.Request) types.ConnectionResponse {
			return types.ConnectionResponse{
				Accept: true,
				ConnectionCallbacks: types.ConnectionCallbacks{
					OnConnected: func(_ context.Context, _ types.Connection) {
						log.Printf("OpAMP agent connected")
					},
					OnMessage: onMessage,
					OnConnectionClose: func(_ types.Connection) {
						log.Printf("OpAMP agent disconnected")
					},
				},
			}
		},
	}

	err := opampSrv.Start(server.StartSettings{
		Settings:       server.Settings{Callbacks: callbacks},
		ListenEndpoint: listen,
		ListenPath:     "/v1/opamp",
	})
	if err != nil {
		log.Fatalf("failed to start OpAMP server: %v", err)
	}
	log.Printf("OpAMP server listening on %s/v1/opamp", listen)

	// 等待中止訊號
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	_ = opampSrv.Stop(context.Background())
}

// onMessage 在每次收到 agent 的回報時被呼叫。
func onMessage(_ context.Context, _ types.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
	if desc := msg.GetAgentDescription(); desc != nil {
		log.Printf("---- agent report (instanceUid=%x) ----", msg.GetInstanceUid())
		for _, kv := range desc.GetIdentifyingAttributes() {
			log.Printf("  identify: %s = %s", kv.GetKey(), kv.GetValue().GetStringValue())
		}
	}
	if h := msg.GetHealth(); h != nil {
		log.Printf("  health: healthy=%v status=%q", h.GetHealthy(), h.GetStatus())
	}
	// agent 回報「目前生效的 collector 設定」——這就是 server 能看見 Operator 管的 collector 設定的地方
	if ec := msg.GetEffectiveConfig(); ec != nil && ec.GetConfigMap() != nil {
		for name, file := range ec.GetConfigMap().GetConfigMap() {
			log.Printf("  effective-config[%s]:\n%s", name, string(file.GetBody()))
		}
	}

	// 回應時要宣告 server 的 capabilities。依 OpAMP spec，agent 只有在 server
	// 宣告 AcceptsEffectiveConfig 時才會回報 EffectiveConfig，所以這裡一定要帶上。
	// 延伸練習：在回應塞 RemoteConfig 就能「下推」設定去改 Operator 管的 collector。
	return &protobufs.ServerToAgent{
		InstanceUid: msg.GetInstanceUid(),
		Capabilities: uint64(
			protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
				protobufs.ServerCapabilities_ServerCapabilities_AcceptsEffectiveConfig |
				protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig,
		),
	}
}
