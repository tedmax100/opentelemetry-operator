# ---- build stage ----
FROM maven:3.9-eclipse-temurin-17 AS build
WORKDIR /workspace

# 先複製 pom.xml 下載相依，利用 layer cache
COPY pom.xml .
RUN mvn -B -q dependency:go-offline

COPY src ./src
RUN mvn -B -q clean package -DskipTests

# ---- runtime stage ----
FROM eclipse-temurin:17-jre
WORKDIR /app
COPY --from=build /workspace/target/order-service.jar /app/app.jar

EXPOSE 8080
# 注意：這裡用最單純的 java -jar 啟動，沒有掛任何 -javaagent。
# Stage 3 由 Operator 注入 agent 時，會自動在 JAVA_TOOL_OPTIONS 加上 -javaagent。
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
