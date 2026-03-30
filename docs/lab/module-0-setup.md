# Module 0：環境準備

## 目標

建立一個可以跑完所有後續模組的 k3d 本機 Kubernetes 環境，包含：
- k3d cluster
- cert-manager（operator webhook 依賴）
- OpenTelemetry Operator
- Grafana Tempo（traces 後端）
- Prometheus（metrics 後端）
- Grafana（dashboard）
- Demo 服務：order-service、inventory-service、PostgreSQL 17、RabbitMQ

完成本模組後，demo 服務可以正常互相呼叫，但**尚未有任何可觀測性數據**，這是後續各模組的起點。

---

## 步驟

### 0.1 建立 k3d cluster

```bash
k3d cluster create otel-lab \
  --agents 2 \
  --k3s-arg "--disable=traefik@server:0" \
  --port "8080:80@loadbalancer" \
  --port "8443:443@loadbalancer"
```

> `--disable=traefik` 是因為我們不需要 traefik，減少資源佔用。
> 兩個 agent node 是為了在 Module 3 展示 DaemonSet 行為。

確認 cluster 正常：

```bash
kubectl get nodes
# 應看到 1 server + 2 agent，狀態 Ready
```

---

### 0.2 建立 Namespace

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: observability
---
apiVersion: v1
kind: Namespace
metadata:
  name: demo
EOF
```

---

### 0.3 安裝 cert-manager

OTel operator 的 webhook 需要 TLS，cert-manager 負責自動簽發憑證。

```bash
helm repo add jetstack https://charts.jetstack.io
helm repo update

helm install cert-manager jetstack/cert-manager \
  --namespace cert-manager \
  --create-namespace \
  --set crds.enabled=true \
  --wait
```

等待 cert-manager 就緒：

```bash
kubectl -n cert-manager rollout status deployment/cert-manager
kubectl -n cert-manager rollout status deployment/cert-manager-webhook
```

---

### 0.4 安裝 OpenTelemetry Operator

```bash
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo update

helm install opentelemetry-operator open-telemetry/opentelemetry-operator \
  --namespace observability \
  --set "manager.collectorImage.repository=otel/opentelemetry-collector-contrib" \
  --wait
```

> `collectorImage.repository` 指定使用 contrib 版，包含更多 receiver/exporter（如 prometheusreceiver、rabbitmq receiver 等）。

確認 operator 就緒：

```bash
kubectl -n observability rollout status deployment/opentelemetry-operator-controller-manager
```

確認 CRD 已建立：

```bash
kubectl get crd | grep opentelemetry
# 應看到：
# instrumentations.opentelemetry.io
# opentelemetrycollectors.opentelemetry.io
# opampbridges.opentelemetry.io
# targetallocators.opentelemetry.io
```

---

### 0.5 安裝 Grafana Tempo

Tempo 是 Grafana 出品的分散式 tracing 後端，相容 Jaeger/Zipkin/OTLP 協議。

```bash
helm repo add grafana https://grafana.github.io/helm-charts
helm repo update

helm install tempo grafana/tempo \
  --namespace observability \
  --set tempo.storage.trace.backend=local \
  --wait
```

> 這裡用 local 儲存（ephemeral），適合學習環境。生產環境應用 S3/GCS。

確認 Tempo 的 OTLP 接收端口：

```bash
kubectl -n observability get svc tempo
# OTLP gRPC: 4317
# OTLP HTTP: 4318
# Tempo query: 3100
```

---

### 0.6 安裝 Prometheus

```bash
helm install prometheus prometheus-community/prometheus \
  --namespace observability \
  --set server.persistentVolume.enabled=false \
  --set alertmanager.enabled=false \
  --set prometheus-pushgateway.enabled=false \
  --wait
```

---

### 0.7 安裝 Grafana

```bash
helm install grafana grafana/grafana \
  --namespace observability \
  --set adminPassword=admin \
  --set persistence.enabled=false \
  --wait
```

設定 Grafana data sources（Tempo + Prometheus）：

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana-datasources
  namespace: observability
  labels:
    grafana_datasource: "1"
data:
  datasources.yaml: |
    apiVersion: 1
    datasources:
      - name: Tempo
        type: tempo
        url: http://tempo:3100
        isDefault: false
        jsonData:
          tracesToLogsV2:
            datasourceUid: ''
          serviceMap:
            datasourceUid: prometheus
          nodeGraph:
            enabled: true
      - name: Prometheus
        type: prometheus
        url: http://prometheus-server
        isDefault: true
EOF
```

取得 Grafana 存取網址：

```bash
kubectl -n observability port-forward svc/grafana 3000:80 &
# 開啟瀏覽器 http://localhost:3000，帳號 admin/admin
```

---

### 0.8 部署 PostgreSQL 17

```bash
helm repo add bitnami https://charts.bitnami.com/bitnami

helm install postgres bitnami/postgresql \
  --namespace demo \
  --set auth.postgresPassword=postgres \
  --set auth.database=orders \
  --set image.tag=17 \
  --set primary.persistence.enabled=false \
  --wait
```

---

### 0.9 部署 RabbitMQ

```bash
helm install rabbitmq bitnami/rabbitmq \
  --namespace demo \
  --set auth.username=admin \
  --set auth.password=admin \
  --set persistence.enabled=false \
  --wait
```

---

### 0.10 部署 Demo 服務

```bash
kubectl apply -f manifests/demo/
```

> manifests/demo/ 包含 order-service 和 inventory-service 的 Deployment、Service。
> 此時兩個服務都**沒有任何 OTel 相關設定**，是原始狀態。

驗證服務正常：

```bash
kubectl -n demo port-forward svc/order-service 8000:8000 &

# 建立一筆訂單
curl -X POST http://localhost:8000/orders \
  -H "Content-Type: application/json" \
  -d '{"product_id": "P001", "quantity": 2}'

# 應回傳 {"order_id": "...", "status": "created"}
```

---

## 完成檢查點

```bash
# 所有 pod 應為 Running
kubectl -n observability get pods
kubectl -n demo get pods

# 預期看到：
# observability: opentelemetry-operator, tempo, prometheus, grafana
# demo: order-service, inventory-service, postgres, rabbitmq
```

此時 Grafana Tempo 裡**沒有任何 trace**，Prometheus 只有基礎 k8s 指標，沒有應用程式指標。這就是我們的起點。

---

## 下一步

→ [Module 1：Auto-Instrumentation](./module-1-auto-instrumentation.md) — 不改一行程式碼，讓 trace 出現在 Tempo
