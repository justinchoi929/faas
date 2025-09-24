# 2025 BYR Team 后端考核——FaaS

## 功能测试

`/api/deploy/funcname`：

请求：

```shell
curl -X POST "http://your-host:8081/api/deploy/test" \
  -H "Content-Type: application/json" \
  -d '{
    "runtime": "js",
    "code": "addEventListener(\"fetch\", event => { event.respondWith(new Response(\"Test Success\")) })"
  }'
```

返回：

```json
{
    "accessUrl": "http://test.func.local",
    "funcName": "test",
    "status": "success",
    "subdomain": "test.func.local",
    "version": ""
}
```

请求：

```sh
curl http://test.func.local
```

返回：

```
Test Success
```

## 问题

## 优缺点分析

## 心路历程

## 人工智能生成部分
