# Stage 2：用 Operator 建 Gateway Collector + Span Load Balancer

> **本階段目標：** 用兩個 `OpenTelemetryCollector` CR 建立「agent（DaemonSet）→ gateway（StatefulSet）」兩層架構。gateway 做 tail sampling（先全收 100%），agent 用 loadbalancing exporter 以 trace ID 把同一條 trace 固定送到同一個 gateway 副本——這就是你要的「otel span load balancer」。

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
      tail_sampling:
        decision_wait: 10s
        policies:
          - name: keep-everything
            type: always_sample   # ← 保留 100%
    service:
      pipelines:
        traces:
          processors: [tail_sampling, batch]
```

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
  config:
    exporters:
      load_balancing:            # 注意：用 load_balancing（loadbalancing 已被標記為 deprecated alias）
        routing_key: traceID
        resolver:
          dns:
            hostname: gateway-collector-headless.otel-lab.svc.cluster.local
            port: "4317"         # 必須是「字串」，給數字會 unmarshal 失敗
```

```bash
kubectl apply -f manifests/11-collector-agent-lb.yaml

kubectl -n otel-lab get daemonset agent-collector
# DESIRED 應該等於節點數（2 個 agent 節點 → 2，或含 server 節點視排程而定）
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

---

## 2.6 這一階段對應到的 Operator 機制

| 你做的事 | Operator 內部 | classroom 章節 |
|---|---|---|
| apply `OpenTelemetryCollector` (statefulset) | controller reconcile 出 StatefulSet + headless Service + ConfigMap | 第 3、5 章 |
| 改 `replicas` / `config` | reconcile 偵測差異、mutate 既有資源 | 第 5 章（樂觀鎖、mutate 策略） |
| `mode` 決定資源型態 | `apis/v1beta1/mode.go` + `internal/manifests/collector/` | 第 2、3 章 |

---

## 練習 2

**動手題：** 把 gateway 的 `replicas` 從 2 改成 3，重新 apply，再進 agent Pod 用 `nslookup` 查 headless service，確認解析到的 IP 變成 3 個。觀察過程中 Operator 有沒有重建整個 StatefulSet，還是只是 scale。

**進階思考題：** tail sampling 的 `policies` 現在是 `always_sample`（全留）。如果改成「只保留有 error 的 trace」，policy 要怎麼寫？而且為什麼這個決策「一定」要放在 gateway、不能放在 agent？

<details>
<summary>參考答案</summary>

**改 replicas：** Operator 只會更新 StatefulSet 的 `spec.replicas`，由 StatefulSet controller 滾動新增 Pod，不會重建。headless service 的 Endpoints 自動多一個，resolver 下次 DNS 重新解析就會帶到第 3 個 backend。

**只留 error 的 policy：**
```yaml
tail_sampling:
  policies:
    - name: errors-only
      type: status_code
      status_code:
        status_codes: [ERROR]
```

**為什麼一定在 gateway：** 判斷「整條 trace 有沒有 error」需要看到 trace 的**所有** span。agent 是每節點一份、只看得到本節點 app 的 span，一條跨服務的 trace 會分散在多個 agent，沒有任何一個 agent 看得到完整 trace。只有經過 loadbalancing 把同 trace 收斂到單一 gateway 副本後，那個副本才擁有完整 trace，才能正確判斷。這就是 2.1 的核心。
</details>

---

| | |
|---|---|
| 上一步 | [← Stage 1](./01-baseline-apps.md) |
| 下一步 | [Stage 3：Java sidecar + auto-instrument →](./03-java-sidecar-and-autoinstrument.md) |
