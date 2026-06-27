FROM python:3.12-slim

WORKDIR /app

# psycopg2-binary 不需要編譯，但保留 curl 方便在容器內 debug
RUN apt-get update && apt-get install -y --no-install-recommends curl \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py .

EXPOSE 5000
# Stage 1 用這個 image（內含手動 OTel SDK）。
# Stage 4 會改用同一個 image，但把手動初始化拿掉、改靠 Operator 注入。
CMD ["python", "app.py"]
