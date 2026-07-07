# Stage 7：業務單位客製化 attributes（不動其他服務）

> **本階段目標：** order team 想在自己的 span 上多加業務標籤、遮罩敏感的 SQL 參數，但這個要求**只能影響 order-service**——payment-service 的 telemetry 一個 byte 都不能變，`Instrumentation` CR、agent、gateway 也都不能動。你會先示範「改共用設定會波及別人」的錯誤做法，再用「複製一份專屬 CR」修正它，並學會 `attributes` processor 與 `transform`(OTTL) processor 該怎麼選。

---

## 7.1 現況：兩個服務共用同一個 sidecar CR

Stage 3 的 order-service 與 Stage 4 遷移後的 payment-service，annotation 都指向同一個 `app-sidecar`：

```bash
kubectl -n otel-lab get deploy order-service payment-service \
  -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.template.metadata.annotations.sidecar\.opentelemetry\.io/inject}{"\n"}{end}'
# 預期兩行都印出：
# order-service: app-sidecar
# payment-service: app-sidecar
```

```
   order-service Pod              payment-service Pod
   annotation: app-sidecar        annotation: app-sidecar
          │                              │
          └──────────────┬───────────────┘
                          ▼
                 OpenTelemetryCollector
                    app-sidecar CR           ← 兩邊共用同一份「範本」
                 （20-instrumentation.yaml）
```

business ask：order team 想要

1. order-service 送出的每個 span 都加上 `team=order-team`、`cost_center=CC-4821`（財務歸屬用）。
2. order-service 寫進 DB 的 JDBC span 裡的 `db.statement`，把 SQL 參數部分遮罩掉，只留樣板。
3. 這兩件事**只能發生在 order-service 身上**，payment-service 不該多任何一個屬性。

---

## 7.2 反面教材：直接改共用的 app-sidecar

先做一次「錯誤示範」，讓你親眼看到共用 CR 的代價。臨時把 `team` 屬性加進 `app-sidecar`（**先別碰 order-sidecar，這步只是實驗，等下會復原**）：

```bash
kubectl apply -f - <<'EOF'
apiVersion: opentelemetry.io/v1beta1
kind: OpenTelemetryCollector
metadata:
  name: app-sidecar
  namespace: otel-lab
spec:
  mode: sidecar
  image: otel/opentelemetry-collector-contrib:0.153.0
  config:
    receivers:
      otlp:
        protocols:
          grpc: { endpoint: 0.0.0.0:4317 }
          http: { endpoint: 0.0.0.0:4318 }
    processors:
      batch: {}
      attributes/leak_test:
        actions:
          - key: team
            value: "order-team"
            action: insert
    exporters:
      otlp/agent:
        endpoint: agent-collector.otel-lab.svc.cluster.local:4317
        tls: { insecure: true }
    service:
      pipelines:
        traces:
          receivers: [otlp]
          processors: [attributes/leak_test, batch]
          exporters: [otlp/agent]
        metrics:
          receivers: [otlp]
          processors: [batch]
          exporters: [otlp/agent]
        logs:
          receivers: [otlp]
          processors: [batch]
          exporters: [otlp/agent]
EOF
```

> **關鍵一步，容易漏掉：** sidecar 的 config 是 Mutating Webhook 在 **Pod 建立當下**讀取 CR、烤進 container 的 `OTEL_CONFIG` 環境變數裡（見 [`pkg/sidecar/pod.go`](../../pkg/sidecar/pod.go) 第 26-35 行）——webhook 只註冊在 Pod 的 **CREATE** 事件，沒有 UPDATE。也就是說**改 CR 對已經在跑的 Pod 完全沒有作用**，只有「下一個被建立的 Pod」才會拿到新設定。order-service/payment-service 的 Deployment 又不是 Operator 管的，CR 改了不會有任何東西自動幫你重建 Pod。所以這裡一定要手動觸發一次 rollout，舊 Pod 才會被換成帶新 config 的版本：
>
> ```bash
> kubectl -n otel-lab rollout restart deployment/order-service deployment/payment-service
> kubectl -n otel-lab rollout status deployment/order-service
> kubectl -n otel-lab rollout status deployment/payment-service
> ```

打兩邊的流量，用 Stage 3 的 `gwlogs` 函式看 gateway（沒有的話重新定義一次：`gwlogs() { for p in $(kubectl -n otel-lab get pods -l app.kubernetes.io/instance=otel-lab.gateway -o name); do kubectl -n otel-lab logs "$p" --since="${1:-20s}" 2>/dev/null; done; }`）：

```bash
curl -s -X POST "http://localhost:5000/pay?item=probe&amount=1" >/dev/null; sleep 14

gwlogs 20s | grep -B6 'team: Str(order-team)' | grep -E 'Name +:|team:'
# 預期：POST /pay（payment-service 自己的 span）也印出 team: Str(order-team)
#   ↑ payment-service 明明什麼都沒改，卻因為共用 app-sidecar 被迫多了這個屬性
```

**復原**（改回 Stage 3/4 原本的 `app-sidecar`，**一樣要 rollout restart 才會生效**）：

```bash
kubectl apply -f manifests/20-instrumentation.yaml
kubectl -n otel-lab rollout restart deployment/order-service deployment/payment-service
kubectl -n otel-lab rollout status deployment/order-service
kubectl -n otel-lab rollout status deployment/payment-service
```

> 這正是「其他不動」失敗的原因：sidecar collector 的 config 是**以 CR 為單位**共用的，改 CR 就是改給所有指到它的 Pod。要精準只影響一個服務，範疇必須切到「一個服務一個 CR」。
>
> **順便看到 sidecar 模式的另一個限制：** 跟 Stage 2 的 `gateway`/`agent`（Operator 直接擁有的 Deployment/StatefulSet/DaemonSet，改 CR 會被 reconcile 自動 patch 進工作負載、觸發真正的 rolling update）不同，sidecar 的「更新」從來不是即時的——CR 只是「範本」，什麼時候套用完全取決於「這個 Pod 什麼時候被重建」。7.6 等一下改 `order-service` annotation 之所以會生效，是因為那次改的是 **Deployment 的 Pod template**（annotation 值變了），k8s 自己就會滾動重建 Pod；如果只改 sidecar CR 本身（像這裡的示範），就必須自己補一次 `rollout restart`。

---

## 7.3 正確做法：複製一份專屬 CR，只改 order-service 的 annotation

完整檔案：[`manifests/50-order-sidecar-attributes.yaml`](./manifests/50-order-sidecar-attributes.yaml)。跟 `app-sidecar` 的差異只在 `processors` 多兩個區塊、CR 改名為 `order-sidecar`；`receivers`/`exporters` 完全沒變：

```yaml
metadata:
  name: order-sidecar        # 新 CR，跟 app-sidecar 分開
spec:
  config:
    processors:
      batch: {}
      attributes/order_team: { ... }      # 7.4
      transform/order_team: { ... }       # 7.5
    service:
      pipelines:
        traces:
          processors: [attributes/order_team, transform/order_team, batch]
```

接著只改 order-service 的 annotation（[`manifests/51-order-service-team-sidecar.yaml`](./manifests/51-order-service-team-sidecar.yaml)，跟 Stage 3 的 `21-order-service-instrumented.yaml` 唯一差別是這一行）：

```yaml
annotations:
  sidecar.opentelemetry.io/inject: "order-sidecar"   # 原本是 "app-sidecar"
```

`payment-service` 的 manifest（`30-payment-service-operator.yaml`）**完全不用碰**，它的 annotation 還是 `app-sidecar`。

---

## 7.4 用 `attributes` processor 加固定業務標籤

```yaml
attributes/order_team:
  actions:
    - key: team
      value: "order-team"
      action: insert
    - key: cost_center
      value: "CC-4821"
      action: insert
```

- `action: insert`：key 不存在才寫入，不會覆蓋 app 自己已經設定的同名屬性（相對地 `upsert` 是不管存不存在都覆蓋、`update` 是只在已存在時才覆蓋）。
- 這種「不管什麼 span，一律加同一組固定 key/value」的需求，`attributes` processor 就是最簡單的工具——不需要條件判斷、不需要字串運算。
- 想在 Resource 層級（等同於整個服務共用，不分 span）加同樣的東西，可以用 `resource` processor，語法幾乎一樣，差別只在作用範圍。

---

## 7.5 用 `transform` processor（OTTL）做條件式加值與遮罩

`attributes` processor 做不到兩件事：**依條件判斷**、**用 regex 做字串處理**。這兩件事都需要 OTTL（OpenTelemetry Transformation Language）：

```yaml
transform/order_team:
  error_mode: ignore
  trace_statements:
    - context: span
      statements:
        - set(attributes["order.endpoint_group"], "checkout") where name == "POST /orders"
        - replace_pattern(attributes["db.statement"], "(?i)VALUES\\s*\\([^)]*\\)", "VALUES (***redacted***)") where attributes["db.statement"] != nil
```

逐行拆解：

| 部分 | 意思 |
|---|---|
| `context: span` | 這組 statements 要套用在「span」層級（其他可選：`resource`、`scope`、`metric`、`datapoint`、`log`） |
| `error_mode: ignore` | 單一 statement 執行出錯時（例如某個 span 根本沒有這個屬性）只跳過該 statement，不影響整批資料 |
| `set(attributes["order.endpoint_group"], "checkout") where name == "POST /orders"` | **條件式加值**：只對 span name 剛好是 `"POST /orders"` 的 span 加這個衍生屬性，其他 span（例如 order-service 對 DB 的 JDBC span）不受影響 |
| `replace_pattern(attributes["db.statement"], "(?i)VALUES\\s*\\([^)]*\\)", "VALUES (***redacted***)") where attributes["db.statement"] != nil` | **regex 遮罩**：把 `db.statement` 裡 `VALUES(...)` 整段換成固定字串。不管 Java agent 有沒有先把參數值換成 `?`（預設的 statement sanitizer 行為），這個 pattern 都吃得到，能一致地擋掉「萬一真的洩漏明碼參數值」的風險 |

**為什麼這兩件事 `attributes` processor 辦不到：**
- `attributes` processor 的 `action` 只有 insert/update/upsert/delete/hash/extract/convert，`where` 級別的條件只能用 `include`/`exclude` 做粗粒度的 match（例如整條 pipeline 只套用在某些 span name），沒辦法「在同一個 processor 裡，對不同 span 套用不同邏輯」。
- 沒有正規表示式取代功能，遮罩、脫敏、字串截斷這類需求做不到。

> **兩個容易踩雷的細節（親自跑過 lab 才會發現）：**
> 1. **OTTL 字串的跳脫規則跟 YAML 是兩層獨立的東西。** 上面這行是一個「沒加引號的 YAML 純量」，YAML 完全不做跳脫處理，OTTL 收到的是你打的原始文字；但 OTTL 自己的字串字面值用的是類似 Go 的跳脫規則（底層用 `strconv.Unquote`），`\s`、`\(` 這種寫法對 OTTL 來說不是合法的跳脫序列，**必須寫成兩個反斜線**（`\\s`、`\\(`）才會被 OTTL 還原成 regex 引擎看到的單一 `\s`、`\(`。少一層會在 collector 啟動時直接報 `invalid quoted string ... invalid syntax` 而整個 sidecar crash-loop。
> 2. **regex 預設區分大小寫。** order-service 用的是 Spring Data JPA/Hibernate，實際產生的 SQL 是**小寫**的 `values (?,?,?)`，不是教材常見手寫 SQL 習慣的大寫 `VALUES`。一開始沒加 `(?i)` 的版本在本機 regex 測試看起來沒問題，一部署到真的 order-service 就完全不遮罩——因為 pattern 裡的 `VALUES` 永遠比對不上小寫的 `values`，`replace_pattern` 沒有錯誤、就是靜默不匹配。這就是為什麼最終版本開頭多了 `(?i)`（不分大小寫）。**教訓：regex-based 的 OTTL 規則一定要拿真實資料源跑過一次，不要只看語法能不能過。**

---

## 7.6 套用，並確認只有 order-service 變了

```bash
kubectl apply -f manifests/50-order-sidecar-attributes.yaml
kubectl apply -f manifests/51-order-service-team-sidecar.yaml
kubectl -n otel-lab rollout status deployment/order-service
```

先確認 annotation 只有 order-service 變了：

```bash
kubectl -n otel-lab get deploy order-service payment-service \
  -o jsonpath='{range .items[*]}{.metadata.name}{": "}{.spec.template.metadata.annotations.sidecar\.opentelemetry\.io/inject}{"\n"}{end}'
# 預期：
# order-service: order-sidecar     ← 只有它變了
# payment-service: app-sidecar     ← 完全沒動，Deployment 這次根本沒被 apply 過
```

---

## 7.7 驗證：order-service 有新屬性，payment-service 完全沒有

```bash
curl -s -X POST "http://localhost:5000/pay?item=verify&amount=3" >/dev/null; sleep 14
```

**(1) order-service 的 HTTP server span 應該多了 `team`/`cost_center`/`order.endpoint_group`：**

```bash
gwlogs 20s | grep -A8 'Name +: POST /orders' | grep -E 'team:|cost_center:|order.endpoint_group:'
# 預期看到三個屬性都出現
```

**(2) order-service 的 JDBC span，`db.statement` 應該被遮罩：**

```bash
gwlogs 20s | grep -A10 'Name +: INSERT labdb' | grep 'db.statement'
# 預期看到 ...VALUES (***redacted***)... 而不是原始的 VALUES(?, ?, ?) 或明碼參數值
```

**(3) payment-service 自己的 span（`POST /pay`）不應該出現 `team` 這個屬性：**

```bash
gwlogs 20s | grep -A12 'Name +: POST /pay' | grep -c 'team:'
# 預期：0 ← 證明 payment-service 完全沒被波及
```

三個結果合起來，就是這一階段要證明的事：**業務單位的客製化只發生在它自己的 sidecar CR 上，其他服務、Instrumentation CR、agent、gateway 全部維持原樣。**

---

## 7.8 附錄：`attributes` vs `transform`(OTTL) 怎麼選

| 需求 | `attributes` processor | `transform`（OTTL） |
|---|---|---|
| 固定值插入/覆蓋一個 key | 適合，設定最簡單 | 可以做，但殺雞用牛刀 |
| 依 span name / 既有屬性值做條件判斷 | 只能用 `include`/`exclude` 做整條 pipeline 的粗粒度篩選 | `where` 子句可以精準到每一條 statement |
| 字串處理（regex 取代、遮罩、截斷） | 不支援 | `replace_pattern`、`truncate_all` 等函式 |
| 跨欄位運算（用既有屬性算出新屬性） | 不支援 | 支援 |
| 上手難度 | 低，declarative | 較高，需要熟悉 OTTL 語法與 `context`/`error_mode` |

**經驗法則：** 能用 `attributes` 解決的靜態需求就別上 OTTL；一旦牽涉條件判斷、字串處理、資料遮罩，才需要 `transform`。兩者也可以在同一個 pipeline 裡混用（就像 7.6 的 `order-sidecar` 那樣），不是二選一。

---

## 練習 7

**動手題：** 把 7.5 的 `replace_pattern` statement 改成用 `delete_key(attributes, "db.statement")` 整個刪掉這個屬性（而不是遮罩內容），重新 apply，觀察 gateway log 裡 JDBC span 是否真的沒有 `db.statement` 這個 key。

**閱讀理解題：** 如果今天不是「order team 想加屬性」，而是「三個業務單位都想在自己的 sidecar 加不同的屬性」，你會怎麼設計 CR 的數量與命名？如果十個業務單位裡有六個其實想要一模一樣的屬性，你的設計會怎麼調整？

<details>
<summary>參考答案</summary>

**`delete_key`：** OTTL 內建 `delete_key(target_map, key)` 函式，statement 會變成：
```yaml
- delete_key(attributes, "db.statement") where attributes["db.statement"] != nil
```
套用後 `gwlogs | grep -A10 'INSERT labdb' | grep 'db.statement'` 應該完全沒有輸出——整個屬性從 span 上消失，而不是內容被換掉。差別在於：遮罩（`replace_pattern`）保留了「這裡曾經有 SQL 語句」的痕跡，方便除錯；整個刪除則是更保守的資料最小化做法，適合合規要求更嚴格的場景。

**多業務單位的 CR 設計：** 每個業務單位一個獨立的 sidecar CR（例如 `order-sidecar`、`billing-sidecar`、`shipping-sidecar`），對應各自的 annotation 值——這是本階段示範的模式，優點是**改一個 CR 絕對不會波及別人**，缺點是重複的 `receivers`/`exporters` 設定要複製 N 份，改共用邏輯（例如 exporter 端點）要改 N 處。

**六個相同需求的調整：** 拆成兩層——一個共用的 `shared-team-sidecar` CR 給那六個業務單位用（就像 Stage 3/4 現在共用 `app-sidecar` 一樣），另外三個需求不同的維持各自獨立的 CR。取捨標準是「這組業務單位的客製化需求未來會不會分岔」：如果會分岔（各自有各自的合規/計費規則），現在就分開比較省事；如果六個是同一個團隊、需求本來就該綁在一起變動，共用一份 CR 才是對的粒度——這也是 CR 設計時「共用範本 vs 各自隔離」的核心權衡，Stage 3 §3.3 提過「50 個服務共用同一套設定」的好處，這裡是它的反面：**共用範圍要對齊「誰的變更誰負責」的組織邊界，不是共用越多越好。**
</details>

---

| | |
|---|---|
| 上一步 | [← Stage 6](./06-opamp-remote-version-upgrade.md) |
| 下一步 | [Stage 8：接上 Grafana + Tempo + Prometheus →](./08-observability-backends.md) |
| 回到 | [README](./README.md) |
