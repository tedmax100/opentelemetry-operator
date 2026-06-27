"""
payment-service —— Python / Flask 服務（情境中「已手動裝好 OTel」的那個）。

這個服務示範三種「應用程式自己產生的遙測」：
  1. trace      —— 由 instrumentation library（flask/requests/psycopg2）自動產生
  2. 自訂 metrics —— 業務指標（付款次數、付款金額分佈），由 app code 主動寫
  3. 自訂 log    —— 結構化日誌，由 app code 主動寫

★ 關鍵設計：自訂 metrics / log 一律透過「global API」取得 meter / logger，
  而不是自己 new 一個 MeterProvider/LoggerProvider 綁死。
  這樣同一份業務 code 在兩種模式下都能運作：
    - Stage 1（MANUAL_OTEL=true）：_init_manual_otel() 提供 SDK provider
    - Stage 4（auto-instrument）  ：Operator 注入的 agent 提供 SDK provider
  → 遷移時只要拿掉「手動 provider 設定」，業務遙測（counter/histogram/log）原封不動。

流程：client → payment-service(本服務) → order-service(Java) → PostgreSQL
"""
import logging
import os

import psycopg2
import requests
from flask import Flask, jsonify, request

# 只 import「API」層（global registry）。SDK 由手動 init 或 Operator agent 提供。
from opentelemetry import metrics

app = Flask(__name__)


def _init_manual_otel(flask_app):
    """Stage 1 的手動 OTel SDK 初始化：trace + metrics + logs 三條都自己接。

    Stage 4 遷移後不再執行（改由 Operator 注入的 agent 提供 SDK provider）。
    """
    from opentelemetry import trace
    from opentelemetry._logs import set_logger_provider
    from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter
    from opentelemetry.exporter.otlp.proto.grpc.metric_exporter import OTLPMetricExporter
    from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
    from opentelemetry.instrumentation.flask import FlaskInstrumentor
    from opentelemetry.instrumentation.psycopg2 import Psycopg2Instrumentor
    from opentelemetry.instrumentation.requests import RequestsInstrumentor
    from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
    from opentelemetry.sdk.resources import Resource
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    endpoint = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317")
    resource = Resource.create(
        {"service.name": os.getenv("OTEL_SERVICE_NAME", "payment-service")}
    )

    # --- traces ---
    tp = TracerProvider(resource=resource)
    tp.add_span_processor(BatchSpanProcessor(OTLPSpanExporter(endpoint=endpoint, insecure=True)))
    trace.set_tracer_provider(tp)

    # --- metrics ---
    # export_interval 預設 60s，lab 為了快點看到自訂 metrics 調成 5s
    reader = PeriodicExportingMetricReader(
        OTLPMetricExporter(endpoint=endpoint, insecure=True),
        export_interval_millis=int(os.getenv("OTEL_METRIC_EXPORT_INTERVAL", "5000")),
    )
    metrics.set_meter_provider(MeterProvider(resource=resource, metric_readers=[reader]))

    # --- logs ---
    lp = LoggerProvider(resource=resource)
    lp.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter(endpoint=endpoint, insecure=True)))
    set_logger_provider(lp)
    logging.getLogger().addHandler(LoggingHandler(logger_provider=lp))

    # 自動 instrument 各 library（Stage 4 後改由 auto-instrument 完成）
    FlaskInstrumentor().instrument_app(flask_app)
    RequestsInstrumentor().instrument()
    Psycopg2Instrumentor().instrument()


if os.getenv("MANUAL_OTEL", "false").lower() == "true":
    _init_manual_otel(app)

# ---------------------------------------------------------------------------
# 自訂業務遙測：一律走 global API（兩種模式皆適用）
# ---------------------------------------------------------------------------
# 設定 log level。注意：不能只靠 logging.basicConfig()——手動模式下 _init_manual_otel
# 已經幫 root 加了 OTLP handler，basicConfig 會因「root 已有 handler」而整個跳過，
# 導致 root level 停在預設的 WARNING，INFO 等級的自訂 log 會在送到 handler 前就被濾掉。
_root = logging.getLogger()
_root.setLevel(logging.INFO)
if not _root.handlers:                          # 沒有 handler 時補一個 stdout，方便觀察
    _root.addHandler(logging.StreamHandler())
log = logging.getLogger("payment-service")
log.setLevel(logging.INFO)

meter = metrics.get_meter("payment-service")   # global MeterProvider（手動或 agent 提供）
payments_counter = meter.create_counter(
    "payments.count", unit="1", description="處理過的付款筆數"
)
payment_amount = meter.create_histogram(
    "payments.amount", unit="1", description="每筆付款金額分佈"
)

ORDER_SERVICE_URL = os.getenv("ORDER_SERVICE_URL", "http://order-service:8080")
DB_DSN = os.getenv(
    "DB_DSN",
    "host=postgres port=5432 dbname=labdb user=lab password=labpass",
)


@app.route("/health")
def health():
    return jsonify(status="ok")


@app.route("/pay", methods=["POST"])
def pay():
    item = request.args.get("item", "widget")
    amount = int(request.args.get("amount", "1"))

    # 1) 呼叫下游 Java 服務建立訂單（跨服務 trace 的關鍵：context 透過 HTTP header 傳遞）
    resp = requests.post(
        f"{ORDER_SERVICE_URL}/orders",
        params={"item": item, "amount": amount},
        timeout=5,
    )
    order = resp.json()

    # 2) 自己也寫一筆付款紀錄到 PostgreSQL
    conn = psycopg2.connect(DB_DSN)
    with conn, conn.cursor() as cur:
        cur.execute(
            "CREATE TABLE IF NOT EXISTS payments "
            "(id SERIAL PRIMARY KEY, order_id BIGINT, amount BIGINT)"
        )
        cur.execute(
            "INSERT INTO payments (order_id, amount) VALUES (%s, %s) RETURNING id",
            (order.get("id"), amount),
        )
        payment_id = cur.fetchone()[0]
    conn.close()

    # 3) 自訂 metrics + 自訂 log（業務語意，instrumentation library 不會幫你產生）
    payments_counter.add(1, {"item": item})
    payment_amount.record(amount, {"item": item})
    log.info("payment processed: id=%s item=%s amount=%s", payment_id, item, amount)

    return jsonify(payment_id=payment_id, order=order)


if __name__ == "__main__":
    app.run(host="0.0.0.0", port=5000)
