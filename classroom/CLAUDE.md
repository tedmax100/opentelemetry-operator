# classroom/ 目錄說明（給 Claude 的參考）

這個目錄是為 **OpenTelemetry Operator 程式碼導讀課程** 設計的教材，供人類學習者閱讀。

---

## 這份教材是什麼

以繁體中文寫成的結構化教材，引導讀者從 Kubernetes 基礎知識出發，逐步理解這份 Operator 的核心程式碼。

**目標讀者：** 具備 K8s 基礎（Pod、Deployment、Service），但不熟悉 Operator Pattern 或 controller-runtime 的工程師。

---

## 章節結構

| 檔案 | 主題 | 核心程式碼 |
|---|---|---|
| `00-overview.md` | 整體架構、學習路徑 | `main.go`、目錄結構 |
| `01-operator-pattern.md` | Operator Pattern、控制迴圈、`SetupWithManager` | `internal/controllers/opentelemetrycollector_controller.go` |
| `02-crd-api-types.md` | 四個 CR 的 Spec/Status 結構、kubebuilder marker | `apis/v1beta1/`、`apis/v1alpha1/` |
| `03-manifests-builder.md` | CR Spec → k8s 資源的生成（純函式、工廠模式）| `internal/manifests/collector/` |
| `04-webhook-injection.md` | Admission Webhook、Pod 注入流程 | `internal/webhook/`、`internal/instrumentation/` |
| `05-reconcile-loop.md` | Create/Update/Delete 完整實作、OwnerReference、樂觀鎖 | `internal/controllers/common.go`、`internal/manifests/mutate.go` |
| `06-auto-instrumentation.md` | 各語言 agent 注入差異、env var 優先級 | `internal/instrumentation/*.go` |
| `07-watch-informer-internals.md` | HTTP Watch 機制、Reflector/DeltaFIFO/Indexer、Predicate | `internal/controllers/targetallocator_controller.go:209-237` |
| `08-key-libraries.md` | 各 library 的職責、用途與真實程式碼範例 | `internal/controllers/common.go`、`internal/manifests/mutate.go`、`pkg/collector/upgrade/` |

---

## 寫作規範（新增或修改章節時必須遵守）

### 語言
- **全程繁體中文**，技術術語保留英文原文（如 `Reconcile`、`Predicate`、`OwnerReference`）

### 章節開頭格式
```markdown
# 第 N 章：標題

> **本章目標：** 一句話說明讀者學完這章會理解什麼

---
```

### 程式碼引用
- 所有程式碼塊必須標注來源（相對於 repo root 的路徑 + 行號）
- 格式：`` **檔案：** [`path/to/file.go`](../path/to/file.go)（第 N 行）``
- 或在程式碼塊開頭加上 `// path/to/file.go:行號` 的注釋

### 圖表
- 使用 ASCII art 繪製架構圖和流程圖
- 風格：`┌─┐│└─┘├─┤↓→←↑▼▲` 這套字元

### 練習題格式
每章結尾有「練習 N」區塊，包含：
1. **閱讀理解題**：要求讀者找到特定程式碼並解釋
2. **動手題**（選用）：可實際執行的操作
3. 每題必須有 `<details><summary>參考答案</summary>...</details>` 折疊區塊

### 不要做的事
- 不加 emoji
- 不寫「總結」大段落（用表格代替）
- 不引用不存在於這個 repo 的檔案路徑
- 不假設讀者知道 `client-go` 或 `controller-runtime` 的細節（需要時，在章節內解釋）

---

## 新增章節時的 checklist

1. 確認引用的每個檔案路徑和行號都存在於 repo 中（用 `grep` 或 `Read` 驗證）
2. 在 `00-overview.md` 的學習路徑表新增一行
3. 在 `00-overview.md` 的 Mermaid mindmap 中，選擇適合的主軸分支並加入新章節節點
4. 章節編號按序接在最後一章之後
5. 練習題要連結到真實的程式碼，不要出現虛構的函式名稱

---

## 目前已知的知識缺口（潛在的新章節）

這些主題在現有章節中被提及但未深入講解：

- **Target Allocator 內部**：Prometheus scrape target 的分配算法（`cmd/otel-allocator/`）
- **Three-way merge**：`kubectl apply` 的差異比對機制（`internal/manifests/mutate.go` 已涉及）
- **OpAMP Bridge**：遠端管理協議的完整流程（`cmd/operator-opamp-bridge/`）
- **OwnerReference vs Finalizer**：兩種清理機制的選擇時機（第 1、5 章有提及）
- **CRD Webhook 驗證**：`apis/v1beta1/opentelemetrycollector_webhook.go` 的邏輯
- **Target Allocator 用到的 library**：`buraksezer/consistent`（一致性雜湊）、`prometheus/prometheus`（服務發現）
