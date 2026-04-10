# OpenTelemetry Operator 程式碼導讀

> **前提知識：** 具備 Kubernetes 基礎（Pod、Deployment、Service、Namespace）  
> **目標：** 從零理解 Kubernetes Operator Pattern，並能讀懂這份 Operator 的核心邏輯

---

## 這個 Operator 做什麼？

OpenTelemetry Operator 是一個 Kubernetes Operator，它讓你可以：

1. **聲明式管理 OTel Collector**：用 YAML 描述你要什麼樣的 Collector，Operator 負責建立並維護對應的 Deployment/DaemonSet/StatefulSet
2. **自動注入 OTel SDK**：在應用程式 Pod 建立時，自動注入各語言（Java、Python、Node.js...）的 instrumentation agent，不需要修改應用程式程式碼
3. **管理 Target Allocator**：當多個 Collector 副本需要分工抓 Prometheus metrics 時，自動分配

---

## 整體架構

```
你寫的 CR (Custom Resource)
         │
         ▼
┌─────────────────────────────────────────────┐
│           OpenTelemetry Operator             │
│                                             │
│  ┌─────────────┐    ┌─────────────────────┐ │
│  │  Controller  │    │  Admission Webhook  │ │
│  │  (Reconcile) │    │  (Pod 注入)          │ │
│  └──────┬──────┘    └──────────┬──────────┘ │
│         │                      │            │
└─────────┼──────────────────────┼────────────┘
          │                      │
          ▼                      ▼
   k8s 資源                  修改後的 Pod
   Deployment               (加了 initContainer
   Service                   和 env vars)
   ConfigMap
   ...
```

---

## 四個核心 Custom Resource

| CR | 版本 | 用途 |
|---|---|---|
| `OpenTelemetryCollector` | v1beta1 | 管理 OTel Collector 部署 |
| `Instrumentation` | v1alpha1 | 設定自動注入的語言與 endpoint |
| `TargetAllocator` | v1alpha1 | Prometheus target 分配（獨立 CR） |
| `OpAMPBridge` | v1alpha1 | 透過 OpAMP 協議遠端管理 Collector |

---

## 專案目錄結構

```
opentelemetry-operator/
├── main.go                          ← 程式進入點，Manager 初始化
├── apis/
│   ├── v1beta1/                     ← 穩定版 CRD 定義
│   │   ├── opentelemetrycollector_types.go  ← 核心 CR
│   │   ├── common.go                ← 共用欄位 struct
│   │   └── mode.go                  ← deployment/daemonset/statefulset/sidecar
│   └── v1alpha1/                    ← 實驗版 CRD 定義
│       ├── instrumentation_types.go ← 自動注入 CR
│       ├── targetallocator_types.go ← Target Allocator CR
│       └── opampbridge_types.go     ← OpAMP Bridge CR
├── internal/
│   ├── controllers/                 ← Reconcile 邏輯
│   │   ├── opentelemetrycollector_controller.go
│   │   ├── targetallocator_controller.go
│   │   ├── opampbridge_controller.go
│   │   └── common.go                ← reconcileDesiredObjects（核心）
│   ├── manifests/                   ← CR Spec → k8s 資源 轉換
│   │   ├── collector/               ← Collector 相關資源
│   │   │   ├── collector.go         ← Build() 入口
│   │   │   ├── deployment.go        ← 生成 Deployment
│   │   │   ├── container.go         ← 生成 Container
│   │   │   ├── configmap.go         ← 生成 ConfigMap
│   │   │   └── service.go           ← 生成 Service
│   │   └── mutate.go                ← Update 時各資源的 mutate 策略
│   ├── instrumentation/             ← 自動注入邏輯
│   │   ├── podmutator.go            ← 決策層（讀 annotation）
│   │   ├── sdk.go                   ← 執行注入
│   │   ├── javaagent.go             ← Java 注入
│   │   ├── nodejs.go                ← Node.js 注入
│   │   └── python.go                ← Python 注入
│   └── webhook/
│       └── podmutation/
│           └── webhookhandler.go    ← Admission Webhook HTTP handler
└── cmd/
    ├── otel-allocator/              ← Target Allocator 獨立 binary
    └── operator-opamp-bridge/       ← OpAMP Bridge 獨立 binary
```

---

## 學習路徑

```mermaid
mindmap
  root((OTel Operator<br/>程式碼導讀))
    基礎概念
      第1章 Operator Pattern
        為什麼需要 Operator
        控制迴圈 Control Loop
        冪等性 Idempotency
        SetupWithManager
      第2章 CRD 資料模型
        OpenTelemetryCollector v1beta1
        Instrumentation v1alpha1
        TargetAllocator v1alpha1
        OpAMPBridge v1alpha1
        kubebuilder markers
    核心機制
      第3章 Manifest 生成
        Spec → k8s 資源
        純函式設計
        工廠模式 Build()
        mutate 策略
      第5章 Reconcile 迴圈
        CreateOrUpdate
        OwnerReference
        樂觀鎖衝突處理
        Finalizer 清理
    注入與觀測
      第4章 Webhook 注入
        Admission Webhook
        MutatingWebhook
        JSON Patch
        initContainer 注入
      第6章 Auto Instrumentation
        annotation 驅動
        各語言注入差異
        env var 優先級
        OTEL_SERVICE_NAME
    底層原理
      第7章 Watch 機制
        HTTP Watch 長連接
        Reflector
        DeltaFIFO
        Indexer 本地快取
        Predicate 過濾
      第8章 重要 Library
        controller-runtime
        client-go retry
        apimachinery
        mergo 合併
        semver 版本比較
        go-logr 結構化日誌
    動手實作
      第9章 從零建立 Operator
        kubebuilder 腳手架
        FooApp CRD 定義
        Reconcile 實作
        OwnerReference 清理
      第10章 部署到 k3d
        建立本機 cluster
        image import 技巧
        RBAC 驗證
        開發迭代迴圈
      第11章 升級與異動機制
        CR Spec 異動
        Operator 版本升級
        CRD Schema 升級
        樂觀鎖衝突處理
```

按照以下順序閱讀，每章都有對應的練習：

| 章節 | 檔案 | 重點 |
|---|---|---|
| [第 1 章](./01-operator-pattern.md) | Operator Pattern | 為什麼需要 Operator？控制迴圈是什麼？ |
| [第 2 章](./02-crd-api-types.md) | CRD 資料模型 | 四個 CR 的結構與設計 |
| [第 3 章](./03-manifests-builder.md) | Manifest 生成 | Spec 如何轉換成 k8s 資源 |
| [第 4 章](./04-webhook-injection.md) | Webhook 注入 | Pod 如何自動被加上 OTel agent |
| [第 5 章](./05-reconcile-loop.md) | Reconcile 迴圈 | Create / Update / Delete 的完整流程 |
| [第 6 章](./06-auto-instrumentation.md) | Auto Instrumentation 深度解析 | 各語言注入差異、env var 優先級、OTEL_SERVICE_NAME 推導 |
| [第 7 章](./07-watch-informer-internals.md) | Watch 機制與 Informer 內部 | HTTP Watch、Reflector/DeltaFIFO/Indexer 管線、Predicate 過濾 |
| [第 8 章](./08-key-libraries.md) | 重要 Library 解析 | controller-runtime、client-go、apimachinery、mergo、semver、logr |
| [第 9 章](./09-build-simple-operator.md) | 從零建立一個簡單的 Operator | kubebuilder 腳手架、FooApp CRD、Reconcile 實作、OwnerReference |
| [第 10 章](./10-deploy-to-k3d.md) | 部署 Operator 到 k3d 環境 | image import、RBAC 驗證、開發迭代迴圈 |
| [第 11 章](./11-upgrade-and-change.md) | 升級與異動機制 | CR Spec 異動、Operator 版本升級、CRD Schema 升級、樂觀鎖 |

---

## 快速定位程式碼

閱讀這份教材時，程式碼連結格式為：

```
internal/controllers/common.go:124
```

在 IDE 中可以直接 Cmd+Click（或 Ctrl+Click）跳轉。
