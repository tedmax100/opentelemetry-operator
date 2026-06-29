# 實戰 Lab：用 OpenTelemetry Operator 接管混合語言服務的可觀測性

> **這份 lab 是什麼：** classroom 前 11 章是「讀懂 Operator 程式碼」，這份 lab 是「動手把 Operator 用在一個真實情境」。你會從一個半成品的系統出發，逐步用 Operator 統一管理 OTel Collector 與 auto-instrumentation。
>
> **預期時間：** 90 ~ 150 分鐘
> **前置知識：** classroom 第 1、2、4、6 章（Operator Pattern、CRD、Webhook 注入、Auto Instrumentation）

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
                            │   tail_sampling (100%) → logs/metrics/  │
                            │   traces pipeline → backend             │
                            └────────────────────────────────────────┘
```

**為什麼要 agent + gateway 兩層、中間放 loadbalancing exporter？**

tail sampling 是「先收集一條 trace 的所有 span，再決定整條要不要保留」。如果 gateway 有多個副本，而同一條 trace 的 span 被分散到不同副本，每個副本都只看到片段，tail sampling 就會做出錯誤決策。**loadbalancing exporter 用 trace ID 當 routing key**，保證同一條 trace 永遠送到同一個 gateway 副本——這就是你說的「otel span load balancer」。Stage 2 會詳細拆解。

---

## 目錄結構

```
classroom/lab/
├── README.md                                  ← 你正在看的這份
├── 00-setup.md  ...  05-opamp-bridge-control-plane.md
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
    └── 40-opampbridge.yaml            ← Stage 5
```

---

## 開始

從 [Stage 0：環境準備](./00-setup.md) 開始。

| | |
|---|---|
| 下一步 | [Stage 0：環境準備 →](./00-setup.md) |
