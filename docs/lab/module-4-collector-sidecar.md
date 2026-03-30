# Module 4：Collector — Sidecar 模式

## 目標

在 order-service pod 裡注入一個 sidecar collector，理解 sidecar 模式與 agent/gateway 的差異，以及何時適合用 sidecar。

---

## 架構

```
demo namespace
┌──────────────────────────────────────┐
│  order-service Pod                   │
│  ┌──────────────┐  ┌──────────────┐  │
│  │  app         │  │  otel-sidecar│  │
│  │  (FastAPI)   │──►  collector   │  │
│  │              │  │              │  │
│  └──────────────┘  └──────┬───────┘  │
└─────────────────────────── │ ────────┘
                             ▼
                  otel-gateway (Deployment)
```

Sidecar 的特點：
- 與 app container 共用同一個 Pod（共用 localhost 網路）
- App 可以用 `localhost:4317` 直接送 telemetry，不需要跨 node
- 每個 pod 都有獨立的 collector，完全隔離
- 資源消耗較高（每個 pod 都跑一個 collector）

---

## Sidecar vs Agent vs Gateway 比較

| | Sidecar | DaemonSet Agent | Deployment Gateway |
|--|---------|-----------------|-------------------|
| 粒度 | Per-pod | Per-node | Cluster-level |
| 隔離性 | 完全隔離 | 同 node 共享 | 所有 node 共享 |
| 資源效率 | 最低 | 中 | 最高 |
| 適合場景 | 多租戶、需嚴格隔離 | Host metrics、log | 集中匯聚、跨 cluster |
| tail-based sampling | 可行（單 pod 所有 span） | 不易 | 需要 sticky routing |

---

## 步驟

### 4.1 建立 Sidecar 用的 OpenTelemetryCollector CR

注意：sidecar 模式的 CR 本身不會建立 pod，而是作為**模板**，當 pod 有對應 annotation 時才注入。

```bash
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: otel-sidecar
  namespace: demo
spec:
  mode: sidecar

  config:
    receivers:
      otlp:
        protocols:
          grpc:
            endpoint: 0.0.0.0:4317
          http:
            endpoint: 0.0.0.0:4318

    processors:
      memory_limiter:
        check_interval: 1s
        limit_mib: 100
        spike_limit_mib: 25

      batch:
        timeout: 2s    # sidecar 批次時間較短，因為是 per-pod 的流量

      # 在 sidecar 加入 pod 自身資訊
      resource:
        attributes:
          - key: collector.mode
            value: sidecar
            action: insert

    exporters:
      # Forward 到 gateway
      otlp/gateway:
        endpoint: otel-gateway-collector.observability:4317
        tls:
          insecure: true

    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [memory_limiter, resource, batch]
          exporters: [otlp/gateway]
        metrics:
          receivers: [otlp]
          processors: [memory_limiter, resource, batch]
          exporters: [otlp/gateway]
EOF
```

---

### 4.2 在 order-service 加入 Sidecar Annotation

```yaml
# order-service Deployment
spec:
  template:
    metadata:
      annotations:
        instrumentation.opentelemetry.io/inject-python: "demo-instrumentation"
        sidecar.opentelemetry.io/inject: "otel-sidecar"  # 新增這行
```

```bash
kubectl patch deployment order-service -n demo \
  --type=json \
  -p='[
    {
      "op": "add",
      "path": "/spec/template/metadata/annotations/sidecar.opentelemetry.io~1inject",
      "value": "otel-sidecar"
    }
  ]'
```

---

### 4.3 觀察 Sidecar 注入效果

```bash
kubectl -n demo get pod -l app=order-service -o jsonpath='{.items[0].spec.containers[*].name}'
# 應看到兩個 container：app  otc-container
```

查看 sidecar container 的詳細資訊：

```bash
kubectl -n demo get pod -l app=order-service -o yaml | grep -A 20 "name: otc-container"
```

---

### 4.4 更新 Instrumentation CR 指向 localhost

Sidecar 的重要特性：app 可以用 `localhost` 直接跟 collector 溝通：

```bash
kubectl patch instrumentation demo-instrumentation -n demo \
  --type=merge \
  -p='{"spec":{"exporter":{"endpoint":"http://localhost:4317"}}}'

kubectl rollout restart deployment/order-service -n demo
```

> **注意**：`localhost:4317` 只對 order-service 有效（因為它有 sidecar）。
> inventory-service 還是要用 gateway 的地址。
>
> 這展示了 sidecar 模式的一個挑戰：不同 pod 的 endpoint 設定不一致。

---

### 4.5 觀察 Sidecar 的資源使用

```bash
kubectl -n demo top pod -l app=order-service --containers
# NAME                              CONTAINER       CPU    MEMORY
# order-service-xxx                 app             5m     80Mi
# order-service-xxx                 otc-container   2m     30Mi  ← sidecar 也佔資源
```

比較：有 sidecar 和沒有 sidecar 的 pod 資源消耗差異。

---

### 4.6 Sidecar 適合的場景：Per-Pod 的 Tail-Based Sampling

Sidecar 的一個特殊優勢：每個 pod 的 sidecar 能看到**同一個 service 的所有 span**，因此可以做 tail-based sampling：

```yaml
processors:
  tail_sampling:
    decision_wait: 10s
    num_traces: 10000
    expected_new_traces_per_sec: 100
    policies:
      - name: keep-errors
        type: status_code
        status_code: {status_codes: [ERROR]}
      - name: keep-slow
        type: latency
        latency: {threshold_ms: 1000}
      - name: sample-rest
        type: probabilistic
        probabilistic: {sampling_percentage: 10}
```

> 在 gateway 做 tail-based sampling 有問題：同一個 trace 的 span 可能分散在多個 collector 副本。
> Sidecar 模式可以完全避免這個問題（一個 pod 的所有 span 都在同一個 sidecar）。

---

## 完成後的清理（避免影響後續模組）

Sidecar 會讓後續模組的 endpoint 設定複雜化，可以選擇移除：

```bash
# 移除 sidecar annotation
kubectl patch deployment order-service -n demo \
  --type=json \
  -p='[{"op": "remove", "path": "/spec/template/metadata/annotations/sidecar.opentelemetry.io~1inject"}]'

# 把 Instrumentation 的 endpoint 改回 gateway
kubectl patch instrumentation demo-instrumentation -n demo \
  --type=merge \
  -p='{"spec":{"exporter":{"endpoint":"http://otel-gateway-collector.observability:4317"}}}'

kubectl rollout restart deployment/order-service -n demo
```

---

## 完成檢查點

- [ ] order-service pod 有兩個 container（app + otc-container）
- [ ] 以 `localhost:4317` 送出的 trace 仍可在 Tempo 看到
- [ ] 理解 sidecar vs agent vs gateway 的取捨

---

## 下一步

→ [Module 5：TargetAllocator](./module-5-target-allocator.md)
