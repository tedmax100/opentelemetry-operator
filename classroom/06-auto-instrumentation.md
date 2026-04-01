# 第 6 章：Auto Instrumentation 深度解析

> **本章目標：** 深入理解 `Instrumentation` CR 的完整機制——從 annotation 到 Pod 內實際注入的內容，以及各語言注入方式的差異

---

## 6.1 什麼是 Auto Instrumentation？

傳統上，要讓應用程式發出 OTel 遙測資料，需要：
1. 修改應用程式程式碼，引入 OTel SDK
2. 設定 exporter、sampler、propagator
3. 重新打包 image

**Auto Instrumentation 讓你不修改任何程式碼**，只在 Pod 加上一個 annotation，Operator 就會在 Pod 建立時自動把 OTel agent 注入進去。

---

## 6.2 整體工作流程

```
① 你建立 Instrumentation CR
   （設定 exporter endpoint、sampler、語言 image 等）
         │
         ▼
② 你在 Pod/Deployment 加上 annotation
   instrumentation.opentelemetry.io/inject-java: "true"
         │
         ▼ Pod 建立請求送到 k8s API Server
③ Admission Webhook 攔截
   ─ 讀取 annotation
   ─ 找到對應的 Instrumentation CR
   ─ 修改 Pod spec
         │
         ▼
④ 修改後的 Pod 啟動
   ─ init container 先跑，複製 agent 到共享 Volume
   ─ 主容器帶著 agent 和 OTel env vars 啟動
   ─ 應用程式自動發送 traces/metrics/logs
```

---

## 6.3 Instrumentation CR 完整結構

**檔案：** [`apis/v1alpha1/instrumentation_types.go`](../apis/v1alpha1/instrumentation_types.go)

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: my-instrumentation
spec:
  # ── 1. 遙測資料送到哪 ──────────────────────────────
  exporter:
    endpoint: http://my-collector:4317   # OTLP gRPC endpoint
    tls:                                 # 可選：TLS 設定
      secretName: my-tls-secret
      ca_file: ca.crt
      cert_file: tls.crt
      key_file: tls.key

  # ── 2. 跨服務 context 傳遞 ──────────────────────────
  propagators:
    - tracecontext   # W3C TraceContext（推薦）
    - baggage        # W3C Baggage
    # - b3           # Zipkin B3（若需要與舊系統互通）
    # - b3multi      # B3 多 header 格式

  # ── 3. 取樣策略 ─────────────────────────────────────
  sampler:
    type: parentbased_traceidratio   # 取樣器類型
    argument: "0.25"                 # 取樣 25%

  # ── 4. Resource Attributes ──────────────────────────
  resource:
    resourceAttributes:
      environment: production
      team: platform
    addK8sUIDAttributes: true        # 加入 Pod UID、Deployment UID 等

  # ── 5. 共用 Env Vars（所有語言都會注入）──────────────
  env:
    - name: OTEL_LOGS_EXPORTER
      value: otlp

  # ── 6. 各語言設定 ────────────────────────────────────
  java:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-java:latest
    env:
      - name: OTEL_INSTRUMENTATION_JDBC_ENABLED
        value: "true"
    resources:
      limits:
        cpu: 500m
        memory: 128Mi

  nodejs:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-nodejs:latest

  python:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-python:latest

  dotnet:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-dotnet:latest

  go:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-go:latest

  apacheHttpd:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-apache-httpd:latest
    version: "2.4"                   # Apache 版本

  nginx:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-nginx:latest

  # ── 7. 全域 image pull policy ───────────────────────
  imagePullPolicy: IfNotPresent
```

---

## 6.4 觸發注入的 Annotations

**檔案：** [`internal/instrumentation/annotation.go`](../internal/instrumentation/annotation.go)

### 主要注入 Annotation

| Annotation | 作用 |
|---|---|
| `instrumentation.opentelemetry.io/inject-java` | 注入 Java agent |
| `instrumentation.opentelemetry.io/inject-nodejs` | 注入 Node.js SDK |
| `instrumentation.opentelemetry.io/inject-python` | 注入 Python SDK |
| `instrumentation.opentelemetry.io/inject-dotnet` | 注入 .NET SDK |
| `instrumentation.opentelemetry.io/inject-go` | 注入 Go agent |
| `instrumentation.opentelemetry.io/inject-apache-httpd` | 注入 Apache HTTPD agent |
| `instrumentation.opentelemetry.io/inject-nginx` | 注入 Nginx agent |
| `instrumentation.opentelemetry.io/inject-sdk` | 只注入共用 SDK 設定（env vars），不注入 agent |

### Annotation 值的三種形式

```yaml
# 形式 1：true — 自動找同 namespace 唯一的 Instrumentation CR
instrumentation.opentelemetry.io/inject-java: "true"

# 形式 2：指定名稱 — 使用特定的 Instrumentation CR
instrumentation.opentelemetry.io/inject-java: "my-java-instrumentation"

# 形式 3：跨 namespace — 使用其他 namespace 的 CR
instrumentation.opentelemetry.io/inject-java: "monitoring/shared-instrumentation"

# 形式 4：false 或空字串 — 明確不注入
instrumentation.opentelemetry.io/inject-java: "false"
```

### 語言專屬額外 Annotation

```yaml
# 指定 Go 程式的執行檔路徑（Go 注入必填）
instrumentation.opentelemetry.io/otel-go-auto-target-exe: "/app/server"

# 指定 Python 平台（musl libc 環境，如 Alpine Linux）
instrumentation.opentelemetry.io/otel-python-platform: "musl"

# 指定 .NET runtime
instrumentation.opentelemetry.io/otel-dotnet-auto-runtime: "linux-x64"
```

---

## 6.5 Multi-Container 注入

一個 Pod 可能有多個容器（`app`、`sidecar`...），你可以精確控制注入哪個容器：

### 方式 1：所有語言注入同一批容器

```yaml
# 把 Java 和 Python 都注入到 app container
annotations:
  instrumentation.opentelemetry.io/inject-java: "true"
  instrumentation.opentelemetry.io/inject-python: "true"
  instrumentation.opentelemetry.io/container-names: "app"
```

### 方式 2：不同語言注入不同容器（需開啟 Multi-Instrumentation feature gate）

```yaml
annotations:
  instrumentation.opentelemetry.io/inject-java: "true"
  instrumentation.opentelemetry.io/inject-python: "true"
  # Java 注入 java-app 容器
  instrumentation.opentelemetry.io/java-container-names: "java-app"
  # Python 注入 python-worker 容器
  instrumentation.opentelemetry.io/python-container-names: "python-worker"
```

**程式碼來源：** [`internal/instrumentation/podmutator.go`](../internal/instrumentation/podmutator.go)（第 147-192 行）

---

## 6.6 各語言的注入機制差異

這是本章最重要的部分。每個語言的注入方式都有差異：

### 共同模式（Java、Node.js、Python、.NET）

```
init container 複製 agent → emptyDir Volume
主容器掛載 Volume，透過 env var 載入 agent
```

### Java

**檔案：** [`internal/instrumentation/javaagent.go`](../internal/instrumentation/javaagent.go)

```
注入的 init container:
  名稱: opentelemetry-auto-instrumentation-java
  動作: cp /javaagent.jar /otel-auto-instrumentation-java/javaagent.jar

注入的 env vars:
  JAVA_TOOL_OPTIONS: -javaagent:/otel-auto-instrumentation-java-{container}/javaagent.jar
  （若 JAVA_TOOL_OPTIONS 已存在，則在後面 append，不是覆蓋）

支援 Java Extensions:
  java:
    extensions:
      - image: my-extension:latest
        dir: /extensions
  → 額外的 init container 複製 extension jar
```

### Node.js

**檔案：** [`internal/instrumentation/nodejs.go`](../internal/instrumentation/nodejs.go)

```
注入的 init container:
  名稱: opentelemetry-auto-instrumentation-nodejs
  動作: cp -r /autoinstrumentation/. /otel-auto-instrumentation-nodejs/

注入的 env vars:
  NODE_OPTIONS: --require /otel-auto-instrumentation-nodejs/autoinstrumentation.js
  （若 NODE_OPTIONS 已存在，同樣是 append）
```

### Python

**檔案：** [`internal/instrumentation/python.go`](../internal/instrumentation/python.go)

```
注入的 init container:
  名稱: opentelemetry-auto-instrumentation-python
  動作: cp -r /autoinstrumentation/. /otel-auto-instrumentation-python/

注入的 env vars:
  PYTHONPATH: /otel-auto-instrumentation-python:$PYTHONPATH
  OTEL_TRACES_EXPORTER: otlp
  OTEL_METRICS_EXPORTER: otlp
  OTEL_EXPORTER_OTLP_PROTOCOL: grpc

特殊情況 - musl libc（Alpine Linux）：
  annotation: instrumentation.opentelemetry.io/otel-python-platform: "musl"
  → 使用不同的 PYTHONPATH（/otel-auto-instrumentation-python/lib/）
```

### .NET

**檔案：** [`internal/instrumentation/dotnet.go`](../internal/instrumentation/dotnet.go)

```
注入的 init container:
  名稱: opentelemetry-auto-instrumentation-dotnet

注入的 env vars:
  CORECLR_ENABLE_PROFILING: 1
  CORECLR_PROFILER: {918728DD-259F-4A6A-AC2B-B85E1B658318}
  CORECLR_PROFILER_PATH: /otel-auto-instrumentation-dotnet/linux-x64/OpenTelemetry.AutoInstrumentation.Native.so
  DOTNET_ADDITIONAL_DEPS: /otel-auto-instrumentation-dotnet/AdditionalDeps
  DOTNET_SHARED_STORE: /otel-auto-instrumentation-dotnet/store
  DOTNET_STARTUP_HOOKS: /otel-auto-instrumentation-dotnet/net/OpenTelemetry.AutoInstrumentation.StartupHook.dll

注意：CORECLR_PROFILER_PATH 依 runtime 有不同路徑
  annotation: instrumentation.opentelemetry.io/otel-dotnet-auto-runtime: "linux-musl-x64"
```

### Go（特殊：Sidecar 模式，而非 initContainer）

**檔案：** [`internal/instrumentation/golang.go`](../internal/instrumentation/golang.go)

Go 的注入機制與其他語言**完全不同**：

```
其他語言：initContainer 複製 agent → 主容器用 env var 載入
Go：     新增一個 sidecar container，用 eBPF 追蹤主 process
```

```
注入的 sidecar container（不是 init container！）:
  名稱: opentelemetry-auto-instrumentation
  SecurityContext:
    runAsUser: 0        ← 需要 root
    privileged: true    ← 需要特權模式
  VolumeMounts:
    - mountPath: /sys/kernel/debug  ← 掛載 kernel debug filesystem
      name: kernel-debug

必要條件：
  pod.Spec.ShareProcessNamespace = true  ← 自動設定，讓 sidecar 能看到主 process

必填 annotation（否則注入會被跳過）:
  instrumentation.opentelemetry.io/otel-go-auto-target-exe: "/app/server"
```

**為什麼 Go 這麼特別？**

Java/Python/Node.js 都有「agent hook」機制（JVM agent、PYTHONPATH preload、Node `--require`），可以在 process 啟動時注入。Go 是編譯型語言，沒有這種機制，所以改用 **eBPF**（Linux kernel 的沙盒執行環境）在 kernel 層面追蹤 syscall，不需要修改程式碼。這需要特權和 kernel debug filesystem 存取。

### Apache HTTPD 和 Nginx（設定檔注入）

**檔案：** [`internal/instrumentation/apachehttpd.go`](../internal/instrumentation/apachehttpd.go)、[`internal/instrumentation/nginx.go`](../internal/instrumentation/nginx.go)

這兩個不是透過 env vars，而是**修改 web server 的設定檔**來啟用 OTel module：

```
注入的 init container:
  1. clone container：複製原本的 Apache/Nginx config
  2. agent container：複製 OTel module 並修改 config

注入的內容：
  - 在 httpd.conf / nginx.conf 加入 LoadModule 指令
  - 設定 OTel exporter endpoint、service name 等
```

---

## 6.7 共用的 SDK 設定注入

**函式：** `injectCommonSDKConfig()`（[`internal/instrumentation/sdk.go`](../internal/instrumentation/sdk.go) 第 416 行）

所有語言注入完 agent 後，都會執行這個函式，注入共用的 OTel 設定：

```
注入的 env vars（依序，若已存在則跳過）：

OTEL_SERVICE_NAME        ← 自動推導（見下方）
OTEL_EXPORTER_OTLP_ENDPOINT ← 來自 Instrumentation.Spec.Exporter.Endpoint
OTEL_RESOURCE_ATTRIBUTES ← 彙整所有 resource attributes
OTEL_PROPAGATORS         ← 來自 Instrumentation.Spec.Propagators
OTEL_TRACES_SAMPLER      ← 來自 Instrumentation.Spec.Sampler.Type
OTEL_TRACES_SAMPLER_ARG  ← 來自 Instrumentation.Spec.Sampler.Argument

以及 k8s metadata（透過 Downward API）：
OTEL_RESOURCE_ATTRIBUTES_NODE_NAME  ← spec.nodeName
OTEL_RESOURCE_ATTRIBUTES_POD_NAME   ← metadata.name
OTEL_RESOURCE_ATTRIBUTES_POD_IP     ← status.podIP

（若 addK8sUIDAttributes: true）
OTEL_RESOURCE_ATTRIBUTES_POD_UID    ← metadata.uid
```

**重要：** `OTEL_RESOURCE_ATTRIBUTES` 永遠被移到 env var 列表的最後一個（`sdk.go:515-522`），確保它引用的其他 env var（如 `$(OTEL_RESOURCE_ATTRIBUTES_NODE_NAME)`）在它之前被展開。

---

## 6.8 OTEL_SERVICE_NAME 的推導邏輯

**函式：** `chooseServiceName()`（[`internal/instrumentation/sdk.go`](../internal/instrumentation/sdk.go) 第 528 行）

`OTEL_SERVICE_NAME` 是最重要的 resource attribute，決定 trace 在監控系統裡顯示的服務名稱。推導優先級（高到低）：

```
1. Pod annotation:  resource.opentelemetry.io/service.name
2. Pod label:       app.kubernetes.io/name（需開啟 useLabelsForResourceAttributes）
                 或 app.kubernetes.io/instance
3. 所屬 Deployment 的名稱
4. 所屬 ReplicaSet 的名稱
5. 所屬 StatefulSet 的名稱
6. 所屬 DaemonSet 的名稱
7. 所屬 CronJob 的名稱
8. 所屬 Job 的名稱
9. Pod 名稱
10. Container 名稱（最後手段）
```

**對應程式碼（sdk.go:529-555）：**

```go
func chooseServiceName(pod corev1.Pod, useLabelsForResourceAttributes bool, resources map[string]string, container *corev1.Container) string {
    // 優先用 annotation
    if name := chooseLabelOrAnnotation(pod, ..., semconv.ServiceNameKey, constants.LabelAppName); name != "" {
        return name
    }
    // 然後找 workload 名稱（Operator 從 k8s API 查詢 ownerReference 取得）
    if name := resources[string(semconv.K8SDeploymentNameKey)]; name != "" {
        return name
    }
    // ...依此類推
    return container.Name  // 最後手段
}
```

**實際建議：** 在 Deployment 加上 `app.kubernetes.io/name` label，並開啟 `useLabelsForResourceAttributes: true`，這樣 service name 會自動對應到 Deployment 名稱。

---

## 6.9 Env Var 的 4 層優先級（詳細版）

```
高優先級
  │
  │  Layer 1（最高）：原始容器自己的 env
  │    → 使用者在 Deployment spec 裡寫的 env
  │    → 永遠不會被 Operator 覆蓋
  │
  │  Layer 2：語言專屬 env
  │    → Java.Env、Python.Env、NodeJS.Env...
  │    → 只注入到對應語言的容器
  │
  │  Layer 3：共用 env
  │    → Instrumentation.Spec.Env
  │    → 注入到所有被注入的容器
  │
  │  Layer 4（最低）：Operator 自動推導的值
  │    → OTEL_SERVICE_NAME（從 Deployment 名稱推導）
  │    → OTEL_EXPORTER_OTLP_ENDPOINT（從 Exporter.Endpoint）
  │    → POD_NAME、NODE_NAME 等 k8s metadata
  │
  ↓  （只有上層沒有設定這個 env，下層才會生效）
低優先級
```

**程式碼來源：** [`internal/instrumentation/sdk.go`](../internal/instrumentation/sdk.go) `injectCommonEnvVar()`（第 354 行）和 `injectCommonSDKConfig()`（第 416 行）都用了 `appendIfNotSet()`，確保已存在的 env var 不被覆蓋。

---

## 6.10 TLS 設定

當 Collector 使用 HTTPS 時，需要設定 TLS：

**檔案：** [`internal/instrumentation/exporter.go`](../internal/instrumentation/exporter.go)

```yaml
spec:
  exporter:
    endpoint: https://my-collector:4317
    tls:
      secretName: my-tls-secret     # 含 tls.crt 和 tls.key
      configMapName: my-ca-bundle   # 含 CA 憑證（可選）
      ca_file: ca.crt
      cert_file: tls.crt
      key_file: tls.key
```

Operator 會：
1. 把 Secret 和 ConfigMap 掛載成 Volume 到容器內
2. 設定 env vars：
   - `OTEL_EXPORTER_OTLP_CERTIFICATE` → CA 憑證路徑
   - `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` → 客戶端憑證路徑
   - `OTEL_EXPORTER_OTLP_CLIENT_KEY` → 私鑰路徑

---

## 6.11 isAutoInstrumentationInjected() — 防止重複注入

**函式：** [`internal/instrumentation/helper.go`](../internal/instrumentation/helper.go)（第 34 行）

Webhook 每次 Pod 建立都會被呼叫。如何防止已注入過的 Pod 被重複注入？

```go
func isAutoInstrumentationInjected(pod corev1.Pod) bool {
    // 方法 1：檢查是否已有 OTel init container
    for _, cont := range pod.Spec.InitContainers {
        if slices.Contains([]string{
            "opentelemetry-auto-instrumentation-java",
            "opentelemetry-auto-instrumentation-nodejs",
            "opentelemetry-auto-instrumentation-python",
            "opentelemetry-auto-instrumentation-dotnet",
            // ...
        }, cont.Name) {
            return true
        }
    }

    // 方法 2：檢查是否已有 Go sidecar
    for _, cont := range pod.Spec.Containers {
        if cont.Name == "opentelemetry-auto-instrumentation" {
            return true
        }
    }

    // 方法 3：檢查是否有 NODE_NAME env var（共用 SDK 設定的標誌）
    for _, cont := range pod.Spec.Containers {
        for _, envVar := range cont.Env {
            if envVar.Name == "OTEL_RESOURCE_ATTRIBUTES_NODE_NAME" {
                return true
            }
        }
    }
    return false
}
```

---

## 6.12 完整範例：一個 Java 應用的注入前後對比

### 注入前（你寫的 Deployment）

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: order-service
  labels:
    app.kubernetes.io/name: order-service
spec:
  template:
    metadata:
      annotations:
        instrumentation.opentelemetry.io/inject-java: "true"
    spec:
      containers:
        - name: app
          image: my-company/order-service:1.0
          ports:
            - containerPort: 8080
          resources:
            limits:
              cpu: 500m
              memory: 512Mi
```

### 注入後（Webhook 修改的 Pod spec）

```yaml
spec:
  # ① 新增的 init container
  initContainers:
    - name: opentelemetry-auto-instrumentation-java
      image: ghcr.io/open-telemetry/.../autoinstrumentation-java:latest
      command: ["cp", "/javaagent.jar", "/otel-auto-instrumentation-java/javaagent.jar"]
      resources:
        limits:
          cpu: 500m
          memory: 128Mi   # 來自 Instrumentation.Spec.Java.Resources
      volumeMounts:
        - name: opentelemetry-auto-instrumentation-java
          mountPath: /otel-auto-instrumentation-java
      securityContext:
        runAsUser: 1000   # 從主容器的 securityContext 複製

  containers:
    - name: app
      image: my-company/order-service:1.0
      # ② 新增的 env vars（不覆蓋原有的）
      env:
        - name: OTEL_RESOURCE_ATTRIBUTES_NODE_IP   # 透過 Downward API
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        - name: OTEL_RESOURCE_ATTRIBUTES_POD_IP    # 透過 Downward API
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        - name: JAVA_TOOL_OPTIONS
          value: " -javaagent:/otel-auto-instrumentation-java-app/javaagent.jar"
        - name: OTEL_SERVICE_NAME
          value: order-service          # 從 app.kubernetes.io/name label 推導
        - name: OTEL_EXPORTER_OTLP_ENDPOINT
          value: http://my-collector:4317
        - name: OTEL_PROPAGATORS
          value: tracecontext,baggage
        - name: OTEL_TRACES_SAMPLER
          value: parentbased_traceidratio
        - name: OTEL_TRACES_SAMPLER_ARG
          value: "0.25"
        - name: OTEL_RESOURCE_ATTRIBUTES_NODE_NAME  # 透過 Downward API
          valueFrom:
            fieldRef:
              fieldPath: spec.nodeName
        - name: OTEL_RESOURCE_ATTRIBUTES_POD_NAME   # 透過 Downward API
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: OTEL_RESOURCE_ATTRIBUTES             # 永遠在最後！
          value: "environment=production,team=platform,k8s.pod.name=$(OTEL_RESOURCE_ATTRIBUTES_POD_NAME),k8s.node.name=$(OTEL_RESOURCE_ATTRIBUTES_NODE_NAME)"
      # ③ 新增的 volumeMount
      volumeMounts:
        - name: opentelemetry-auto-instrumentation-java
          mountPath: /otel-auto-instrumentation-java-app

  # ④ 新增的 volume
  volumes:
    - name: opentelemetry-auto-instrumentation-java
      emptyDir:
        sizeLimit: 200Mi   # 預設值，可用 Java.VolumeSizeLimit 覆蓋
```

---

## 練習 1：閱讀理解 — Go 的特殊需求

打開 [`internal/instrumentation/golang.go`](../internal/instrumentation/golang.go)，看 `injectGoSDK()` 函式（第 23 行）。

**問題 1：** 第 25-27 行有一個早期返回：
```go
if pod.Spec.ShareProcessNamespace != nil && !*pod.Spec.ShareProcessNamespace {
    return pod, errors.New("shared process namespace has been explicitly disabled")
}
```
為什麼 Go instrumentation 需要 `ShareProcessNamespace`？如果使用者明確設為 false，注入會怎樣？

**問題 2：** 為什麼 `OTEL_GO_AUTO_TARGET_EXE` 是 Go instrumentation 的必填項目（`sdk.go:199-204`）？

<details>
<summary>參考答案</summary>

**問題 1：** Go instrumentation 用 eBPF sidecar 追蹤主 process。要看到另一個 container 的 process，必須共用 process namespace（`ShareProcessNamespace: true`）。如果明確設為 false，注入會失敗並 return 原本的 pod（不注入），並記錄 log。

**問題 2：** eBPF 需要知道要追蹤哪個執行檔。Go 應用編譯後是單一 binary，Operator 無法自動猜測路徑。`OTEL_GO_AUTO_TARGET_EXE` 告訴 eBPF agent 要 hook 哪個 binary。沒有這個值，agent 不知道要追蹤什麼，注入沒有意義。
</details>

---

## 練習 2：追蹤 OTEL_RESOURCE_ATTRIBUTES 的組成

當 Instrumentation CR 設定了：
```yaml
resource:
  resourceAttributes:
    environment: production
    team: platform
  addK8sUIDAttributes: true
```

`OTEL_RESOURCE_ATTRIBUTES` 最終的值長什麼樣？

追蹤 `injectCommonSDKConfig()` 中的 `resourceMap` 是如何建立和組合的（`sdk.go:416` 附近）。

<details>
<summary>參考答案</summary>

最終 `OTEL_RESOURCE_ATTRIBUTES` 大約是：

```
environment=production,team=platform,k8s.pod.name=$(OTEL_RESOURCE_ATTRIBUTES_POD_NAME),k8s.node.name=$(OTEL_RESOURCE_ATTRIBUTES_NODE_NAME),k8s.pod.uid=$(OTEL_RESOURCE_ATTRIBUTES_POD_UID),k8s.namespace.name=default,k8s.deployment.name=order-service,...,service.version=1.0
```

組成來源：
1. `Instrumentation.Spec.Resource.Attributes`（手動設定的）
2. 從 k8s API 查詢的 workload 資訊（Deployment name、Namespace 等）
3. 透過 Downward API env var 設定的動態值（Pod name、Node name 用 `$(ENV_VAR)` 引用）
4. `service.version`（從 `app.kubernetes.io/version` label 推導）

`OTEL_RESOURCE_ATTRIBUTES` 被故意放在最後，確保 `$(ENV_VAR)` 格式的引用在 k8s 展開時能找到對應的 env var。
</details>

---

## 練習 3：寫一個完整的 Instrumentation + Deployment YAML

設計一個場景：

- 你有一個 Python FastAPI 服務，跑在 Alpine Linux（musl libc）
- 想注入 OTel instrumentation
- 服務資料送到 `http://otelcol.monitoring:4317`
- 取樣 10%
- 加上 `environment=staging` resource attribute
- 只注入 `api` container（Pod 裡還有一個 `nginx` container 不需要注入）

寫出：
1. Instrumentation CR
2. Deployment（含正確的 annotations）

<details>
<summary>參考答案</summary>

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: python-staging
spec:
  exporter:
    endpoint: http://otelcol.monitoring:4317
  propagators:
    - tracecontext
    - baggage
  sampler:
    type: parentbased_traceidratio
    argument: "0.1"
  resource:
    resourceAttributes:
      environment: staging
  python:
    image: ghcr.io/open-telemetry/opentelemetry-operator/autoinstrumentation-python:latest
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fastapi-service
spec:
  selector:
    matchLabels:
      app: fastapi-service
  template:
    metadata:
      labels:
        app: fastapi-service
        app.kubernetes.io/name: fastapi-service
      annotations:
        # 注入 Python，指定只注入 api container
        instrumentation.opentelemetry.io/inject-python: "python-staging"
        instrumentation.opentelemetry.io/python-container-names: "api"
        # musl libc 環境
        instrumentation.opentelemetry.io/otel-python-platform: "musl"
    spec:
      containers:
        - name: api
          image: my-company/fastapi-service:1.0
          ports:
            - containerPort: 8000
        - name: nginx
          image: nginx:alpine
          ports:
            - containerPort: 80
```
</details>

---

## 練習 4：手動觀察各語言的差異

如果你有 cluster，分別注入 Java 和 Go，比較兩者的 Pod spec 差異：

```bash
# 建立 Instrumentation CR
kubectl apply -f classroom/examples/instrumentation.yaml  # 自己建立

# 注入 Java
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-java
  annotations:
    instrumentation.opentelemetry.io/inject-java: "true"
spec:
  containers:
    - name: app
      image: busybox
      command: ["sleep", "3600"]
EOF

# 注入 Go（需要提供 target exe）
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: test-go
  annotations:
    instrumentation.opentelemetry.io/inject-go: "true"
    instrumentation.opentelemetry.io/otel-go-auto-target-exe: "/app/server"
spec:
  containers:
    - name: app
      image: busybox
      command: ["sleep", "3600"]
EOF

# 比較
kubectl get pod test-java -o yaml | grep -E "(initContainers|containers|volumes)" -A 20
kubectl get pod test-go -o yaml | grep -E "(initContainers|containers|volumes)" -A 20
```

觀察並記錄兩者的差異：
- init container vs sidecar container
- 注入的 env vars 有什麼不同
- volumes 有什麼不同

---

[← 第 4 章：Webhook 注入](./04-webhook-injection.md) | [← 回目錄](./00-overview.md)
