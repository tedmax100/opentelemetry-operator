# 分享大綱（35 分鐘）：為什麼用 Operator，以及往 AIOps 的下一步

> **對象：** 平台團隊 + 開發團隊混合聽眾，具備基本 K8s 概念，不要求熟悉 controller-runtime。
> **形式：** 35 分鐘，含約 5 分鐘 QA（實際講述抓 28-30 分鐘）。
> **核心命題：** Operator 解決「怎麼讓開發團隊低成本接入 OTel、平台團隊集中治理」的問題；但治理不能止於「有資料」，AIOps 需要「資料長得一致」——這是 Weaver 在 CI 階段要補的那塊。
> **取材：** 本 repo [classroom 教材](./00-overview.md)（[00](./00-overview.md)、[02](./02-crd-api-types.md)、[04](./04-webhook-injection.md)、[06](./06-auto-instrumentation.md)、[07](./07-watch-informer-internals.md)）、[互動版治理分享](./sharing-otel-operator-governance-interactive.html)、書籍《Observability Engineering》[中文版線上讀](https://tedmax100.github.io/observability-engineering-2e-zh-tw/)、參考影片 [cCGNs7nyutU](https://youtu.be/cCGNs7nyutU)（K8s webhook/injection 實作細節）。

---

## 時間軸總覽

| 時間 | 段落 | 內容 |
|---|---|---|
| 0:00 – 0:04 | 開場問題框架 | 企業導入 OTel 的兩個真實痛點 |
| 0:04 – 0:12 | 為什麼用 Operator | 手動接 SDK 的成本 vs Operator 的兩種介入點 |
| 0:12 – 0:20 | Instrumentation 怎麼運作 | annotation → webhook → 自動注入的機制與 K8s 細節 |
| 0:20 – 0:24 | OpAMP：把治理往前推一步 | 從「apply CR」到「遠端下推設定與升級」 |
| 0:24 – 0:31 | Weaver：AIOps 需要的標準化 | 為什麼有 Operator 還不夠；CI 階段做 schema 檢查 |
| 0:31 – 0:35 | 收尾 + QA | 一張全景圖、可以先做的三件事 |

---

## 0. 開場：兩個真實痛點（4 min）

不要用「Operator Pattern 是什麼」開場——聽眾要先感覺到痛，才會在意解法。

**痛點一：導入 SDK 對開發團隊是重活。**
每個語言的 OTel SDK 有自己的初始化、依賴版本、agent 掛載方式；一個 50 個服務的公司，要嘛開發團隊各自摸索（結果是每個服務的 SDK 版本、resource attribute 命名都不一樣），要嘛平台團隊逐一手把手教，兩者都不 scale。

**痛點二：即使都有資料，平台團隊管不動。**
Collector 設定散在各服務自己的 Helm values 裡（sampling rate、遮罩規則、export 目的地）。想全公司調一次 sampling，等於要動所有服務的部署。

一句話破題：**Operator 想解決「怎麼讓開發團隊低成本擁有可觀測性」，而不是解決「資料本身有沒有意義」——後者是 Weaver 要接手的部分**，這也是這場分享要走到的地方。

---

## 1. 為什麼用 Operator（8 min）

### 1.1 不用 Operator 會怎樣

沒有 Operator，平台團隊的選擇只有兩種，都不好：

- **文件 + 範本**：開發團隊複製貼上別人的 sidecar YAML 和 SDK 初始化程式碼，版本升級時沒有回頭路，覆蓋率永遠殘缺。
- **手動介入每個 PR**：平台團隊變成瓶頸。

### 1.2 Operator 提供的兩種介入點

Operator 不是「一個」東西，是兩個獨立運作的元件，各自對應治理的一個面向：

| | Collector 本體（Deployment/DaemonSet/StatefulSet） | 開發團隊的服務（自動注入） |
|---|---|---|
| CR | `OpenTelemetryCollector`（v1beta1） | `Instrumentation`（v1alpha1） |
| 機制 | Reconcile 迴圈，持續收斂 | Mutating Webhook，Pod 建立時注入 |
| 平台團隊做什麼 | 集中管一份 collector 拓撲、sampling、遮罩、路由 | 定義好「標準的接入方式」讓開發團隊選用 |
| 開發團隊做什麼 | 不用管 | 在自己的 Pod 加一行 annotation |

參考：[classroom 第 1 章](./01-operator-pattern.md)（Operator Pattern、Reconcile）、[第 3 章](./03-manifests-builder.md)（CR → k8s 資源）。

### 1.3 對開發團隊而言，接入長這樣

```yaml
metadata:
  annotations:
    instrumentation.opentelemetry.io/inject-java: "true"
```

不用改程式碼、不用自己管 agent jar、不用自己兜 `OTEL_*` env。平台團隊在 `Instrumentation` CR 裡預先定好 agent 版本、endpoint、propagator、sampler——**这一层就是治理生效的地方**：換 collector endpoint、升 agent 版本，是改一個 CR，不是發 50 個 PR。

---

## 2. Instrumentation 怎麼運作：K8s 細節（8 min）

這段對應參考影片提到的「hook 怎麼做、inject 怎麼做」，用本 repo 的真實程式碼講清楚,而不是抽象帶過。

### 2.1 Mutating Admission Webhook 是什麼

K8s 在 Pod 被建立（`CREATE`，不含 `UPDATE`）時，允許外部服務攔截並修改該物件——這就是 Admission Webhook。Operator 註冊了一個 `MutatingWebhookConfiguration`，K8s API server 在寫入 etcd 前，先把 Pod 物件 POST 給 Operator，Operator 回傳一份 JSON Patch。

關鍵細節：`failurePolicy=ignore`（[`internal/webhook/podmutation/webhookhandler.go`](../internal/webhook/podmutation/webhookhandler.go)）——webhook 不可用時 Pod 照建，只是不會被注入。這是刻意的設計：**可觀測性元件不該擋住業務部署**，代價是覆蓋率要用量測驗證，不能假設「有 annotation 就一定有注入」。

### 2.2 注入實際做了什麼

以 Java 為例，注入不是修改應用程式碼，而是三個 JSON Patch 操作：

1. 加一個 `initContainer`，把 agent jar 從 operator 自帶的 image 複製到一個共用 `emptyDir` volume
2. Mount 那個 volume 到主 container
3. 加 env var，例如 `JAVA_TOOL_OPTIONS=-javaagent:/otel-auto-instrumentation/javaagent.jar`

程式邏輯：[`internal/instrumentation/podmutator.go`](../internal/instrumentation/podmutator.go)（決策層，讀 annotation）→ [`internal/instrumentation/sdk.go`](../internal/instrumentation/sdk.go)（執行注入）→ 各語言檔案如 [`internal/instrumentation/javaagent.go`](../internal/instrumentation/javaagent.go)。

### 2.3 這個機制的邊界（治理上要知道的事）

- **注入是「烤進去」的，不是持續收斂的。** Pod 建立後，config 已經寫死在 Pod spec 裡，改 `Instrumentation` CR 不會讓已存在的 Pod 更新——要等下一次自然的 rollout（deployment 滾動更新、pod 被刪重建）。這跟 Collector 本體「改 CR 立刻 reconcile」的生效語意完全不同。
- **env var 有優先順序**：使用者自己設的 env var 永遠贏過 operator 注入的值——這是刻意讓開發團隊仍可覆寫個別設定（詳見 [第 6 章](./06-auto-instrumentation.md)）。

---

## 3. OpAMP：把治理往前推一步（4 min）

前面講的都還是「Kubernetes 內部」——平台團隊改 CR，操作對象是 in-cluster 的 collector。OpAMP（Open Agent Management Protocol）把控制面拉到 cluster 外：

- 一個中央 OpAMP server 可以對多個 cluster、多個環境（例如 dev/staging/prod）的 collector 群，統一下推設定變更、觸發滾動升級、蒐集 collector 自身的健康狀態。
- 本 repo 的 `OpAMPBridge`（v1alpha1）CR 是這個協議在 operator 這端的落地：它代理「這個 cluster 裡由 operator 管的 collector」給遠端 server，遠端下推的設定變更，會**回寫成 CR 的變更**，實際生效還是走 operator 原本的 reconcile 迴圈——不是繞過 K8s 的旁門機制。

治理意義：不用登入每個 cluster 手動 apply，policy 變更是「從一個地方推、多處生效」。這對多環境、多 cluster 的組織是關鍵的規模化能力。

---

## 4. Weaver：AIOps 需要的標準化（7 min）

### 4.1 為什麼 Operator 還不夠

Operator 解決的是「有沒有資料、資料怎麼流」——它不管資料的**語意**。實務上常見的狀況：同一個「使用者 ID」欄位，A 服務叫 `user_id`，B 服務叫 `userId`，C 服務塞在 `attributes.user.id`。人眼看 dashboard 還能將就，但如果目標是讓 AIOps agent 去讀多個服務的 trace/log 做根因分析、自動關聯，**欄位命名不一致 = agent 沒有共同的世界觀，推理就會斷掉**。

這正是《Observability Engineering》裡談 semantic conventions 的核心論點（可對照[書中相關章節](https://tedmax100.github.io/observability-engineering-2e-zh-tw/)）：可觀測性的價值不是「有沒有 telemetry」，是「telemetry 能不能被系統性地查詢與關聯」。Operator 讓資料「進得來」，不保證資料「講同一種語言」。

### 4.2 Weaver 做什麼

OpenTelemetry Weaver 是官方的 schema 工具鏈：用 YAML 定義 semantic convention（哪些 attribute 該叫什麼名字、什麼型別、哪些是必填），可以：

- 從 schema 產生型別安全的程式碼（各語言的常數/enum），減少手打字串出錯
- **在 CI 階段檢查** 一個服務實際送出的 telemetry schema 是否符合公司定義的 convention

### 4.3 落在流程裡的位置

```
開發團隊寫 code
      │
      ▼
CI pipeline ──▶ weaver check（比對送出的 telemetry schema vs 公司 registry）
      │                                │
      │ 不合規 → fail build，附上哪個欄位不對
      ▼
merge / deploy
      │
      ▼
Operator 負責的部分：注入 SDK、collector 收資料、路由到後端
      │
      ▼
AIOps agent 讀資料：因為 schema 一致，才能跨服務關聯、自動推理
```

**Operator 和 Weaver 是互補、不是取代關係**：Operator 保證「大家都有資料、資料流向對」，Weaver 保證「資料長得一樣」。前者是接入層的治理，後者是語意層的治理，AIOps 兩者都要。

---

## 5. 收尾（4 min）

### 5.1 全景圖

```
                     ┌─────────────────────────┐
                     │   平台團隊定義的標準       │
                     │  (Instrumentation CR +   │
                     │   Weaver semantic conv.) │
                     └───────────┬──────────────┘
                                 │
              ┌──────────────────┼──────────────────┐
              ▼                                      ▼
     開發團隊：Pod 加 annotation             CI：weaver check schema
              │                                      │
              ▼                                      ▼
     Operator webhook 自動注入                 不合規擋在 merge 前
              │
              ▼
     Collector（Operator 管、OpAMP 可遠端下推設定）
              │
              ▼
     後端 + AIOps agent（因為 schema 一致，能跨服務推理）
```

### 5.2 可以先做的三件事

1. 先讓一個既有服務改用 `Instrumentation` annotation 接入，量測「注入覆蓋率」而不是假設它 100%。
2. 把現有 collector Helm values 裡的 `config` 原封不動搬進 `OpenTelemetryCollector` CR 的 `spec.config`（透傳設計，遷移成本低）。
3. 先在一到兩個 semantic convention 高風險欄位（例如 `service.name`、使用者識別欄位）試跑 weaver check，不用一次覆蓋所有 attribute。

### 5.3 QA 引導問題（備用）

- 「Instrumentation 的注入是 best-effort，那我們怎麼知道哪些 Pod 漏注了？」→ 帶到覆蓋率量測、`failurePolicy=ignore` 的取捨。
- 「OpAMP 跟我們直接改 CR 有什麼差？」→ 帶到多 cluster/多環境規模化的差異。
- 「Weaver 檢查失敗要卡 CI 嗎？」→ 帶到漸進式導入（warning → blocking）的路徑討論。
