# Stage 0：環境準備

> **本階段目標：** 建好一個能跑 webhook 的 k3d cluster，用 **Helm** 安裝 OpenTelemetry Operator（憑證用 chart 自簽，不需要 cert-manager），並把兩個 app image build 出來、import 進 cluster。做完這一階段，後面四個階段才有舞台。

---

## 0.1 你需要先裝好的工具

| 工具 | 用途 | 檢查指令 |
|---|---|---|
| Docker | 跑 k3d、build image | `docker version` |
| [k3d](https://k3d.io) | 本機 Kubernetes（k3s） | `k3d version` |
| kubectl | 操作 cluster | `kubectl version --client` |
| [helm](https://helm.sh) v3 | 安裝 Operator | `helm version` |
| (build Java 用) Docker 即可 | image 內含 maven，不需本機裝 JDK | — |

> Java / Python image 都用多階段 Dockerfile，build 過程在容器內完成，**你本機不需要裝 JDK 或 Python**。

---

## 0.2 建立 k3d cluster

```bash
k3d cluster create otel-lab \
  --agents 2 \
  --port "18080:80@loadbalancer"

kubectl cluster-info
kubectl get nodes
# 預期：1 個 server + 2 個 agent，共 3 個節點
```

用 2 個 agent 節點的原因：Stage 2 的 agent collector 是 DaemonSet（每節點一份），多節點才看得出「每節點一個 agent」的效果。

> **常見坑：node 卡在 NotReady / 建立失敗，log 出現 `too many open files`。** 同時跑多個 k3s/k3d cluster 時，很容易把 host 的 inotify 上限（`fs.inotify.max_user_instances`，預設常是 128）吃滿，導致 kubelet 無法註冊。提高它即可（重開機會還原）：
> ```bash
> # 有 sudo：
> sudo sysctl -w fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=524288
> # 沒有 sudo 但能用 docker：用一個 privileged 容器寫進 host kernel
> docker run --rm --privileged alpine sysctl -w fs.inotify.max_user_instances=1024 fs.inotify.max_user_watches=524288
> ```
> 調整後重新 `k3d cluster delete otel-lab && k3d cluster create ...`。

---

## 0.3 用 Helm 安裝 OpenTelemetry Operator

Operator 有兩種常見裝法：

| 裝法 | 來源 | webhook 憑證 |
|---|---|---|
| `kubectl apply -f .../opentelemetry-operator.yaml` | 本 repo release 的 bundle manifest | **需要另外裝 cert-manager** |
| **Helm chart**（本 lab 採用） | [opentelemetry-helm-charts](https://github.com/open-telemetry/opentelemetry-helm-charts) | 可用 chart 內建自簽憑證，**不需要 cert-manager** |

我們用 Helm，並開啟 `autoGenerateCert`，讓 chart 自己產生 webhook 用的自簽憑證——這樣連 cert-manager 都省了。

```bash
helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
helm repo update

helm install opentelemetry-operator open-telemetry/opentelemetry-operator \
  --version 0.117.0 \
  -n opentelemetry-operator-system --create-namespace \
  --set admissionWebhooks.certManager.enabled=false \
  --set admissionWebhooks.autoGenerateCert.enabled=true \
  --wait
# chart 0.117.0 對應 operator appVersion 0.153.0（與本 lab 其他元件版本一致）
```

> **webhook 憑證哪來的？背景知識展開（classroom 第 4 章）：** `kubectl apply` 不會直接建立 Pod，中間會先經過 API Server 的 **Mutating Admission Webhook**：
>
> ```
> kubectl apply → API Server → Mutating Webhook（Operator 在這裡改 Pod spec）→ 存入 etcd → 排程
> ```
>
> Operator 把「解碼 Pod → 呼叫各個 `PodMutator.Mutate()` → 算出 JSON Patch → 回傳」這段邏輯放在 [`internal/webhook/podmutation/webhookhandler.go`](../../internal/webhook/podmutation/webhookhandler.go)（第 60 行起）。這個 HTTP handler 要跑在 HTTPS 上，API Server 才會信任它——這就是「webhook 需要 TLS 憑證」的原因，**憑證本身跟注入邏輯無關，只是 API Server ↔ Webhook Server 這段連線的身分驗證**。
>
> - 用 bundle manifest 時，這份憑證由 **cert-manager** 簽發（所以那條路一定要先裝 cert-manager）。
> - 用 Helm + `autoGenerateCert.enabled=true` 時，**chart 自己產生自簽 CA 與憑證**並塞進 webhook 設定，因此不需要 cert-manager。
>
> 如果你的叢集已經有 cert-manager、想交給它管，把上面兩個 `--set` 換成 `--set admissionWebhooks.certManager.enabled=true` 即可。

驗證 operator 與 CRD：

```bash
kubectl -n opentelemetry-operator-system get pods
# opentelemetry-operator-xxxx  1/1  Running

# chart 產生的自簽憑證（不是 cert-manager）
kubectl -n opentelemetry-operator-system get secret | grep cert
# opentelemetry-operator-controller-manager-service-cert  kubernetes.io/tls

kubectl get crd | grep opentelemetry.io
# 預期看到：
#   instrumentations.opentelemetry.io
#   opampbridges.opentelemetry.io
#   opentelemetrycollectors.opentelemetry.io
#   targetallocators.opentelemetry.io
```

這四個 CRD 對應 Operator 的四種 Custom Resource（背景知識展開，classroom 第 2 章）：

| CRD | 這個 lab 會用它做什麼 | 特性 |
|---|---|---|
| `opentelemetrycollectors.opentelemetry.io` | Stage 2 的 gateway/agent、Stage 3/4 的 sidecar 範本 | `spec.mode` 決定建 Deployment / DaemonSet / StatefulSet，或不建資源直接注入（`sidecar`） |
| `instrumentations.opentelemetry.io` | Stage 3/4 的 `lab-instrumentation` | **本身不建立 Pod**，只是「注入設定範本」，等 Pod annotation 指到它才生效 |
| `opampbridges.opentelemetry.io` | Stage 5 | 讓 Operator 建一個連到外部 OpAMP server 的 Deployment |
| `targetallocators.opentelemetry.io` | 本 lab 未使用 | 分配 Prometheus scrape target 給多個 Collector 副本 |

---

## 0.5 Build 兩個 app image 並 import 進 k3d

`k3d image import` 把本機 Docker image 直接灌進 cluster 節點，**不需要 registry**（classroom 第 10.3 節介紹過）。

```bash
# 在 repo 根目錄執行
cd classroom/lab

# Java order-service（第一次 build 會比較久，要下載 maven 相依）
docker build -t order-service:lab ./apps/order-service

# Python payment-service
docker build -t payment-service:lab ./apps/payment-service

# import 進 cluster
k3d image import order-service:lab payment-service:lab --cluster otel-lab
```

> opamp-server image 留到 Stage 5 再 build，避免一次卡太久。

---

## 0.6 驗證

```bash
# Operator 正常運行（Helm 裝出來的 deployment 叫 opentelemetry-operator）
kubectl get pods -n opentelemetry-operator-system
# opentelemetry-operator-xxxx  1/1  Running

# helm release 狀態
helm -n opentelemetry-operator-system list

# image 已經在 cluster 裡（在任一節點容器內查 k3s 的 image）
docker exec k3d-otel-lab-server-0 crictl images | grep -E 'order-service|payment-service'
```

---

## 本階段你完成了什麼

```
┌─────────────────────────────────────────────────────────┐
│  k3d cluster: otel-lab (1 server + 2 agents)            │
│                                                         │
│              ┌──────────────────────────────┐           │
│              │  opentelemetry-operator       │           │
│              │  (Helm 安裝, 含自簽 webhook 憑證) │           │
│              │  controller + webhook         │           │
│              └──────────────────────────────┘           │
│   （不需要 cert-manager，憑證由 chart 自己產生）            │
│                                                         │
│  已 import 的 image: order-service:lab, payment-service:lab │
└─────────────────────────────────────────────────────────┘
```

下一階段會把 PostgreSQL + 兩個服務部署上去，重現「接手時的現況」。

---

## 練習 0

**問：** `make run`（在本機跑 operator）和我們這裡用 Helm 把 operator 裝進 cluster，對「webhook 注入」這件事有什麼關鍵差異？webhook 的 TLS 憑證又是哪來的？

<details>
<summary>參考答案</summary>

`make run` 把 manager 跑在本機 process，k8s API server 無法回連到你本機的 webhook server（需要 in-cluster 可達的 service + TLS），所以 **MutatingWebhook 不會生效**，auto-instrumentation / sidecar 注入不會發生。

裝進 cluster 後，operator 以 Deployment 形式運行，webhook service 在叢集內可達，API server 才能安全呼叫它、注入才會真正觸發。這也是為什麼這份 lab 一定要用 k3d 而不是 `make run`。

**憑證來源**取決於安裝方式：bundle manifest 那條路靠 **cert-manager** 簽發；本 lab 的 Helm + `autoGenerateCert` 則由 **chart 自己產生自簽 CA/憑證**塞進 webhook 設定，所以不需要 cert-manager。兩者最終效果一樣：API server 能用 TLS 安全呼叫 webhook。

（對應 classroom 第 10.1 與第 4 章。）
</details>

---

| | |
|---|---|
| 上一步 | [← README](./README.md) |
| 下一步 | [Stage 1：重現現況 →](./01-baseline-apps.md) |
