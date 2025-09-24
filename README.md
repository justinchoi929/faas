# 2025 BYR Team 后端考核——FaaS

## 功能测试

### 部署函数

`/api/deploy/funcname`：

请求：

```shell
curl -X POST "http://your-host:8081/api/deploy/hello" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", event => { event.respondWith(new Response(\"v1\")) })",
    "version": "v1"
  }'
```

返回：

```json
{
    "accessUrl": "http://v1.hello.func.local",
    "alias": "",
    "funcName": "hello",
    "status": "success",
    "subdomain": "v1.hello.func.local",
    "version": "v1"
}
```

请求：

```sh
curl http://v1.hello.func.local
```

返回：

```
v1
```

### 别名测试

请求：

```sh
curl -X POST "http://your-host:8081/api/deploy/hello" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", event => { event.respondWith(new Response(\"v2\")) })",
    "version": "v2",
    "alias": "test"
  }'
```

返回：

```json
{
    "accessUrl": "http://v2.hello.func.local",
    "alias": "test",
    "funcName": "hello",
    "status": "success",
    "subdomain": "v2.hello.func.local",
    "version": "v2"
}
```

请求：

```sh
curl http://test.hello.func.local
```

返回：

```
v2
```

请求：

```sh
curl http://hello.func.local
```

返回：

```
v2
```

### 回退测试

请求：

```sh
curl -X POST "http://your-host:8081/api/rollback/hello" \
  -H "Content-Type: application/json" \
  -d '{
    "version": "v1"
  }'
```

返回：

```json
{
    "accessUrl": "http://v1.hello.func.local",
    "alias": "",
    "funcName": "hello",
    "status": "success",
    "targetVersion": "v1"
}
```

请求：

```sh
curl http://hello.func.local
```

返回：

```
v1
```

## 问题

## 优缺点分析

## 心路历程

## 人工智能生成部分
