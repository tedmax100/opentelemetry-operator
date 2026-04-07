# 第 4 章：Webhook 注入機制

> **本章目標：** 理解 Kubernetes Admission Webhook 的運作方式，以及 OTel Operator 如何在 Pod 建立時自動注入 agent

---

## 4.1 什麼是 Admission Webhook？

當你執行 `kubectl apply -f pod.yaml`，Pod 不是直接被建立的。中間有一個「准入控制」的流程：

```
kubectl apply
     │
     ▼
Kubernetes API Server
     │
     ├─① Mutating Admission Webhook  ← 可以修改物件
     │   （這個 Operator 就是在這裡注入 OTel agent）
     │
     ├─② Validating Admission Webhook ← 只能允許或拒絕
     │
     └─③ 物件存入 etcd，Pod 開始調度
```

**Mutating Webhook 的運作：**
1. API Server 把「即將建立的 Pod」以 JSON 格式 POST 到 Webhook Server
2. Webhook Server 修改 Pod（加入 initContainer、env var、volume）
3. 把修改後的 Pod 以 JSON Patch 格式回傳給 API Server
4. API Server 套用 patch，繼續建立流程

**關鍵：** Webhook 是同步的，在回應之前 Pod 不會建立。

---

## 4.2 Webhook 的架構

```
internal/webhook/podmutation/webhookhandler.go  ← HTTP 層
         │ 呼叫每個 PodMutator.Mutate()
         ▼
internal/instrumentation/podmutator.go          ← 決策層
  ─ 讀 annotation → 找 Instrumentation CR → 決定注入哪些語言
         │ 呼叫 sdkInjector.inject()
         ▼
internal/instrumentation/sdk.go                 ← 執行層
  ─ injectJava() / injectPython() / injectNodeJS()...
         │
         ▼
internal/instrumentation/javaagent.go           ← 各語言具體邏輯
internal/instrumentation/python.go
internal/instrumentation/nodejs.go
...
```

---

## 4.3 第一層：HTTP Handler

**檔案：** [`internal/webhook/podmutation/webhookhandler.go`](../internal/webhook/podmutation/webhookhandler.go)

```go
// 第 21 行：kubebuilder marker 定義了這個 webhook 的設定
// +kubebuilder:webhook:path=/mutate-v1-pod,mutating=true,failurePolicy=ignore,...
//
// failurePolicy=ignore 非常重要：
// 如果 Webhook Server 掛了，Pod 照常建立（只是不會有 OTel 注入）
// 不會阻擋正常的 Pod 建立流程

func (p *podMutationWebhook) Handle(ctx context.Context, req admission.Request) admission.Response {
    // 1. 解碼：JSON → Pod struct
    pod := corev1.Pod{}
    p.decoder.Decode(req, &pod)

    // 2. 取得 Pod 所在的 Namespace（含 namespace 的 annotation）
    ns := corev1.Namespace{}
    p.client.Get(ctx, types.NamespacedName{Name: req.Namespace}, &ns)

    // 3. 逐一呼叫所有 PodMutator
    //    （目前只有 instrumentation mutator，未來可以加更多）
    for _, m := range p.podMutators {
        pod, err = m.Mutate(ctx, ns, pod)
    }

    // 4. 把修改後的 Pod 跟原始 Pod 比較，計算 JSON Patch 回傳
    marshaledPod, _ := json.Marshal(pod)
    return admission.PatchResponseFromRaw(req.Object.Raw, marshaledPod)
}
```

**`PodMutator` interface（第 44-47 行）：**
```go
type PodMutator interface {
    Mutate(ctx context.Context, ns corev1.Namespace, pod corev1.Pod) (corev1.Pod, error)
}
```

任何實作這個 interface 的 struct 都可以被加入 webhook 的處理鏈，是個開放的擴展點。

---

## 4.4 第二層：決策邏輯

**檔案：** [`internal/instrumentation/podmutator.go`](../internal/instrumentation/podmutator.go)

`Mutate()` 函式的完整流程：

### 步驟 1：防止重複注入（第 218-221 行）

```go
if isAutoInstrumentationInjected(pod) {
    logger.Info("Skipping pod instrumentation - already instrumented")
    return pod, nil
}
```

### 步驟 2：逐語言讀取 annotation，找對應的 Instrumentation CR

```go
// 對每個語言重複這個模式（共 8 種語言）：
inst, err = pm.getInstrumentationInstance(ctx, ns, pod, annotationInjectJava)
// annotationInjectJava = "instrumentation.opentelemetry.io/inject-java"

if pm.config.EnableJavaAutoInstrumentation || inst == nil {
    insts.Java.Instrumentation = inst
} else {
    // operator 沒有啟用 Java 注入，記錄 warning
}
```

### `getInstrumentationInstance()` 的三種情況（第 361-386 行）

```go
func (pm *instPodMutator) getInstrumentationInstance(..., instAnnotation string) (*v1alpha1.Instrumentation, error) {
    instValue := annotationValue(ns.ObjectMeta, pod.ObjectMeta, instAnnotation)

    switch {
    case instValue == "" || instValue == "false":
        return nil, nil  // 不注入

    case instValue == "true":
        // 自動找同 namespace 的唯一 Instrumentation CR
        return pm.selectInstrumentationInstanceFromNamespace(ctx, ns)

    default:
        // instValue 是具體名稱，例如 "my-instrumentation" 或 "other-ns/my-inst"
        return pm.Client.Get(ctx, instNamespacedName, otelInst)
    }
}
```

### 步驟 3：實際注入

```go
modifiedPod = pm.sdkInjector.inject(ctx, insts, ns, modifiedPod, pm.config)
```

---

## 4.5 Annotation 優先級解析

**檔案：** [`internal/instrumentation/annotation.go`](../internal/instrumentation/annotation.go)  
**函式：** `annotationValue()`（第 38 行）

Annotation 可以設定在 **Namespace** 或 **Pod** 上，有以下優先級規則：

```
Namespace Annotation | Pod Annotation | 最終結果
─────────────────────────────────────────────────
""                   | "true"         | "true"     （Pod 設了就用 Pod 的）
"true"               | ""             | "true"     （Pod 沒設就用 Namespace 的）
"my-inst"            | "true"         | "my-inst"  （NS 更具體，覆蓋 Pod 的 true）
"true"               | "false"        | "false"    （Pod 明確拒絕，尊重 Pod）
"false"              | "true"         | "true"     （Pod 明確要求，覆蓋 NS 的 false）
"true"               | "my-other"     | "my-other" （Pod 指定具體名稱，以 Pod 為準）
```

**實際使用場景：**
```yaml
# 在 Namespace 上設定預設注入 Java
apiVersion: v1
kind: Namespace
metadata:
  name: my-app
  annotations:
    instrumentation.opentelemetry.io/inject-java: "my-instrumentation"

# 某個特定 Pod 不想被注入
apiVersion: v1
kind: Pod
metadata:
  annotations:
    instrumentation.opentelemetry.io/inject-java: "false"
```

---

## 4.6 第三層：實際注入邏輯

**檔案：** [`internal/instrumentation/sdk.go`](../internal/instrumentation/sdk.go)

```go
func (i *sdkInjector) inject(ctx context.Context, insts languageInstrumentations, ...) corev1.Pod {
    // 逐一注入各語言
    if insts.Java.Instrumentation != nil {
        pod = i.injectJava(ctx, insts.Java, ns, pod)
    }
    if insts.NodeJS.Instrumentation != nil {
        pod = i.injectNodeJS(ctx, insts.NodeJS, ns, pod)
    }
    // ...依此類推
    return pod
}
```

### 以 Java 為例，注入分兩個步驟

**步驟 A：修改每個目標 Container**（`javaagent.go:22`）

```go
func injectJavaagentToContainer(javaSpec v1alpha1.Java, container *corev1.Container) error {
    // 1. 掛載 Volume（用來存放 javaagent.jar）
    containerMountPath := fmt.Sprintf("/otel-auto-instrumentation-java-%s", container.Name)
    container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
        Name:      javaVolumeName,
        MountPath: containerMountPath,
    })

    // 2. 注入語言專屬 env（javaSpec.Env 的內容）
    container.Env = appendIfNotSet(container.Env, javaSpec.Env...)
}
```

**步驟 B：修改 Pod（加入 initContainer 和 Volume）**（`javaagent.go:43`）

```go
func injectJavaagentToPod(javaSpec v1alpha1.Java, pod corev1.Pod, ...) corev1.Pod {
    // 加入共享 Volume（emptyDir）
    pod.Spec.Volumes = append(pod.Spec.Volumes, volume)

    // 加入 init container
    initContainer := corev1.Container{
        Name:  "opentelemetry-auto-instrumentation-java",
        Image: javaSpec.Image,  // 這個 image 內含 javaagent.jar
        Command: []string{
            "cp",
            "/javaagent.jar",
            "/otel-auto-instrumentation-java/javaagent.jar",
        },
        VolumeMounts: []corev1.VolumeMount{{
            Name:      volume.Name,
            MountPath: "/otel-auto-instrumentation-java",
        }},
    }
    pod.Spec.InitContainers = append(pod.Spec.InitContainers, initContainer)
    return pod
}
```

### Init Container 的巧思

為什麼要用 init container 複製 jar，而不是直接讓主容器用 agent image？

```
因為 Kubernetes 不允許 container 直接存取其他 image 的檔案系統。

解法：
  Init Container 先跑 → 把 javaagent.jar 複製到 emptyDir Volume
  主容器後跑 → 透過 Volume mount 存取 javaagent.jar
  透過 JAVA_TOOL_OPTIONS 告訴 JVM 載入它

這個 emptyDir Volume 的生命週期跟 Pod 一樣，是暫時的。
```

### 注入後的 Pod 長這樣

```yaml
spec:
  initContainers:
    - name: opentelemetry-auto-instrumentation-java   # 新增
      image: ghcr.io/open-telemetry/.../autoinstrumentation-java:latest
      command: ["cp", "/javaagent.jar", "/otel-auto-instrumentation-java/javaagent.jar"]
      volumeMounts:
        - name: opentelemetry-auto-instrumentation-java
          mountPath: /otel-auto-instrumentation-java

  containers:
    - name: your-java-app                            # 你原本的容器，被修改了
      env:
        - name: JAVA_TOOL_OPTIONS                    # 新增
          value: " -javaagent:/otel-auto-instrumentation-java-your-java-app/javaagent.jar"
        - name: OTEL_SERVICE_NAME                    # 新增
          value: your-java-app
        - name: OTEL_EXPORTER_OTLP_ENDPOINT          # 新增
          value: http://my-collector:4317
      volumeMounts:
        - name: opentelemetry-auto-instrumentation-java  # 新增
          mountPath: /otel-auto-instrumentation-java-your-java-app

  volumes:
    - name: opentelemetry-auto-instrumentation-java  # 新增
      emptyDir:
        sizeLimit: 200Mi
```

---

## 4.7 共用 SDK 設定注入

**函式：** `injectCommonSDKConfig()`（`sdk.go` 中）

除了語言專屬的 agent，Operator 還會注入共用的 OTel SDK 設定：

```go
// 從 Instrumentation CR 轉換成 env vars
OTEL_SERVICE_NAME        ← 從 pod labels 推導（app.kubernetes.io/name 等）
OTEL_EXPORTER_OTLP_ENDPOINT ← 來自 Instrumentation.Spec.Exporter.Endpoint
OTEL_PROPAGATORS         ← 來自 Instrumentation.Spec.Propagators
OTEL_TRACES_SAMPLER      ← 來自 Instrumentation.Spec.Sampler.Type
OTEL_TRACES_SAMPLER_ARG  ← 來自 Instrumentation.Spec.Sampler.Argument
OTEL_RESOURCE_ATTRIBUTES ← 來自 Instrumentation.Spec.Resource.Attributes
```

---

## 練習 1：追蹤 annotation 解析

打開 [`internal/instrumentation/annotation.go`](../internal/instrumentation/annotation.go)，閱讀 `annotationValue()` 函式（第 38-67 行）。

**問題：** 以下情況，最終會用哪個 Instrumentation CR？

```
Namespace annotation: instrumentation.opentelemetry.io/inject-python: "default-py-inst"
Pod annotation:       instrumentation.opentelemetry.io/inject-python: "true"
```

手動追蹤 `annotationValue()` 的邏輯後回答。

<details>
<summary>參考答案</summary>

1. `nsAnnValue = "default-py-inst"`（非空）
2. `podAnnValue = "true"`（非空）
3. Pod annotation 不是空的，繼續判斷
4. `podAnnValue == "true"`，進入最後一個 if
5. `nsAnnValue` 不是 `"false"`
6. 回傳 `nsAnnValue = "default-py-inst"`

所以最終使用名為 `default-py-inst` 的 Instrumentation CR。

這個設計讓 Namespace 管理員可以設定「我這個 namespace 的預設 instrumentation 是 X」，而使用 `true` 的 Pod 會自動用這個預設值，而不是找 namespace 中唯一的 CR。
</details>

---

## 練習 2：理解 init container 的執行順序

假設一個 Pod 同時需要注入 Java 和 Python：

1. 打開 [`internal/instrumentation/sdk.go`](../internal/instrumentation/sdk.go)，看 `inject()` 函式（第 47 行）
2. `injectJava()` 和 `injectPython()` 各自會加一個 init container

**問題：** 這兩個 init container 的執行順序是什麼？有可能同時執行嗎？

<details>
<summary>參考答案</summary>

Kubernetes init container 是**依序執行**的，不會並行。先加入的先執行。

根據 `sdk.go:47-77` 的順序：Java 的 init container 先加，Python 的後加，所以執行順序是：

1. `opentelemetry-auto-instrumentation-java`（複製 javaagent.jar）
2. `opentelemetry-auto-instrumentation-python`（複製 Python SDK）
3. 主容器開始執行

不會同時執行。但因為 init container 之間是相互獨立的（各自用自己的 Volume），即使順序不同也不影響結果。
</details>

---

## 練習 3：手動觀察注入結果

Webhook 無法用 `make run` 測試（因為 operator 跑在本機，API Server 無法回呼你的 localhost）。需要把 operator 真正部署進 cluster。

### k3d 環境準備

```bash
# 建立本地 cluster（若已有 otel-lab 可跳過）
k3d cluster create otel-lab
kubectl config use-context k3d-otel-lab

# 安裝 cert-manager：Webhook Server 需要 TLS 憑證，cert-manager 負責自動簽發
make cert-manager

# 等待 cert-manager 就緒（三個 pod 都 Running 才能繼續）
kubectl rollout status deployment/cert-manager -n cert-manager
kubectl rollout status deployment/cert-manager-webhook -n cert-manager

# 用官方映像部署 Operator（不用自己 build）
IMG=ghcr.io/open-telemetry/opentelemetry-operator/opentelemetry-operator:latest make deploy

# 等待 Operator 的 controller-manager pod 就緒
kubectl rollout status deployment/opentelemetry-operator-controller-manager \
  -n opentelemetry-operator-system
```

> 部署完成後，API Server 每次建立 Pod 都會呼叫 Operator 的 Webhook，注入才會生效。

```bash
# 建立 Instrumentation CR
kubectl apply -f - <<EOF
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata:
  name: my-instrumentation
  namespace: default
spec:
  exporter:
    endpoint: http://otelcol:4317
  propagators:
    - tracecontext
    - baggage
  sampler:
    type: parentbased_always_on
EOF

# 建立一個帶 annotation 的 Pod
kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: my-java-app
  annotations:
    instrumentation.opentelemetry.io/inject-java: "true"
spec:
  containers:
    - name: app
      image: busybox
      command: ["sleep", "3600"]
EOF

# 觀察 Pod 被注入後的 spec
kubectl get pod my-java-app -o yaml | grep -A 50 initContainers
kubectl get pod my-java-app -o yaml | grep -A 20 "name: JAVA_TOOL_OPTIONS" || \
  kubectl get pod my-java-app -o yaml | grep "OTEL_"
```

記錄實際注入了哪些 env vars 和 init containers，跟本章說明的比對。

---

[← 第 3 章：Manifest 生成](./03-manifests-builder.md) | [第 5 章：Reconcile 迴圈 →](./05-reconcile-loop.md)
