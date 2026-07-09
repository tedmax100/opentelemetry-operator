<?php

use Illuminate\Support\Facades\DB;
use Illuminate\Support\Facades\Http;
use Illuminate\Support\Facades\Log;
use Illuminate\Support\Facades\Route;

Route::get('/', function () {
    return view('welcome');
});

// 出貨 API：呼叫 payment-service 完成付款，再寫一筆出貨紀錄到 MySQL。
// 這條 route 上沒有任何 OTel 程式碼 —— trace/span 全部來自 zero-code instrumentation：
//   - 進站 HTTP span：opentelemetry-auto-laravel（RequestWatcher）
//   - Http::post 出站 span + traceparent header：auto-guzzle / auto-psr18
//     （Laravel HTTP client 底層就是 Guzzle）
//   - SQL span：auto-pdo + auto-laravel 的 QueryWatcher
//   - Log::info → OTLP logs：auto-laravel 的 LogWatcher
Route::post('/ship', function () {
    $item = request()->query('item', 'widget');
    $qty  = max(1, (int) request()->query('qty', '1'));

    // 1) 先請 payment-service 收款（它會再往下呼叫 order-service → 三服務串成一條 trace）
    $payment = Http::timeout(5)
        ->post(env('PAYMENT_SERVICE_URL', 'http://payment-service:5000')
            . '/pay?item=' . urlencode($item) . '&amount=' . $qty)
        ->json();

    // 2) 寫出貨紀錄（lab 風格：表不存在就建，不跑 migration）
    DB::statement(
        'CREATE TABLE IF NOT EXISTS shipments ('
        . 'id BIGINT AUTO_INCREMENT PRIMARY KEY, '
        . 'item VARCHAR(64), qty INT, payment_id BIGINT)'
    );
    DB::insert(
        'INSERT INTO shipments (item, qty, payment_id) VALUES (?, ?, ?)',
        [$item, $qty, $payment['payment_id'] ?? null]
    );
    $shipmentId = DB::getPdo()->lastInsertId();

    Log::info(sprintf(
        'shipment created: id=%s item=%s qty=%d payment_id=%s',
        $shipmentId, $item, $qty, $payment['payment_id'] ?? 'null'
    ));

    return response()->json([
        'shipment_id' => $shipmentId,
        'payment'     => $payment,
    ]);
});
