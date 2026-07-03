# 實戰 Lab：用 OpenTelemetry Operator 接管混合語言服務的可觀測性

> **這份 lab 是什麼：** classroom 前 11 章是「讀懂 Operator 程式碼」，這份 lab 是「動手把 Operator 用在一個真實情境」。你會從一個半成品的系統出發，逐步用 Operator 統一管理 OTel Collector 與 auto-instrumentation。
>
> **預期時間：** 90 ~ 150 分鐘
> **前置知識：** classroom 第 1、2、4、6 章（Operator Pattern、CRD、Webhook 注入、Auto Instrumentation）

---

## 前置知識速覽（classroom 第 1、2、4、6 章重點）

> 讀過對應章節可以跳過這節；沒讀過也不影響往下走，遇到「為什麼 apply 一個 annotation 就能觸發注入」「Operator 怎麼知道要建立什麼資源」這類疑問時，回來看這裡就好。各 stage 文件裡出現「classroom 第 N 章」的地方，也會就地展開跟當下操作直接相關的部分。

### 第 1 章：Operator Pattern —— Reconcile 是什麼

Operator 的核心是一個不停跑的控制迴圈：**觀察 CR 的 Spec（期望狀態）→ 跟 cluster 目前實際狀態比較 → Create/Update/Delete 補上差距**。這個迴圈就是 `Reconcile()` 函式：

**檔案：** [`../internal/controllers/opentelemetrycollector_controller.go`](../../internal/controllers/opentelemetrycollector_controller.go)（第 231-293 行）

```go
func (r *OpenTelemetryCollectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var instance v1beta1.OpenTelemetryCollector
    r.Get(ctx, req.NamespacedName, &instance)              // 讀期望狀態
    desiredObjects, _ := BuildCollector(params)              // 算出「應該有哪些資源」
    ownedObjects, _ := r.findOtelOwnedObjects(ctx, params)   // 查「目前實際有哪些」
    reconcileDesiredObjects(ctx, r.Client, log, &instance, params.Scheme, desiredObjects, ownedObjects) // 補差距
    return collectorStatus.HandleReconcileStatus(ctx, log, params, instance, err)
}
```

`Reconcile()` 必須**冪等**——重複呼叫結果要一樣，因為 k8s 不保證只呼叫一次。這正是這份 lab 裡「改一個 CR 欄位，Operator 自動把對應資源改掉」的原理：不是有人手動介入，是控制迴圈偵測到 Spec 跟現況有差距、自動補上。Stage 2 練習「把 gateway 的 `replicas` 從 2 改成 3」，答案會提到「Operator 只更新 StatefulSet 的 `spec.replicas`」，就是這個機制的直接體現。

### 第 2 章：CRD —— 四個 CR 分別是什麼

本 lab 用到 Operator 的全部四種 CR：

| CR | Kind | 這個 lab 怎麼用它 |
|---|---|---|
| `opentelemetrycollectors.opentelemetry.io` | `OpenTelemetryCollector` | Stage 2 的 gateway/agent、Stage 3/4 的 `app-sidecar` |
| `instrumentations.opentelemetry.io` | `Instrumentation` | Stage 3/4 的 `lab-instrumentation`——**不建立 Pod**，只是一份被 Webhook 讀取的「注入設定範本」 |
| `opampbridges.opentelemetry.io` | `OpAMPBridge` | Stage 5 |
| `targetallocators.opentelemetry.io` | `TargetAllocator` | 本 lab 未使用（分配 Prometheus scrape target 給多個 Collector 副本） |

**檔案：** [`../apis/v1beta1/opentelemetrycollector_types.go`](../../apis/v1beta1/opentelemetrycollector_types.go)

`OpenTelemetryCollector` 的 `spec.mode` 決定 Operator 建立什麼資源：

```go
// apis/v1beta1/mode.go:6-24
const (
    ModeDaemonSet   Mode = "daemonset"    // Stage 2 的 agent：每節點一份
    ModeDeployment  Mode = "deployment"
    ModeSidecar     Mode = "sidecar"      // Stage 3/4 的 app-sidecar：不建立獨立資源，注入到別的 Pod
    ModeStatefulSet Mode = "statefulset"  // Stage 2 的 gateway：需要 headless service 給 loadbalancing resolver 用
)
```

`Instrumentation` CR 本身「什麼都不做」，只有被 Pod annotation 指到時，Webhook 才會讀它的 spec 去注入——這正是 Stage 3 §3.2 反覆強調的「CR 是範本，annotation 才是觸發」。

### 第 4 章：Webhook 注入機制 —— 誰在什麼時候修改 Pod

`kubectl apply` 建立 Pod 時，中間會經過 API Server 的 **Mutating Admission Webhook**——這是整個 lab「零改 code 就能注入 sidecar/agent」的原理：

```
kubectl apply → API Server → Mutating Webhook（Operator 在這裡注入）→ 存入 etcd → 排程
```

Webhook 是同步的：Pod 真正建立前，Operator 已經把 initContainer、env var、volume 修改完，用 JSON Patch 回傳給 API Server。三層架構：

```
internal/webhook/podmutation/webhookhandler.go   ← HTTP 層，呼叫每個 PodMutator
internal/instrumentation/podmutator.go           ← 決策層，讀 annotation → 找 Instrumentation CR
internal/instrumentation/sdk.go                  ← 執行層，injectJava()/injectPython()...
```

**跟這份 lab 直接對應：** Stage 3/4 apply 帶 annotation 的 Deployment 後，`kubectl get pod -o yaml` 看到的 `initContainers`、`JAVA_TOOL_OPTIONS`、`PYTHONPATH` 都是這個 Webhook 在 Pod 建立當下即時 patch 上去的，**app image 本身完全沒變**。`failurePolicy=ignore` 也解釋了：如果 Operator 掛了，Pod 還是能建立，只是不會被注入。

### 第 6 章：Auto Instrumentation —— annotation 怎麼變成注入內容

`inject-java`/`inject-python` 這類 annotation 的值有四種語意（Stage 3 §3.3 已經用過，這裡是它的完整規則）：

| 值 | 意義 |
|---|---|
| `"<name>"` | 指名用哪個 `Instrumentation` CR |
| `"true"` | 用同 namespace 唯一的 CR（多個就會出錯） |
| `"<namespace>/<name>"` | 跨 namespace 指名 |
| `"false"` 或空字串 | 不注入 |

**各語言注入機制的關鍵差異**（Stage 3/4 分別用到 Java 和 Python）：

| 語言 | 機制 | 需要什麼 |
|---|---|---|
| Java | initContainer 複製 `javaagent.jar` → `JAVA_TOOL_OPTIONS=-javaagent:...` | 無額外條件 |
| Python | initContainer 複製 SDK → `PYTHONPATH` + `sitecustomize.py` 自動載入 | musl libc（Alpine）要額外設 `otel-python-platform` |
| Go | **sidecar**（不是 initContainer）+ eBPF，需要 `privileged`、`ShareProcessNamespace: true` | 必填 `otel-go-auto-target-exe` |

`OTEL_SERVICE_NAME` 的推導優先序（Stage 3 §3.4 會看到它自動被填成 `order-service`）：

```
annotation resource.opentelemetry.io/service.name
  > Pod label app.kubernetes.io/name
  > 所屬 Deployment/ReplicaSet/StatefulSet/... 名稱
  > Pod 名稱 > container 名稱（最後手段）
```

**環境變數 4 層優先級**（原始容器 env > 語言專屬 env > 共用 env > Operator 自動推導值，前者定義了就不會被後者覆蓋）在 Stage 4 §4.2.1 的「留 API、去 SDK」設計裡很關鍵：payment-service 遷移後，自訂 metrics/log 全靠 Operator 注入的 agent 提供 provider，app code 完全沒改一行。

---

## 情境設定

你接手一個已經在運行的系統，現況如下：

```
                        ┌──────────────────────┐
       HTTP             │  payment-service     │
   client ───────────▶ │  (Python / Flask)    │
                        │                      │
                        │  ★ 已手動裝好：       │
                        │    - OTel SDK         │
                        │    - 自己的 sidecar   │
                        │      otel collector   │
                        │    - 自訂 metrics +   │
                        │      自訂 log         │
                        └──────────┬───────────┘
                                   │ HTTP 呼叫
                                   ▼
                        ┌──────────────────────┐
                        │  order-service       │
                        │  (Java / Spring Boot)│
                        │                      │
                        │  ✗ 完全沒有 OTel      │
                        │    (還沒裝)           │
                        └──────────┬───────────┘
                                   │ JDBC
                                   ▼
                        ┌──────────────────────┐
                        │  PostgreSQL          │
                        └──────────────────────┘
```

**現況的問題：**

| 問題 | 說明 |
|---|---|
| Python collector 各管各的 | sidecar collector 的設定散落在 app 的 manifest，要改設定得改 app deployment、重新部署 |
| Java 完全沒有 telemetry | 看不到 order-service 的 trace / metrics，跨服務的 trace 斷在 payment → order 之間 |
| 沒有統一的 tail sampling | 想要「保留 100% 採樣（先全收，之後再決定丟棄策略）」但沒有地方做 |
| 多副本 collector 無法正確 tail sampling | tail sampling 要求「同一條 trace 的所有 span 進到同一個 collector 副本」，目前沒有 span 層級的負載平衡 |
| 沒有集中管理 collector 設定的控制面 | 改一個 collector 設定要逐一手動套用 |

---

## Lab 目標（對應你提的 5 個需求）

| 階段 | 檔案 | 你會做什麼 | 對應需求 |
|---|---|---|---|
| Stage 0 | [00-setup.md](./00-setup.md) | 建 k3d cluster、用 Helm 裝 Operator（chart 自簽憑證，免 cert-manager）、build 兩個 app image | 環境準備 |
| Stage 1 | [01-baseline-apps.md](./01-baseline-apps.md) | 部署 PostgreSQL + Java(無 OTel) + Python(手動 OTel)，重現「現況」 | 重現情境 |
| Stage 2 | [02-collector-gateway-and-loadbalancer.md](./02-collector-gateway-and-loadbalancer.md) | 用 Operator 建 **gateway collector**（tail sampling：error/slow 全留 + 其餘 10%）+ **agent collector**（loadbalancing 做 span load balancer，並負責 `memory_limiter`/`k8sattributes`+RBAC/`resourcedetection` 補強）| 需求 2 |
| Stage 3 | [03-java-sidecar-and-autoinstrument.md](./03-java-sidecar-and-autoinstrument.md) | 用 Operator 替 Java 服務注入 **sidecar collector + auto-instrument**（零改動 app） | 需求 3 |
| Stage 4 | [04-python-migrate-to-operator.md](./04-python-migrate-to-operator.md) | 把 Python 手動裝的 collector + SDK 換成 **Operator 管理的 sidecar + auto-instrument** | 需求 4 |
| Stage 5 | [05-opamp-bridge-control-plane.md](./05-opamp-bridge-control-plane.md) | 用 **OpAMP Bridge**（repo 內建的 Go server）遠端管理 Operator 建立的 collector | 需求 5 |
| Stage 6（延伸） | [06-opamp-remote-version-upgrade.md](./06-opamp-remote-version-upgrade.md) | 透過 OpAMP RemoteConfig **遠端下推 collector 版本升級**（改 `spec.image`），不用 `kubectl apply` | 需求 5 延伸 |
| Stage 7（延伸） | [07-team-scoped-attributes.md](./07-team-scoped-attributes.md) | 幫 order-service 的 sidecar 加業務 attributes（`attributes` + `transform`/OTTL processor），並驗證 payment-service 完全不受影響 | 業務單位客製化屬性、CR 隔離範疇 |

每個階段結尾都有「驗證」與「練習」，跟 classroom 章節同樣格式。

---

## 最終架構（做完整個 lab 之後）

```
                            ┌────────────────────────────────────────┐
                            │       OpAMP Bridge (Go server)          │  ← Stage 5
                            │  遠端讀取 / 回報 collector 設定           │
                            └───────────────┬────────────────────────┘
                                            │ 管理
                  ┌─────────────────────────┼──────────────────────────┐
                  ▼                         ▼                          ▼
   ┌──────────────────────┐   ┌──────────────────────┐   ┌──────────────────────┐
   │ payment-service      │   │ order-service        │   │  agent collector     │
   │ (Python)             │   │ (Java)               │   │  DaemonSet           │  ← Stage 2
   │  + sidecar collector │   │  + sidecar collector │   │  接收 OTLP            │
   │  + auto-instrument   │   │  + auto-instrument   │   │  loadbalancing       │
   │   (Stage 4)          │   │   (Stage 3)          │   │  exporter            │
   └──────────┬───────────┘   └──────────┬───────────┘   └──────────┬───────────┘
              │ OTLP                      │ OTLP                     │
              └───────────────┬──────────┘                          │
                              ▼                                     │
                  (sidecar 直接送，或經 agent)                       │
                              │     routing_key: traceID            │
                              └──────────────┬──────────────────────┘
                                             ▼
                            ┌────────────────────────────────────────┐
                            │   gateway collector (StatefulSet x N)   │  ← Stage 2
                            │   tail_sampling (error/slow 全留 + 其餘 │
                            │   10%) → logs/metrics/traces → backend  │
                            └────────────────────────────────────────┘
```

**為什麼要 agent + gateway 兩層、中間放 loadbalancing exporter？**

tail sampling 是「先收集一條 trace 的所有 span，再決定整條要不要保留」。如果 gateway 有多個副本，而同一條 trace 的 span 被分散到不同副本，每個副本都只看到片段，tail sampling 就會做出錯誤決策。**loadbalancing exporter 用 trace ID 當 routing key**，保證同一條 trace 永遠送到同一個 gateway 副本——這就是你說的「otel span load balancer」。Stage 2 會詳細拆解。

---

## 目錄結構

```
classroom/lab/
├── README.md                                  ← 你正在看的這份
├── 00-setup.md  ...  07-team-scoped-attributes.md
├── apps/
│   ├── order-service/        ← Java / Spring Boot（最小可跑，連 PostgreSQL）
│   │   ├── pom.xml
│   │   ├── Dockerfile
│   │   └── src/main/...
│   └── payment-service/      ← Python / Flask（手動裝 OTel SDK）
│       ├── app.py
│       ├── requirements.txt
│       └── Dockerfile
└── manifests/
    ├── 00-namespace.yaml
    ├── 01-postgres.yaml
    ├── 02-order-service.yaml          ← Stage 1：無 OTel 的 Java
    ├── 03-payment-service.yaml        ← Stage 1：手動 OTel 的 Python（含 sidecar collector）
    ├── 10-collector-gateway.yaml      ← Stage 2：gateway + tail sampling
    ├── 11-collector-agent-lb.yaml     ← Stage 2：agent + loadbalancing
    ├── 20-instrumentation.yaml        ← Stage 3/4：Instrumentation CR
    ├── 21-order-service-instrumented.yaml  ← Stage 3：Java 注入版
    ├── 30-payment-service-operator.yaml    ← Stage 4：Python 改用 Operator
    ├── 40-opampbridge.yaml            ← Stage 5/6（Stage 6 沿用同一份 manifest，多開 admin port）
    ├── 50-order-sidecar-attributes.yaml    ← Stage 7：order team 專屬 sidecar CR（attributes + OTTL）
    ├── 51-order-service-team-sidecar.yaml  ← Stage 7：order-service 改指向 order-sidecar
    ├── 60-example-llm-guard-api-operator.yaml      ← 真實案例參考：把公司服務 llm-guard-api
    │                                                  現有的手動 sidecar+env 改寫成 Instrumentation
    │                                                  + sidecar CR（不是 k3d lab 的一部分，不要 apply）
    └── 60-example-llm-guard-api-values-after.yaml  ← 同上，改用 Operator 後 Helm values.yaml 的樣子
```

---

## 開始

從 [Stage 0：環境準備](./00-setup.md) 開始。

| | |
|---|---|
| 下一步 | [Stage 0：環境準備 →](./00-setup.md) |
