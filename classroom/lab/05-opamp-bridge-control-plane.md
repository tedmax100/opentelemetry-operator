# Stage 5：用 OpAMP Bridge 當「管理 Operator 的 Go Server」控制面

> **本階段目標：** 回答你的問題「是不是有個 go server 能管理這 operator？」。答案是：有兩層 Go 程式——**Operator 本身**是一個 Go controller，而 **OpAMP Bridge** 是一個 Go agent，它把 Operator 管理的 collector 接到一個遠端 **OpAMP server**（控制面）上，讓你能集中觀察、甚至遠端下推設定。本階段會自己寫一個最小 OpAMP server 跑起來。

---

## 5.1 先把名詞理清楚：誰管誰？

```
┌────────────────────────────────────────────────────────────────────┐
│                         控制面 (control plane)                       │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │  OpAMP Server  (Go)   ← 本階段我們自己寫的最小 server          │  │
│  │  - 接收各 agent 回報的身分 / 健康 / 生效設定                    │  │
│  │  - 可下推 remote config                                       │  │
│  └───────────────────────────┬──────────────────────────────────┘  │
└──────────────────────────────┼─────────────────────────────────────┘
                               │ WebSocket (OpAMP 協議)
                               │ 回報 / 下推
┌──────────────────────────────┼─────────────────────────────────────┐
│  Kubernetes cluster          ▼                                      │
│  ┌──────────────────────────────────────────────────────────────┐  │
│  │  OpAMP Bridge (Go agent)  ← Operator 依 OpAMPBridge CR 跑起來   │  │
│  │  - 把「Operator 管理的每個 OpenTelemetryCollector」當 agent 回報 │  │
│  │  - 收到 remote config 時，套用回去                            │  │
│  └───────────────────────────┬──────────────────────────────────┘  │
│                              │ 讀取 / 影響                          │
│  ┌───────────────────────────▼──────────────────────────────────┐  │
│  │  OpenTelemetry Operator (Go controller)                       │  │
│  │  管理的 collector：gateway / agent / 各 app 的 sidecar         │  │
│  └──────────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────┘
```

三層 Go 程式各自的角色：

| 元件 | 是什麼 | 原始碼位置 | 角色 |
|---|---|---|---|
| **Operator** | controller-runtime 寫的 controller | `internal/controllers/`、`main.go` | 把 CR reconcile 成 k8s 資源（classroom 全書主角） |
| **OpAMP Bridge** | OpAMP client（agent 側） | `cmd/operator-opamp-bridge/` | 把 collector 接上控制面 |
| **OpAMP Server** | OpAMP server（控制面側） | 本 lab 的 `apps/opamp-server/`（你自己寫） | 集中觀察 / 下推設定 |

> 換句話說：「管理 operator 的 server」其實是「**透過 OpAMP Bridge 管理 operator 所建立的 collector**」。Server 不直接改 Operator 的程式，而是透過協議下推設定。

---

## 5.2 OpAMP 協議在做什麼

OpAMP（Open Agent Management Protocol）是 OTel 官方的「agent 遠端管理協議」。最常用的兩個方向：

```
Agent ──▶ Server   回報：我是誰(AgentDescription)、我健康嗎(Health)、
                        我「目前生效的設定」是什麼(EffectiveConfig)

Server ──▶ Agent   下推：這是你「應該」用的設定(RemoteConfig)
```

我們的最小 server（[`apps/opamp-server/main.go`](./apps/opamp-server/main.go)）實作了「接收回報並印出」這一半，下推留作練習。核心就是 `OnMessage` callback：

```go
// apps/opamp-server/main.go
func onMessage(_ context.Context, _ types.Connection, msg *protobufs.AgentToServer) *protobufs.ServerToAgent {
    if ec := msg.GetEffectiveConfig(); ec != nil && ec.GetConfigMap() != nil {
        for name, file := range ec.GetConfigMap().GetConfigMap() {
            log.Printf("  effective-config[%s]:\n%s", name, string(file.GetBody()))
        }
    }
    // ★ 關鍵：回應時必須宣告 server 的 capabilities。依 OpAMP spec，agent 只有在 server
    //   宣告 AcceptsEffectiveConfig 時，才會回報 EffectiveConfig。少了這個，bridge 連上、
    //   回報 health，但你永遠看不到 collector 的設定。
    return &protobufs.ServerToAgent{
        InstanceUid: msg.GetInstanceUid(),
        Capabilities: uint64(
            protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
                protobufs.ServerCapabilities_ServerCapabilities_AcceptsEffectiveConfig |
                protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig,
        ),
    }
}
```

> 這個 server 用 `github.com/open-telemetry/opamp-go/server`（跟本 repo 的 bridge 同一個 module，版本見 repo 的 `go.mod`：`opamp-go v0.23.0`）。

---

## 5.3 Build 並部署 OpAMP server

```bash
cd classroom/lab

docker build -t opamp-server:lab ./apps/opamp-server
k3d image import opamp-server:lab --cluster otel-lab
```

---

## 5.4 部署 server + OpAMPBridge CR

完整檔案：[`manifests/40-opampbridge.yaml`](./manifests/40-opampbridge.yaml)。重點：

```yaml
kind: OpAMPBridge
spec:
  endpoint: ws://opamp-server.otel-lab.svc.cluster.local:4320/v1/opamp   # ws://
  capabilities:
    ReportsEffectiveConfig: true   # 回報生效設定
    AcceptsRemoteConfig: true      # 接受遠端下推
    ReportsHealth: true
  componentsAllowed:               # 安全白名單：下推設定只允許這些 component
    receivers: [otlp]
    processors: [batch, memory_limiter, tail_sampling]
    exporters: [debug, otlp, load_balancing]
```

**兩個一定要做、否則 bridge 連得上卻看不到任何 collector 的前置條件：**

**(a) RBAC**：Operator 會幫 bridge 建好 ServiceAccount，但**不會**自動給它「讀 OpenTelemetryCollector CR」的權限。少了它，bridge log 會出現
`opentelemetrycollectors.opentelemetry.io is forbidden ... at the cluster scope`，且回報 `health=false`。`40-opampbridge.yaml` 內含對應的 ClusterRole / ClusterRoleBinding：

```yaml
kind: ClusterRole
metadata: { name: lab-bridge-opamp-bridge }
rules:
  - apiGroups: ["opentelemetry.io"]
    resources: ["opentelemetrycollectors"]
    verbs: ["get", "list", "watch", "update", "patch"]   # update/patch 是為了將來下推 remote config
```

**(b) collector 要「opt-in」**：bridge 只回報帶有 `opentelemetry.io/opamp-managed: "true"` label 的 collector（這是 bridge 原始碼 `ManagedLabelKey` 的過濾條件）。所以 Stage 2 的 `gateway` 與 `agent` CR 都加了這個 label：

```yaml
metadata:
  labels:
    opentelemetry.io/opamp-managed: "true"
```

```bash
kubectl apply -f manifests/40-opampbridge.yaml

# server 起來
kubectl -n otel-lab rollout status deployment/opamp-server
# bridge 由 Operator reconcile 出來（注意：bridge 也是 Operator 管的一個 CR）
kubectl -n otel-lab get opampbridge lab-bridge
kubectl -n otel-lab get deployment -l app.kubernetes.io/managed-by=opentelemetry-operator | grep -i bridge

# 確認被管理的 collector 有 opt-in label
kubectl -n otel-lab get opentelemetrycollector -L opentelemetry.io/opamp-managed
```

> `OpAMPBridge` 也是四大 CR 之一（classroom 第 2 章）。Operator 看到這個 CR，就 reconcile 出一個跑 `operator-opamp-bridge` 的 Deployment 與基本 RBAC，並把 `endpoint` / `capabilities` / `componentsAllowed` 帶進它的設定——但「讀 collector」的權限要自己補（上面的 (a)）。

---

## 5.5 驗證：控制面「看見」了 Operator 管理的 collector

先確認 bridge 端是健康的（`health=true` 代表它成功讀到了 collector；若 RBAC 沒設好這裡會是 `false`）：

```bash
kubectl -n otel-lab logs deployment/lab-bridge-opamp-bridge --tail=40 | grep -iE 'forbidden|error' | head
# 預期：沒有 forbidden（若有，回去檢查 5.4 的 (a) RBAC）
```

看我們的 OpAMP server log——它會印出 bridge 回報上來的 agent 與其生效設定：

```bash
kubectl -n otel-lab logs deployment/opamp-server --since=60s | grep -iE 'agent report|effective-config|health' 
```

實測會看到 server 印出每個被管理 collector 的「實際生效設定」，例如 `gateway` 與 `agent`（節錄真實輸出）：

```
---- agent report (instanceUid=019f0988...) ----
  effective-config[otel-lab/agent]:
      {"apiVersion":"opentelemetry.io/v1beta1","kind":"OpenTelemetryCollector",...
       "exporters":{"load_balancing":{"routing_key":"traceID","resolver":{"dns":
       {"hostname":"gateway-collector-headless...","port":"4317"}}}, ...}}
  effective-config[otel-lab/gateway]:
      {"apiVersion":"opentelemetry.io/v1beta1","kind":"OpenTelemetryCollector",...
       "processors":{"tail_sampling":{"decision_wait":"10s","policies":
       [{"name":"keep-everything","type":"always_sample"}]}}, ...}}
```

**這就是答案的具體呈現**：一個獨立的 Go server，透過 OpAMP Bridge，集中看到了 Operator 在 cluster 裡管理的 collector（`agent` 的 `load_balancing`、`gateway` 的 `tail_sampling`）此刻「實際生效的設定」——不需要你逐一 `kubectl get configmap`。

> 小觀察：bridge 會依 `heartbeatInterval` 週期性重連並重送一次完整狀態，所以 server log 會反覆出現 agent connect / report。這是正常行為。

---

## 5.6 整個 lab 的最終全貌

```
        ┌──────────────── opamp-server (你寫的 Go 控制面) ────────────────┐
        │            觀察所有 collector 的生效設定 / 健康                  │
        └───────────────────────────▲──────────────────────────────────┘
                                     │ ws OpAMP
                             ┌───────┴────────┐
                             │  OpAMP Bridge  │ (Operator 依 CR 跑起來)
                             └───────┬────────┘
                                     │ 回報 Operator 管理的 collector
   payment-service        order-service                agent-collector
   (inject-python +       (inject-java +               (DaemonSet,
    sidecar, Stage 4)      sidecar, Stage 3)            loadbalancing, Stage 2)
        │ OTLP                  │ OTLP                       │ routing_key: traceID
        └──────────┬───────────┘                            │
                   ▼ (各自 sidecar → agent)                  ▼
              agent-collector ───────────────▶ gateway-collector x2
                                               (tail_sampling 100%, Stage 2)
                                                         │ debug exporter
                                                         ▼
                                                    （真實環境換成 Tempo/Jaeger/SaaS）
```

五個需求全部對上：

| 需求 | 由哪一階段達成 |
|---|---|
| 1. 既有 Python(手動) + Java(無) + PostgreSQL | Stage 1 重現 |
| 2. Operator 建 collector(log/metric/trace + tail sampling 100%) + span load balancer | Stage 2 |
| 3. Operator 替 Java 注入 sidecar collector + auto-instrument | Stage 3 |
| 4. Python 從手動改用 Operator + auto-instrument | Stage 4 |
| 5. 用 Go server（OpAMP Bridge + OpAMP server）管理 Operator | Stage 5 |

---

## 練習 5

**進階動手題（下推 remote config）：** 目前 server 只「讀」不「寫」。試著讓 server 在 `OnMessage` 回傳一個帶 `RemoteConfig` 的 `ServerToAgent`，把某個 collector 的 exporter `verbosity` 從 `detailed` 改成 `basic`，觀察 bridge 是否套用、以及 `componentsAllowed` 白名單在這裡扮演什麼守門角色。

提示：要構造 `protobufs.AgentRemoteConfig{Config: &protobufs.AgentConfigMap{...}, ConfigHash: ...}`，塞進 `ServerToAgent.RemoteConfig`。

**閱讀理解題：** 為什麼說「OpAMP server 不直接管理 Operator，而是管理 collector」？如果我想透過 OpAMP 把 gateway 的 `replicas` 從 2 改成 4，這條路走得通嗎？

<details>
<summary>參考答案</summary>

**為什麼是管 collector 不是管 Operator：** OpAMP 是「agent 管理協議」，它的管理對象是「會回報 EffectiveConfig 的 agent」，也就是 collector。Operator 是 reconcile 引擎，本身不是 OpAMP agent。Bridge 站在中間：對 server 而言它代表那些 collector，對 cluster 而言它能影響 collector 的設定。

**改 replicas 走不走得通：** OpAMP 下推的是「collector 的執行設定（receivers/processors/exporters/service pipelines）」，`replicas` 是 `OpenTelemetryCollector` CR 的 **Kubernetes 部署層欄位**，不屬於 collector 自身的 OTel 設定。要改 replicas 仍然是改 CR（`kubectl`/GitOps）由 Operator reconcile。也就是說：**OpAMP 管「collector 跑什麼設定」，Operator 管「collector 怎麼部署」**，兩者分工不同。這也呼應 classroom 第 2 章 CR 的 spec 同時含「部署描述」與「config 內容」兩種性質的欄位。
</details>

---

## 清理整個 lab

```bash
# 移除 lab 的所有資源
kubectl delete namespace otel-lab

# 移除 operator（Helm 安裝，用 helm uninstall；選用）
helm -n opentelemetry-operator-system uninstall opentelemetry-operator
# CRD 不會被 helm uninstall 移除，要的話手動清：
kubectl delete crd opentelemetrycollectors.opentelemetry.io instrumentations.opentelemetry.io \
  opampbridges.opentelemetry.io targetallocators.opentelemetry.io

# 刪掉整個 k3d cluster（最乾淨）
k3d cluster delete otel-lab
```

---

| | |
|---|---|
| 上一步 | [← Stage 4](./04-python-migrate-to-operator.md) |
| 下一步 | [Stage 6：透過 OpAMP 遠端下推版本升級 →](./06-opamp-remote-version-upgrade.md) |
| 回到 | [README](./README.md) |
