# 分享：用 OpenTelemetry Operator 治理全公司的可觀測性

> **對象：** 平台工程團隊，以及會被我們「治理」到的業務開發團隊
> **形式：** 深入淺出的分享，約 40-50 分鐘
> **核心命題：** Operator 不只是「幫你裝 Collector 的工具」，它是平台團隊對可觀測性做治理的**控制面**——標準由平台定義、由機器強制執行，業務團隊只碰一個 annotation。
> **取材：** 本 repo 的 [classroom 導讀教材](./00-overview.md)與[實戰 lab](./lab/README.md)，所有程式碼引用皆可在 repo 中找到。

---

## 0. 先講結論（TL;DR）

沒有 Operator 的世界，可觀測性治理靠「文件 + 宣導 + 求人」：

| 治理需求 | 沒有 Operator | 有 Operator |
|---|---|---|
| 所有服務都要有 trace | 寫文件求各團隊自己接 SDK | Pod 加一個 annotation，Webhook 自動注入 |
| SDK / agent 版本統一 | 追著 50 個 repo 發 PR | 改一個 `Instrumentation` CR |
| 統一 sampling、控制後端成本 | 各團隊 collector 設定散落各處 | 平台擁有的 gateway 集中做 tail sampling |
| 敏感資料遮罩（SQL 參數、PII） | 期待每個團隊自律 | pipeline 上的 OTTL processor，出不了 cluster |
| Collector 版本升級 | 逐一通知、逐一改 manifest | Operator 升級時自動升所有 CR，或走 OpAMP 遠端下推 |
| 有人手改了線上設定 | 沒人發現，直到出事 | Reconcile 迴圈幾秒內改回來（drift correction） |

一句話：**把「靠人遵守的規範」變成「靠控制迴圈強制的狀態」。**

---

## 1. 三十秒懂 Operator Pattern

Operator = 自訂資源（CR）+ 一個不停跑的控制迴圈：

```
        你宣告的期望狀態                    實際狀態
   ┌─────────────────────┐          ┌──────────────────┐
   │  OpenTelemetryCollector │          │ Deployment        │
   │  spec:                │  比較差距  │ ConfigMap         │
   │    mode: statefulset  │─────────▶│ Service           │
   │    replicas: 2        │  補上差距  │ ServiceAccount ...│
   └─────────────────────┘          └──────────────────┘
              ▲                              │
              └──────── 持續觀察、永不停止 ────┘
```

**檔案：** [`internal/controllers/opentelemetrycollector_controller.go`](../internal/controllers/opentelemetrycollector_controller.go)（第 231-293 行）

```go
func (r *OpenTelemetryCollectorReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    var instance v1beta1.OpenTelemetryCollector
    r.Get(ctx, req.NamespacedName, &instance)            // 讀期望狀態
    desiredObjects, _ := BuildCollector(params)          // 算出「應該有哪些資源」
    ownedObjects, _ := r.findOtelOwnedObjects(ctx, params) // 查「目前實際有哪些」
    reconcileDesiredObjects(...)                         // Create/Update/Delete 補差距
    ...
}
```

對治理而言，這個迴圈給了兩個別的工具給不了的性質：

1. **宣告式**：平台政策長成 YAML（CR），可以進 Git、可以 review、可以 diff。
2. **持續強制**：不是「部署當下套用一次」，而是**一直**比對。有人 `kubectl edit` 手改 Operator 管的 Deployment，下一輪 Reconcile 就改回來。政策不會默默漂移。

想深入：classroom [第 1 章](./01-operator-pattern.md)、[第 5 章](./05-reconcile-loop.md)。

---

## 2. 治理視角：CR 是平台團隊與業務團隊之間的 API

這個 Operator 提供四個 CR。與其記「它們是什麼功能」，不如記「它們各治理什麼」：

| CR | 治理的東西 | 誰擁有 |
|---|---|---|
| `OpenTelemetryCollector` | telemetry **管線拓撲與資料政策**（sampling、遮罩、路由、資源上限） | 平台團隊（gateway/agent）；業務團隊可持有自己的 sidecar CR（見 §6） |
| `Instrumentation` | **監測標準**：SDK/agent 版本、上報 endpoint、propagator、sampler | 平台團隊 |
| `OpAMPBridge` | Collector 群的**遠端控制面**：讀取/下推設定、升級 | 平台團隊 |
| `TargetAllocator` | Prometheus scrape target 在多副本間的分配 | 平台團隊 |

業務團隊呢？他們的介面小到只剩 **Pod template 上的 annotation**：

```yaml
annotations:
  instrumentation.opentelemetry.io/inject-java: "true"   # 要自動儀器化
  sidecar.opentelemetry.io/inject: "app-sidecar"         # 要 sidecar collector
```

這就是治理設計的第一原則：**平台擁有政策（CR），業務團隊只表達意圖（annotation）。** 介面越窄，標準越不容易被繞過；同時業務團隊也不需要學會 Collector 的 300 個設定項。

想深入：classroom [第 2 章](./02-crd-api-types.md)。

---

## 3. 政策如何被「強制」：兩個執行點

平台訂的標準要落地，靠的是 Kubernetes 的兩個掛載點。理解它們，就理解了這套治理的能與不能。

### 3.1 執行點一：Admission Webhook（Pod 建立當下）

```
kubectl apply ──▶ API Server ──▶ Mutating Webhook ──▶ etcd ──▶ 排程
                                （Operator 在這裡
                                  注入 agent/sidecar/env）
```

Pod 在**真正誕生之前**就被改好了：initContainer 掛上 `javaagent.jar`、`JAVA_TOOL_OPTIONS`/`PYTHONPATH` 設好、`OTEL_SERVICE_NAME` 依規則推導出來。應用 image 一個 byte 都沒變——這是「零改 code 接入標準」的機制基礎。

**檔案：** [`internal/webhook/podmutation/webhookhandler.go`](../internal/webhook/podmutation/webhookhandler.go)（第 21 行）

```go
// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,
//   groups="",resources=pods,verbs=create,versions=v1,name=mpod.kb.io,...
```

這一行 marker 藏了兩個治理上必須知道的決策：

- **`failurePolicy=ignore`**：Operator 掛掉時，Pod 照樣建立，只是**不會被注入**。這是「可用性優先於政策完整性」的取捨——observability 元件故障不該擋下業務部署。代價是：注入是 best-effort，平台要自己監控「該被注入卻沒被注入」的 Pod，而不是假設 100% 覆蓋。
- **`verbs=create`**：webhook 只看 Pod 的 **CREATE**，沒有 UPDATE。所以改了 `Instrumentation` 或 sidecar CR，**已經在跑的 Pod 完全不受影響**，要等 Pod 重建才拿到新政策。政策更新的生效時間 = 各服務下一次 rollout 的時間（見 §7 陷阱 1）。

### 3.2 執行點二：Reconcile 迴圈（持續）

Webhook 管「Pod 出生時」，Reconcile 管「之後的每一天」。Operator 直接擁有的資源（gateway 的 StatefulSet、agent 的 DaemonSet、ConfigMap...）由控制迴圈持續對齊 CR：

- 改 CR 的 `replicas`，Operator 只 patch StatefulSet 的 `spec.replicas`，其他欄位不動。
- 手改 Operator 管的資源，會被改回——**configuration drift 在機制層面被消滅**，而不是靠 code review 抓。
- 刪除 CR，OwnerReference 讓 Kubernetes GC 自動清掉所有衍生資源，不留孤兒。

想深入：classroom [第 4 章](./04-webhook-injection.md)（webhook 三層架構）、[第 5 章](./05-reconcile-loop.md)（mutate 策略與樂觀鎖）、[第 6 章](./06-auto-instrumentation.md)（各語言注入差異、env var 四層優先級）。

---

## 4. 治理場景一：管線拓撲與成本治理（agent + gateway）

平台團隊最實際的痛：**後端儲存的帳單**。tail sampling（看完整條 trace 再決定留不留：error/慢請求全留、其餘留 10%）是控成本的標準解，但它有一個分散式難題——同一條 trace 的所有 span 必須進到**同一個** collector 副本，否則每個副本都只看到片段、做出錯誤決策。

lab Stage 2 的解法是平台層的標準拓撲：

```
  各服務的 sidecar / SDK
          │ OTLP
          ▼
  ┌─────────────────────┐
  │ agent (DaemonSet)    │  每節點一份：memory_limiter、
  │ loadbalancing        │  k8sattributes、resourcedetection
  │ exporter             │  ← routing_key: traceID
  └─────────┬───────────┘
            │ 同一 traceID 永遠送同一副本
            ▼
  ┌─────────────────────┐
  │ gateway (StatefulSet)│  tail_sampling：error/slow 全留
  │        x N           │  其餘 10% → 後端
  └─────────────────────┘
```

治理重點：

- **sampling 政策只存在一個地方**（gateway CR），改政策 = 改一份 YAML + Git PR，而不是通知 50 個團隊。
- **k8sattributes、環境標籤在 agent 層統一補**，業務團隊不必（也不能）自己亂標。
- gateway 用 `statefulset` mode 是因為 loadbalancing resolver 需要 headless service 的穩定 DNS——`spec.mode` 一個欄位，Operator 幫你長出正確的資源組合。

---

## 5. 治理場景二：儀器化標準與版本治理

### 5.1 `Instrumentation` CR = 全公司的儀器化規格書

一份 CR 集中定義：各語言 agent 的 image 與版本、上報 endpoint、propagators、sampler。50 個服務指向同一份 CR，**升級 Java agent = 改 CR 裡一個 image tag**。

值得記的細節：`Instrumentation` CR 本身「什麼都不做」，不建立任何 Pod——它是一份被 webhook 讀取的範本，annotation 才是觸發。所以它可以放在平台的 namespace、由平台 RBAC 保護，業務團隊唯讀。

### 5.2 Operator 自己也治理版本：CR 的自動升級

Operator 升級時，會主動把 cluster 裡所有 CR 從舊版 schema/預設值遷移到新版：

**檔案：** [`pkg/collector/upgrade/upgrade.go`](../pkg/collector/upgrade/upgrade.go)（第 44-51 行）

```go
func (u VersionUpgrade) NeedsUpgrade(instance v1beta1.OpenTelemetryCollector) bool {
    return instance.Status.Version != "" &&
        instance.Status.Version != u.Version.OpenTelemetryCollector &&
        instance.Spec.ManagementState != v1beta1.ManagementStateUnmanaged &&
        instance.Spec.UpgradeStrategy != v1beta1.UpgradeStrategyNone
}
```

平台可以用兩個欄位控制「治理的力度」，這是給例外情況的安全閥：

| 欄位 | 值 | 效果 |
|---|---|---|
| `spec.upgradeStrategy` | `automatic`（預設）/ `none` | 要不要讓 Operator 升級時自動遷移這個 CR |
| `spec.managementState` | `managed`（預設）/ `unmanaged` | Operator 是否接管這個 CR 的 reconcile |

預設版本由 repo 根目錄的 [`versions.txt`](../versions.txt) 統一釘住（collector、各語言 agent、target allocator...），這就是「平台發佈一個 Operator 版本 = 發佈一整組經過驗證的元件版本」。

### 5.3 更進一步：OpAMP 控制面（lab Stage 5/6）

`OpAMPBridge` 讓你有一個遠端 server 能**讀取並下推** Operator 管的 collector 設定——lab Stage 6 示範了不碰 `kubectl` 、從控制面遠端改 `spec.image` 完成 collector 版本升級。對多 cluster 的平台團隊，這是把治理面從「每個 cluster 的 GitOps」再拉高一層的路。

想深入：classroom [第 11 章](./11-upgrade-and-change.md)、lab [Stage 5](./lab/05-opamp-bridge-control-plane.md)、[Stage 6](./lab/06-opamp-remote-version-upgrade.md)。

---

## 6. 治理場景三：資料治理與租戶隔離（誰能改到誰）

### 6.1 CR 的邊界 = 爆炸半徑的邊界

lab Stage 7 用一個真實需求示範：order team 想在自己的 span 加 `team`、`cost_center` 標籤並遮罩 SQL 參數，但 payment-service **一個 byte 都不能變**。

錯誤做法：直接改兩個服務共用的 `app-sidecar` CR → payment-service 的 span 也長出 `team=order-team`。**共用 CR 的修改，波及所有指到它的 Pod。**

正確做法：複製一份 `order-sidecar` CR，只改 order-service 的 annotation 指過去：

```
   order-service Pod                payment-service Pod
   inject: "order-sidecar"          inject: "app-sidecar"
          │                                │
          ▼                                ▼
   order-sidecar CR                 app-sidecar CR
   （order team 的客製）             （平台預設，不動）
```

治理原則：**CR 的共用範圍要對齊「誰的變更誰負責」的組織邊界，不是共用越多越好。** 六個團隊需求相同就共用一份；需求會分岔的，現在就拆開。

### 6.2 資料政策放在 pipeline，不是放在信任裡

敏感資料遮罩做在 collector 的 processor，資料**出不了 cluster 就已經被處理**：

```yaml
transform/order_team:
  trace_statements:
    - context: span
      statements:
        - replace_pattern(attributes["db.statement"],
            "(?i)VALUES\\s*\\([^)]*\\)", "VALUES (***redacted***)")
          where attributes["db.statement"] != nil
```

選型的經驗法則（lab Stage 7 §7.8）：固定 key/value 用 `attributes` processor 就好；**條件判斷、regex 遮罩、跨欄位運算才上 `transform`（OTTL）**。合規更嚴的場景，用 `delete_key` 整個刪除屬性，比遮罩更保守。

### 6.3 RBAC 分層

落到權限設計，一個可行的起點：

| 角色 | 可以做什麼 |
|---|---|
| 平台團隊 | 安裝/升級 Operator 與 CRD；擁有 gateway/agent CR、`Instrumentation` CR、`OpAMPBridge` |
| 業務團隊 | 在自己 namespace 的 workload 上加 annotation；（進階）擁有自己的 sidecar CR |
| 誰都不行 | 直接改 Operator 管理的 Deployment/ConfigMap——改了也會被 reconcile 改回去 |

---

## 7. 治理的邊界：四個必須知道的陷阱

深入淺出的「深入」部分——這些是實際跑過 lab 才會撞到的，每一個都是治理假設的破口：

1. **sidecar 政策更新不是即時的。** sidecar 的 config 是 webhook 在 Pod **建立當下**烤進去的（[`pkg/sidecar/pod.go`](../pkg/sidecar/pod.go) 第 26-35 行），改 CR 對跑著的 Pod 無效，必須等（或觸發）rollout。平台下推政策後，要有「多少 % 的 Pod 已拿到新政策」的可見性，而不是 apply 完就當作生效。相對地，gateway/agent 這種 Operator 直接擁有的 workload，改 CR 會立刻觸發真正的 rolling update——**兩種模式的政策生效語意不同**。
2. **`failurePolicy=ignore` 意味著注入是 best-effort。** Operator webhook 不可用的那段時間建立的 Pod，安靜地沒有 telemetry。需要配套的偵測（例如定期盤點「有 annotation 但沒有 initContainer 的 Pod」）。
3. **共用 CR 是隱形的耦合。** §6.1 的教訓：改共用範本前，先回答「有多少 Pod 指向它、分屬哪些團隊」。
4. **Regex/OTTL 政策要拿真實資料驗證。** lab 實測：Hibernate 產生的 SQL 是小寫 `values`，沒加 `(?i)` 的遮罩規則**靜默不匹配**——語法全對、collector 不報錯、資料照漏。資料治理規則上線前，必須用真實流量驗證命中。

---

## 8. 落地路徑：brownfield 怎麼遷移

現實中沒有綠地。lab 的情境就是為此設計的——一個「Python 服務手動裝了 SDK + 自管 sidecar、Java 服務什麼都沒有」的半成品系統：

| 階段 | 動作 | 對業務團隊的成本 |
|---|---|---|
| 1 | 平台先立標準拓撲：gateway + agent（Stage 2） | 零 |
| 2 | 沒有 telemetry 的服務：加 annotation 接入（Stage 3，Java） | 改兩行 annotation |
| 3 | 手動裝過 OTel 的服務：拆掉自管 sidecar 與 SDK 初始化，換成 Operator 注入（Stage 4，Python） | 一次性遷移，「留 API、去 SDK」——自訂 metrics/log 的 API 呼叫不用改，provider 由注入的 agent 提供 |
| 4 | 需要控制面再上 OpAMP（Stage 5/6） | 零 |
| 5 | 業務客製需求用專屬 sidecar CR 隔離（Stage 7） | 各團隊自理 |

repo 裡也有一份對照真實服務的遷移範例（`llm-guard-api` 從手動 sidecar+env 改寫成 `Instrumentation` + sidecar CR）：[`lab/manifests/60-example-llm-guard-api-operator.yaml`](./lab/manifests/60-example-llm-guard-api-operator.yaml)。

**遷移論證的核心：** 遷移前，collector 設定散落在 app 的 Helm values / manifest 裡，改設定 = 改 app 部署；遷移後，設定收斂到 CR，**app 的部署與 observability 政策解耦**——這正是平台工程要的介面。

---

## 9. 帶走的三句話

1. **CR 是合約**：平台擁有政策（CR），業務團隊表達意圖（annotation），介面窄才守得住標準。
2. **治理靠迴圈，不靠自律**：webhook 在 Pod 出生時執行政策，reconcile 在之後的每一秒守住政策。
3. **知道機制的邊界**：sidecar 政策生效要等 rollout、注入是 best-effort、共用 CR 有爆炸半徑——治理設計要把這些寫進 runbook,而不是被它們 surprise。

---

## 附錄：想深入時往哪走

| 你想深入的問題 | 去讀 |
|---|---|
| Reconcile 迴圈到底怎麼寫的 | classroom [第 1](./01-operator-pattern.md)、[5 章](./05-reconcile-loop.md) |
| 四個 CR 的完整欄位 | classroom [第 2 章](./02-crd-api-types.md) |
| Webhook 注入的三層架構 | classroom [第 4 章](./04-webhook-injection.md) |
| 各語言注入差異、env var 優先級、`OTEL_SERVICE_NAME` 推導 | classroom [第 6 章](./06-auto-instrumentation.md) |
| CR/Operator/CRD 三種升級的異動機制 | classroom [第 11 章](./11-upgrade-and-change.md) |
| 從零到有動手跑一遍（90-150 分鐘） | [lab/README.md](./lab/README.md) Stage 0-7 |
| tail sampling + span load balancer 實作 | lab [Stage 2](./lab/02-collector-gateway-and-loadbalancer.md) |
| 業務客製與隔離的完整實驗 | lab [Stage 7](./lab/07-team-scoped-attributes.md) |
