# ============================================================
# Stage 1: composer 依賴
# composer 官方 image 裡沒有 opentelemetry extension，而所有
# opentelemetry-auto-* 套件都宣告 require ext-opentelemetry，
# 所以這層用 --ignore-platform-req 跳過檢查（runtime 層才真的裝）。
# ============================================================
FROM composer:2 AS vendor
WORKDIR /app
COPY composer.json composer.lock ./
RUN composer install --no-dev --prefer-dist --no-scripts --no-autoloader \
      --ignore-platform-req=ext-opentelemetry
COPY . .
# --no-scripts：post-autoload-dump 會載入 auto-guzzle 的 _register.php，
# 在沒有 extension 的 composer image 裡會炸；Laravel 的 package:discover 留到 Stage 2
RUN composer dump-autoload --optimize --no-dev --no-scripts \
      --ignore-platform-req=ext-opentelemetry

# ============================================================
# Stage 2: runtime（PHP-FPM）
# auto-pdo 要求 PHP >= 8.2、Laravel 12 skeleton 要求 >= 8.3；
# 用 8.4 是因為 composer stage（PHP 8.4）解析出的 lock 檔要求 >= 8.4.1，
# runtime 版本必須 >= composer 解析依賴時用的版本
# ============================================================
FROM php:8.4-fpm-alpine

# install-php-extensions 會自動處理 alpine 的編譯依賴（比裸 pecl 乾淨）
#   opentelemetry —— zero-code 的地基：Zend engine 層的 hook extension
#   protobuf      —— OTLP 序列化的 C 實作（不裝會 fallback 到純 PHP，較慢）
#   pdo_mysql     —— MySQL 驅動（auto-pdo hook 的就是 PDO 這層）
ADD https://github.com/mlocati/docker-php-extension-installer/releases/latest/download/install-php-extensions \
    /usr/local/bin/install-php-extensions
RUN chmod +x /usr/local/bin/install-php-extensions \
 && install-php-extensions opentelemetry protobuf pdo_mysql opcache

WORKDIR /var/www/html
COPY --from=vendor /app /var/www/html

# package:discover 是 composer post-script，Stage 1 用 --no-scripts 跳過了，
# 補在這裡跑（此時 extension 已存在，不會再炸）
# 先清掉本機開發時（含 dev 套件）產生的 bootstrap cache，
# 否則 packages.php 會引用 --no-dev vendor 裡不存在的 dev provider（如 laravel/pail）
RUN rm -f bootstrap/cache/*.php \
 && php artisan package:discover --ansi \
 && chown -R www-data:www-data storage bootstrap/cache

# 啟用 auto-instrumentation。只設這一個開關 ——
# endpoint / service.name / resource attributes / propagators
# 全部留白，交給 Operator 的 inject-sdk annotation 在 Pod 建立時注入。
ENV OTEL_PHP_AUTOLOAD_ENABLED=true

EXPOSE 9000
CMD ["php-fpm"]
