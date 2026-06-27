package io.otel.lab.order;

import jakarta.persistence.Entity;
import jakarta.persistence.GeneratedValue;
import jakarta.persistence.GenerationType;
import jakarta.persistence.Id;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.data.jpa.repository.JpaRepository;
import org.springframework.web.bind.annotation.GetMapping;
import org.springframework.web.bind.annotation.PostMapping;
import org.springframework.web.bind.annotation.RequestParam;
import org.springframework.web.bind.annotation.RestController;

import java.time.Instant;
import java.util.List;

/**
 * 最小可跑的訂單服務。
 *
 * 重點：這份程式碼「完全沒有」OpenTelemetry 的痕跡。
 * - 沒有 import io.opentelemetry.*
 * - 沒有手動建立 Tracer / Span
 *
 * Stage 3 會由 Operator 在 Pod 啟動時注入 Java agent（opentelemetry-javaagent.jar），
 * agent 會自動為 Spring MVC、JDBC（PostgreSQL）等產生 span，app 不需任何改動。
 */
@SpringBootApplication
public class OrderServiceApplication {

    public static void main(String[] args) {
        SpringApplication.run(OrderServiceApplication.class, args);
    }
}

@Entity
class OrderRecord {
    @Id
    @GeneratedValue(strategy = GenerationType.IDENTITY)
    Long id;

    String item;
    Long amount;
    Instant createdAt;

    OrderRecord() {
    }

    OrderRecord(String item, Long amount) {
        this.item = item;
        this.amount = amount;
        this.createdAt = Instant.now();
    }

    // getter 是必要的：Jackson 靠 getter 序列化回應，否則 /orders 會回傳空物件 {}
    public Long getId() {
        return id;
    }

    public String getItem() {
        return item;
    }

    public Long getAmount() {
        return amount;
    }

    public Instant getCreatedAt() {
        return createdAt;
    }
}

interface OrderRepository extends JpaRepository<OrderRecord, Long> {
}

@RestController
class OrderController {

    private final OrderRepository repository;

    OrderController(OrderRepository repository) {
        this.repository = repository;
    }

    /** 建立一筆訂單（會寫進 PostgreSQL，agent 會自動產生一個 JDBC span）。 */
    @PostMapping("/orders")
    OrderRecord create(@RequestParam(defaultValue = "widget") String item,
                       @RequestParam(defaultValue = "1") Long amount) {
        return repository.save(new OrderRecord(item, amount));
    }

    /** 列出所有訂單。 */
    @GetMapping("/orders")
    List<OrderRecord> list() {
        return repository.findAll();
    }
}
