// 一個最小可跑的 OpAMP 控制面 server。
//
// 它做的事：
//   - 在 :4320 的 /v1/opamp 接受 OpAMP 連線（OpAMP Bridge 會連進來）
//   - 每收到一個 AgentToServer 訊息，就把 agent 身分、health、以及它回報的
//     「目前生效的 collector 設定（EffectiveConfig）」印到 log，並快取起來
//   - 在 :8080 的 /upgrade 接受平台工程團隊的 HTTP 請求，登記「某個 collector
//     要升級到哪個 image」；下次該 collector 回報時，就把新 image 透過
//     RemoteConfig 下推回去，交給 Operator 管理的 bridge 去 kubectl update CR
//
// 這示範了「一個 Go server 如何看見並管理 Operator 跑起來的 collector」：
// OpAMP Bridge 把 Operator 建立的每個 OpenTelemetryCollector 當成一個 agent 回報上來，
// server 因此能集中觀察生效設定，也能下推新設定（含版本）回去。
//
// 這是教學用的最小實作，不是 production server：沒有 auth、沒有持久化、
// pending upgrade 只存在記憶體裡。
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
	"sigs.k8s.io/yaml"
)

// stdLogger 把 opamp-go 要求的 Logger 介面接到標準 log。
type stdLogger struct{}

func (stdLogger) Debugf(_ context.Context, format string, v ...any) {
	log.Printf("[debug] "+format, v...)
}
func (stdLogger) Errorf(_ context.Context, format string, v ...any) {
	log.Printf("[error] "+format, v...)
}

// state 是整個 server 唯一的共享狀態：
//   - effectiveConfigs：每個 collector（key = "namespace/name"，跟 bridge 的
//     KubeResourceKey 格式一致）最後一次回報的完整 CR YAML
//   - pendingImage：平台工程團隊透過 /upgrade 登記、還沒送出去的目標 image
type state struct {
	mu               sync.Mutex
	effectiveConfigs map[string][]byte
	pendingImage     map[string]string
}

func newState() *state {
	return &state{
		effectiveConfigs: map[string][]byte{},
		pendingImage:     map[string]string{},
	}
}

func main() {
	listen := os.Getenv("LISTEN_ENDPOINT")
	if listen == "" {
		listen = ":4320"
	}
	adminListen := os.Getenv("ADMIN_LISTEN_ENDPOINT")
	if adminListen == "" {
		adminListen = ":8080"
	}

	st := newState()
	opampSrv := server.New(stdLogger{})

	callbacks := types.Callbacks{
		OnConnecting: func(_ *http.Request) types.ConnectionResponse {
			return types.ConnectionResponse{
				Accept: true,
				ConnectionCallbacks: types.ConnectionCallbacks{
					OnConnected: func(_ context.Context, _ types.Connection) {
						log.Printf("OpAMP agent connected")
					},
					OnMessage: st.onMessage,
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

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/upgrade", st.handleUpgrade)
	adminSrv := &http.Server{Addr: adminListen, Handler: adminMux}
	go func() {
		log.Printf("admin HTTP listening on %s (POST /upgrade)", adminListen)
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("admin server failed: %v", err)
		}
	}()

	// 等待中止訊號
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	_ = adminSrv.Shutdown(context.Background())
	_ = opampSrv.Stop(context.Background())
}

// upgradeRequest 是 /upgrade 的請求 body。
// Key 對應 bridge 回報的 config key，格式是 "namespace/name"（例如 "otel-lab/gateway"）。
type upgradeRequest struct {
	Key   string `json:"key"`
	Image string `json:"image"`
}

// handleUpgrade 登記一個「下次該 collector 回報時要下推的目標 image」。
// 它本身不會主動連線去 push——OpAMP 是 agent 先連上來、server 才能回應，所以
// 這裡只是把意圖記下來，等 bridge 下一次心跳帶著 EffectiveConfig 進來時，
// onMessage 才會真的組出 RemoteConfig 回應。
func (s *state) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "only POST is supported", http.StatusMethodNotAllowed)
		return
	}
	var req upgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" || req.Image == "" {
		http.Error(w, "both key and image are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	_, known := s.effectiveConfigs[req.Key]
	s.pendingImage[req.Key] = req.Image
	s.mu.Unlock()

	if !known {
		log.Printf("upgrade registered for %q -> %q (尚未收到過這個 collector 的 report，會等它下次連線)", req.Key, req.Image)
	} else {
		log.Printf("upgrade registered for %q -> %q (下次收到它的 report 時會下推)", req.Key, req.Image)
	}
	w.WriteHeader(http.StatusAccepted)
}

// onMessage 在每次收到 agent 的回報時被呼叫。
func (s *state) onMessage(_ context.Context, _ types.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
	if desc := msg.GetAgentDescription(); desc != nil {
		log.Printf("---- agent report (instanceUid=%x) ----", msg.GetInstanceUid())
		for _, kv := range desc.GetIdentifyingAttributes() {
			log.Printf("  identify: %s = %s", kv.GetKey(), kv.GetValue().GetStringValue())
		}
	}
	if h := msg.GetHealth(); h != nil {
		log.Printf("  health: healthy=%v status=%q", h.GetHealthy(), h.GetStatus())
	}

	resp := &protobufs.ServerToAgent{
		InstanceUid: msg.GetInstanceUid(),
		// 回應時要宣告 server 的 capabilities。依 OpAMP spec，agent 只有在 server
		// 宣告 AcceptsEffectiveConfig 時才會回報 EffectiveConfig，所以這裡一定要帶上；
		// OffersRemoteConfig 則是讓 agent 知道 server 可能會下推 RemoteConfig。
		Capabilities: uint64(
			protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
				protobufs.ServerCapabilities_ServerCapabilities_AcceptsEffectiveConfig |
				protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig,
		),
	}

	// agent 回報「目前生效的 collector 設定」——這就是 server 能看見 Operator 管的
	// collector 設定的地方。bridge 把整個 OpenTelemetryCollector CR marshal 成 YAML
	// 當作 body，key 是 "namespace/name"（見 cmd/operator-opamp-bridge 的
	// CRDInstance.GetConfigMap / KubeResourceKey）。
	ec := msg.GetEffectiveConfig()
	if ec == nil || ec.GetConfigMap() == nil {
		return resp
	}

	remoteFiles := map[string]*protobufs.AgentConfigFile{}
	for key, file := range ec.GetConfigMap().GetConfigMap() {
		log.Printf("  effective-config[%s]:\n%s", key, string(file.GetBody()))

		s.mu.Lock()
		s.effectiveConfigs[key] = file.GetBody()
		targetImage, hasPending := s.pendingImage[key]
		s.mu.Unlock()

		if !hasPending {
			continue
		}

		patched, err := patchImage(file.GetBody(), targetImage)
		if err != nil {
			log.Printf("  !! failed to patch image for %s: %v (upgrade 保留，下次重試)", key, err)
			continue
		}

		remoteFiles[key] = &protobufs.AgentConfigFile{
			Body:        patched,
			ContentType: "yaml",
		}
		log.Printf("  >> pushing image upgrade for %s -> %s", key, targetImage)

		s.mu.Lock()
		delete(s.pendingImage, key)
		s.mu.Unlock()
	}

	if len(remoteFiles) == 0 {
		return resp
	}

	cfgMap := &protobufs.AgentConfigMap{ConfigMap: remoteFiles}
	resp.RemoteConfig = &protobufs.AgentRemoteConfig{
		Config:     cfgMap,
		ConfigHash: hashConfigMap(cfgMap),
	}
	return resp
}

// patchImage 把一份 OpenTelemetryCollector CR 的 YAML 內容裡的 spec.image 換成
// newImage，其餘欄位原封不動地保留（bridge 的 Client.Apply 會把整份 spec 拿去
// 取代舊的 CR spec，所以這裡必須回傳「完整」的 CR，不能只丟一個 image 欄位）。
//
// 用 map[string]interface{} 而不是 import operator 的 v1beta1 型別，是為了讓這個
// 教學用的 opamp-server 保持獨立、輕量，不需要拉進整個 operator 的 API 模組。
func patchImage(body []byte, newImage string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}

	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		doc["spec"] = spec
	}
	spec["image"] = newImage

	return yaml.Marshal(doc)
}

func hashConfigMap(cfgMap *protobufs.AgentConfigMap) []byte {
	h := sha256.New()
	for key, file := range cfgMap.GetConfigMap() {
		h.Write([]byte(key))
		h.Write(file.GetBody())
	}
	return h.Sum(nil)
}
