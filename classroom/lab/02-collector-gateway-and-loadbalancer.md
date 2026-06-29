# Stage 2：用 Operator 建 Gateway Collector + Span Load Balancer

> **本階段目標：** 用兩個 `OpenTelemetryCollector` CR 建立「agent（DaemonSet）→ gateway（StatefulSet）」兩層架構。gateway 做 tail sampling，agent 用 loadbalancing exporter 以 trace ID 把同一條 trace 固定送到同一個 gateway 副本——這就是你要的「otel span load balancer」。同時，依照 OTel 官方的生產級指引，把 `memory_limiter`、`k8sattributes`（含 RBAC）、`resourcedetection`、真實的 tail sampling 策略一起補上。

---

## 2.1 為什麼 tail sampling 需要「span load balancer」？

先講清楚問題，不然這層架構會看起來很多餘。

**Head sampling vs Tail sampling：**

```
Head sampling（在來源端決定）：
  span 一產生就丟硬幣決定留不留 → 無法根據「整條 trace 的結果」決策
  （例如：想「只保留有錯誤的 trace」就做不到，因為產生第一個 span 時還不知道後面會不會出錯）

Tail sampling（在 collector 端決定）：
  先把一條 trace 的所有 span 收齊、等一小段時間，再根據整條 trace 的特徵決定
  （可以做「有 error 才留」「latency > 1s 才留」等）
```

Tail sampling 的前提是 **「同一條 trace 的所有 span 必須進到同一個 collector 實例」**。

**多副本 gateway 的災難：**

```
            gateway-0          gateway-1
trace A ──▶ spanA1            spanA2  ◀── trace A 的另一半
            spanA3
                   ▲                ▲
                   │  各看到一半     │
                   └─ 兩個副本都以為自己看到的是「完整」的 trace A
                      → tail sampling 各做各的錯誤決策（一邊留一邊丟，trace 破碎）
```

**解法：在 gateway 前面放一層 loadbalancing exporter**，用 trace ID 當 routing key：

```
                       routing_key = traceID
agent ──▶ loadbalancing ──┬── hash(traceA) → 永遠 gateway-0
                          └── hash(traceB) → 永遠 gateway-1

結果：trace A 的所有 span 100% 落在 gateway-0，tail sampling 看到完整 trace ✓
```

這就是 loadbalancing exporter 被稱為「span load balancer」的原因：它平衡的不是「請求」，而是「以 trace 為單位的 span 群組」。

---

## 2.2 兩層架構長這樣

```
   app sidecar / SDK
        │ OTLP
        ▼
  ┌─────────────────────┐   agent-collector (DaemonSet, 每節點一份)
  │ receivers: otlp     │   ← 11-collector-agent-lb.yaml
  │ exporters:          │
  │   loadbalancing ────┼───┐ routing_key: traceID
  │     resolver: dns   │   │ 解析 gateway headless service
  └─────────────────────┘   │ 拿到每個 gateway 副本的 IP
                            │
        ┌───────────────────┴───────────────────┐
        ▼                                       ▼
  ┌─────────────┐                        ┌─────────────┐
  │ gateway-0   │                        │ gateway-1   │   gateway (StatefulSet x2)
  │ tail_sampling│                        │ tail_sampling│   ← 10-collector-gateway.yaml
  │  → debug     │                        │  → debug     │
  └─────────────┘                        └─────────────┘
```

**為什麼 gateway 用 StatefulSet？** loadbalancing 的 dns resolver 需要解析「每一個副本的穩定位址」。Operator 對 `mode: statefulset` 會自動建立一個 **headless service**（`gateway-collector-headless`），DNS 查它會回傳所有 Pod 的 IP，正好給 resolver 用。`mode: deployment` 的 ClusterIP service 只會回一個 VIP，達不到「routing 到特定副本」的效果。

---

## 2.3 部署 gateway

先看設定重點（完整檔案：[`manifests/10-collector-gateway.yaml`](./manifests/10-collector-gateway.yaml)）：

```yaml
spec:
  mode: statefulset        # ← 換來 headless service
  replicas: 2              # ← 兩個副本，凸顯 loadbalancing 的必要
  config:
    processors:
      memory_limiter:      # ← 每條 pipeline 的第一道，記憶體背壓
        check_interval: 1s
        limit_mib: 400
        spike_limit_mib: 100
      tail_sampling:
        decision_wait: 10s
        policies:
          - name: errors          # 有 error 的 trace 全留
            type: status_code
            status_code: { status_codes: [ERROR] }
          - name: slow            # latency > 500ms 全留
            type: latency
            latency: { threshold_ms: 500 }
          - name: random-10pct    # 其餘只隨機留 10%
            type: probabilistic
            probabilistic: { sampling_percentage: 10 }
    service:
      pipelines:
        traces:
          processors: [memory_limiter, tail_sampling, batch]
```

### 為什麼補這兩段（對照 OTel 官方的生產級指引）

[Managed telemetry platforms for K8s][bp] 這份 blueprint、以及 [Mastodon][mst] / [Skyscanner][sky] 兩個 reference implementation 反覆強調幾件事，原本第一版 lab 為了聚焦 loadbalancing 而省略了，這裡補回來：

**1. `memory_limiter` 是必備、不是選配。** blueprint 跟 Skyscanner 都用 "essential / mandatory" 形容它。它的作用是「背壓」：當 collector 記憶體逼近上限，主動拒收新資料，而不是放任自己 OOM 被 kill。一旦 collector 被 kill，記憶體裡還沒送出的整批資料全部消失——那比「擋掉一小段」嚴重得多。**規則：放在每條 pipeline 的最前面**，才能在資料進到後面昂貴的 processor（如 tail_sampling）前先擋下。

> `limit_mib`（固定值）在 lab 最好懂。生產環境建議改用 `limit_percentage` + `spike_limit_percentage`，並在 `spec.resources.limits.memory` 設好記憶體上限，讓百分比有依據。`spec.resources` 是 `OpenTelemetryCollector` CR 的欄位，Operator 會把它套到生成的 Pod 上（對應 classroom 第 3 章的 manifests builder）。

**2. tail sampling 要用「真的會丟東西」的策略。** 原本的 `always_sample`（全留）只是教學起點。Mastodon 的實際策略是「**錯誤 100% 留、成功只留約 0.1%**」，靠採樣控制成本，而不是靠 resource limit 砍量。這裡示範一個更貼近真實的組合：

```
policy 之間是 OR —— 任一條判定 sample，整條 trace 就保留：
  errors        ──┐
  slow (>500ms) ──┼──▶ 留下：所有錯誤 + 所有慢的 + 其餘的 10%
  random 10%    ──┘
```

[bp]: https://opentelemetry.io/docs/guidance/blueprints/managed-telemetry-platforms-for-k8s-workloads/
[mst]: https://opentelemetry.io/docs/guidance/reference-implementations/mastodon/
[sky]: https://opentelemetry.io/docs/guidance/reference-implementations/skyscanner/

```bash
kubectl apply -f manifests/10-collector-gateway.yaml

# Operator reconcile 出來的資源
kubectl -n otel-lab get statefulset,svc,configmap -l app.kubernetes.io/instance=otel-lab.gateway
# 預期看到：
#   statefulset.apps/gateway-collector            2/2
#   service/gateway-collector                     (ClusterIP)
#   service/gateway-collector-headless            (None / headless)  ← 關鍵
#   service/gateway-collector-monitoring
#   configmap/gateway-collector-<hash>
```

確認 headless service 真的是 headless：

```bash
kubectl -n otel-lab get svc gateway-collector-headless -o jsonpath='{.spec.clusterIP}{"\n"}'
# 輸出：None    ← headless 的特徵
```

---

## 2.4 部署 agent（span load balancer）

設定重點（完整檔案：[`manifests/11-collector-agent-lb.yaml`](./manifests/11-collector-agent-lb.yaml)）：

```yaml
spec:
  mode: daemonset
  serviceAccount: agent-collector   # ← 綁定帶 RBAC 的 SA（見下方說明）
  config:
    processors:
      memory_limiter: { check_interval: 1s, limit_mib: 200, spike_limit_mib: 50 }
      k8sattributes:               # 補上 k8s.pod.name / k8s.namespace / k8s.deployment...
        auth_type: serviceAccount
        pod_association:
          - sources: [{ from: resource_attribute, name: k8s.pod.ip }]
          - sources: [{ from: connection }]
      resourcedetection: { detectors: [env, system, k8snode] }
      resource:                    # 統一注入組織自訂屬性
        attributes: [{ key: deployment.environment.name, value: lab, action: upsert }]
    exporters:
      load_balancing:            # 注意：用 load_balancing（loadbalancing 已被標記為 deprecated alias）
        routing_key: traceID
        resolver:
          dns:
            hostname: gateway-collector-headless.otel-lab.svc.cluster.local
            port: "4317"         # 必須是「字串」，給數字會 unmarshal 失敗
    service:
      pipelines:
        traces:
          processors: [memory_limiter, k8sattributes, resourcedetection, resource]
          exporters: [load_balancing]
```

### 為什麼補強放在 agent，而且需要 RBAC

**補強放在 agent（不是 gateway）的原因：** `k8sattributes` 靠「來源 pod IP / 連線來源」去對應「這筆資料是哪個 Pod 發的」。agent 是 daemonset、就跑在 app 所在節點、直接收 app 送來的 OTLP，pod IP 對得上。gateway 收到的是 agent 經 loadbalancing 轉發來的流量，來源 IP 已經是 agent 的、不是原始 app 的——放在 gateway 會對應到錯的 Pod。這也呼應 blueprint 的「在靠近來源的 local 層做 k8s 補強」。

**為什麼要 RBAC：** `k8sattributes` 要去 API server 讀 `pods` / `namespaces` / `replicasets`（反推 deployment 名），`resourcedetection` 的 `k8snode` detector 要讀 `nodes`。blueprint 特別警告：**少了這些權限會「靜默失敗」**——資料照流，但少了 k8s 標籤，你很難第一時間發現。所以 [`11-collector-agent-lb.yaml`](./manifests/11-collector-agent-lb.yaml) 裡明確帶了一組 `ServiceAccount` + `ClusterRole` + `ClusterRoleBinding`，並用 `spec.serviceAccount` 綁上去。

> Operator 其實有能力「依 collector config 自動產生這組 RBAC」（偵測到 config 用了 `k8sattributes` 就幫你建 ClusterRole/Binding），但會受 feature gate 與 operator 自身權限影響。lab 選擇手寫，一來保證在任何環境都能跑，二來讓你看清楚這個 processor 到底需要哪些權限——這正是 classroom 裡 RBAC / ServiceAccount 主題的具體落地。

```bash
kubectl apply -f manifests/11-collector-agent-lb.yaml

kubectl -n otel-lab get daemonset agent-collector
# DESIRED 應該等於節點數（2 個 agent 節點 → 2，或含 server 節點視排程而定）

# 確認 RBAC 都建好了
kubectl -n otel-lab get sa agent-collector
kubectl get clusterrole,clusterrolebinding otel-lab-agent-collector
```

---

## 2.5 驗證 loadbalancing 真的有解析到副本

```bash
# 看 agent 的 log，loadbalancing exporter 啟動時會記錄解析到的 backend 數量
kubectl -n otel-lab logs daemonset/agent-collector | grep -i -E 'loadbalanc|resolver|backend' | head
```

更直接的驗證：headless service 的 Endpoints 應該對應 2 個 gateway 副本的 Pod IP。

```bash
# 方法 1：直接看 headless service 的 endpoints（最簡單，不需進 Pod）
kubectl -n otel-lab get endpoints gateway-collector-headless \
  -o jsonpath='{range .subsets[*].addresses[*]}{.ip}{"\n"}{end}'
# 預期：兩個不同的 Pod IP

# 對照 gateway 兩個副本的 IP，應一致
kubectl -n otel-lab get pods -l app.kubernetes.io/instance=otel-lab.gateway \
  -o jsonpath='{range .items[*]}{.metadata.name}{"  "}{.status.podIP}{"\n"}{end}'
```

> 注意：collector 用的是 distroless image，**Pod 裡沒有 `nslookup` / `getent` 等指令**，不能用 `kubectl exec ... nslookup` 來查 DNS。要從叢集內做 DNS 解析，得另開一個有工具的暫時 Pod，例如：
> ```bash
> kubectl -n otel-lab run dnstest --rm -it --restart=Never --image=busybox:1.36 -- \
>   nslookup gateway-collector-headless.otel-lab.svc.cluster.local
> ```

> 此時 agent / gateway 已經就緒，但還沒有 app 把資料送進來——app 端要等 Stage 3/4 接上 sidecar 後，資料才會流經 agent → gateway。本階段先把「管線」鋪好。
>
> **等 Stage 3/4 有資料後，回來這裡驗證補強有生效：** 看 gateway 的 debug log，每個 span 的 resource 區塊應該帶有 `k8s.pod.name`、`k8s.namespace.name`、`k8s.deployment.name`、`deployment.environment.name=lab` 等屬性——這些就是 agent 的 `k8sattributes` / `resource` processor 補上去的。若完全沒看到 `k8s.*`，先檢查 RBAC（`kubectl get clusterrolebinding otel-lab-agent-collector`）跟 agent log 有沒有權限錯誤。
> ```bash
> kubectl -n otel-lab logs statefulset/gateway-collector | grep -E 'k8s.pod.name|deployment.environment' | head
> ```

---

## 2.6 這一階段對應到的 Operator 機制

| 你做的事 | Operator 內部 | classroom 章節 |
|---|---|---|
| apply `OpenTelemetryCollector` (statefulset) | controller reconcile 出 StatefulSet + headless Service + ConfigMap | 第 3、5 章 |
| 改 `replicas` / `config` | reconcile 偵測差異、mutate 既有資源 | 第 5 章（樂觀鎖、mutate 策略） |
| `mode` 決定資源型態 | `apis/v1beta1/mode.go` + `internal/manifests/collector/` | 第 2、3 章 |
| `spec.serviceAccount` + 自建 RBAC | collector Pod 以該 SA 身分呼叫 API server，供 `k8sattributes`/`resourcedetection` 讀取資源 | 第 3 章（manifests builder） |

---

## 練習 2

**動手題：** 把 gateway 的 `replicas` 從 2 改成 3，重新 apply，再進 agent Pod 用 `nslookup` 查 headless service，確認解析到的 IP 變成 3 個。觀察過程中 Operator 有沒有重建整個 StatefulSet，還是只是 scale。

**進階思考題：** gateway 的 tail sampling 現在有三條 policy（`errors` / `slow` / `random-10pct`）。一條「又快又成功」的 trace 會被留下嗎？一條「有 error 但很快」的 trace 呢？policy 之間是 AND 還是 OR？而且為什麼這整個決策「一定」要放在 gateway、不能放在 agent？

**選讀題：** 為什麼 `k8sattributes` 放在 agent、`tail_sampling` 放在 gateway？兩者都是「補強/決策」，差別在哪？

<details>
<summary>參考答案</summary>

**改 replicas：** Operator 只會更新 StatefulSet 的 `spec.replicas`，由 StatefulSet controller 滾動新增 Pod，不會重建。headless service 的 Endpoints 自動多一個，resolver 下次 DNS 重新解析就會帶到第 3 個 backend。

**三條 policy 的判定：** policy 之間是 **OR**——任一條判定 sample，整條 trace 就保留。所以「又快又成功」的 trace 三條都不中（除非剛好落在 `random-10pct` 的 10%），大多會被丟；「有 error 但很快」的 trace 命中 `errors`，**一定保留**。這就是 Mastodon「錯誤全留、成功只留一小撮」策略的寫法。

**為什麼 tail sampling 一定在 gateway：** 判斷「整條 trace 有沒有 error / 夠不夠慢」需要看到 trace 的**所有** span。agent 是每節點一份、只看得到本節點 app 的 span，一條跨服務的 trace 會分散在多個 agent，沒有任何一個 agent 看得到完整 trace。只有經過 loadbalancing 把同 trace 收斂到單一 gateway 副本後，那個副本才擁有完整 trace，才能正確判斷。這就是 2.1 的核心。

**為什麼 `k8sattributes` 在 agent、`tail_sampling` 在 gateway：** 兩者依賴的「上下文」不同。`k8sattributes` 依賴**來源 Pod 的身分**（pod IP / 連線來源），這個資訊只有在「最靠近 app 的那一跳」才正確——也就是 agent。`tail_sampling` 依賴**整條 trace 的全貌**，必須等 span 收斂到同一副本才成立——也就是 loadbalancing 之後的 gateway。一個要「靠近來源」，一個要「看到全部」，方向剛好相反。
</details>

---

| | |
|---|---|
| 上一步 | [← Stage 1](./01-baseline-apps.md) |
| 下一步 | [Stage 3：Java sidecar + auto-instrument →](./03-java-sidecar-and-autoinstrument.md) |
