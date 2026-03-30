# OTel Operator 實戰學習 Lab

用 k3d 在本機搭建一個完整的 OpenTelemetry 學習環境，涵蓋 operator 的四個核心 CRD，以及各種 collector 部署模式。

## 業務場景

**訂單系統**，兩個微服務互動：

```
用戶請求
    │
    ▼
order-service (Python/FastAPI)
    ├── 寫入 PostgreSQL 17（訂單資料）
    └── 發布 RabbitMQ（order.created event）
            │
            ▼
    inventory-service (Java/Spring Boot)
            └── 消費 RabbitMQ → 扣庫存 → 寫入 PostgreSQL 17
```

這個場景可以同時觀察：HTTP trace、DB span、MQ span，適合展示 distributed tracing 的完整價值。

## 可觀測性後端

| 功能 | 工具 |
|------|------|
| Traces | Grafana Tempo |
| Metrics | Prometheus |
| Dashboards | Grafana |
| Collector | OpenTelemetry Collector（由 operator 管理）|

## 課程模組

| 模組 | 主題 | 學習重點 |
|------|------|---------|
| [Module 0](./module-0-setup.md) | 環境準備 | k3d、cert-manager、operator、後端安裝 |
| [Module 1](./module-1-auto-instrumentation.md) | Auto-Instrumentation | Python/Java 零改動注入，init container 機制 |
| [Module 2](./module-2-collector-deployment.md) | Collector — Deployment 模式 | 集中式 gateway，pipeline 設計 |
| [Module 3](./module-3-collector-daemonset.md) | Collector — DaemonSet 模式 | Node agent，host metrics 收集 |
| [Module 4](./module-4-collector-sidecar.md) | Collector — Sidecar 模式 | Per-pod collector，log 收集 |
| [Module 5](./module-5-target-allocator.md) | TargetAllocator | Prometheus scrape 負載分配 |
| [Module 6](./module-6-opamp.md) | OpAMP Bridge | 遠端動態管理 collector config |

## 建議學習路徑

```
Module 0 → Module 1 → Module 2 → Module 3 → Module 5 → Module 6
                                     │
                                     └── Module 4（選修，理解 sidecar 取捨）
```

## 目錄結構（完成後）

```
docs/lab/
├── README.md               # 本文件
├── module-0-setup.md
├── module-1-auto-instrumentation.md
├── module-2-collector-deployment.md
├── module-3-collector-daemonset.md
├── module-4-collector-sidecar.md
├── module-5-target-allocator.md
├── module-6-opamp.md
└── manifests/              # 各模組所需 YAML
    ├── namespace.yaml
    ├── module-1/
    ├── module-2/
    ├── module-3/
    ├── module-4/
    ├── module-5/
    └── module-6/
```

## 前置需求

本機需要安裝：
- [k3d](https://k3d.io) v5+
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- [helm](https://helm.sh/docs/intro/install/) v3+
- Docker（k3d 依賴）
- 建議記憶體：8GB 以上
